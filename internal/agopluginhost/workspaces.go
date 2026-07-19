package agopluginhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"claudexflow/internal/agopluginprotocol"
)

const maxWorkspacePluginConfigBytes = 1 << 20

// WorkspaceManagerFactory constructs an uninitialized Manager. The registry
// calls it at most once for each successfully initialized canonical workspace.
type WorkspaceManagerFactory func(canonicalWorkspace string) *Manager

// WorkspaceRegistry owns one lazily initialized plugin manager per canonical
// workspace directory.
type WorkspaceRegistry struct {
	mu       sync.Mutex
	factory  WorkspaceManagerFactory
	config   ReloadConfig
	managers map[string]*Manager
	closed   bool
}

func NewWorkspaceRegistry(factory WorkspaceManagerFactory, config ReloadConfig) *WorkspaceRegistry {
	return &WorkspaceRegistry{factory: factory, config: cloneReloadConfig(config), managers: make(map[string]*Manager)}
}

// Get returns the manager for workspace, creating and initializing it when
// first requested. Creation is serialized so concurrent callers cannot start
// duplicate generations for the same workspace.
func (r *WorkspaceRegistry) Get(ctx context.Context, workspace string) (*Manager, error) {
	canonical, err := canonicalDirectory(workspace)
	if err != nil {
		return nil, fmt.Errorf("canonicalize workspace: %w", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, errors.New("workspace plugin registry is shut down")
	}
	if manager := r.managers[canonical]; manager != nil {
		return manager, nil
	}
	if r.factory == nil {
		return nil, errors.New("workspace plugin registry has no manager factory")
	}
	plugins, err := discoverWorkspacePlugins(canonical)
	if err != nil {
		return nil, err
	}
	manager := r.factory(canonical)
	if manager == nil {
		return nil, errors.New("workspace plugin manager factory returned nil")
	}
	workspaceURI := fileURI(canonical)
	config := cloneReloadConfig(r.config)
	config.WorkspaceURI = &workspaceURI
	config.Plugins = append(config.Plugins, plugins...)
	if _, err := manager.Reload(ctx, config, "workspace initialization"); err != nil {
		return nil, fmt.Errorf("initialize workspace plugin manager: %w", err)
	}
	r.managers[canonical] = manager
	return manager, nil
}

// Shutdown closes all managers in canonical workspace order. It attempts every
// shutdown and returns all errors. Once called, the registry cannot be reused.
func (r *WorkspaceRegistry) Shutdown(ctx context.Context) error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	keys := make([]string, 0, len(r.managers))
	for key := range r.managers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	managers := make([]*Manager, 0, len(keys))
	for _, key := range keys {
		managers = append(managers, r.managers[key])
	}
	r.managers = nil
	r.mu.Unlock()

	var errs []error
	for index, manager := range managers {
		if err := manager.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("shutdown workspace %q: %w", keys[index], err))
		}
	}
	return errors.Join(errs...)
}

// Close is an alias for Shutdown for owners that use close terminology.
func (r *WorkspaceRegistry) Close(ctx context.Context) error { return r.Shutdown(ctx) }

func canonicalDirectory(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%q is not a directory", canonical)
	}
	return filepath.Clean(canonical), nil
}

func discoverWorkspacePlugins(workspace string) ([]agopluginprotocol.PluginConfig, error) {
	configPath := filepath.Join(workspace, ".ago", "plugins.json")
	file, err := os.Open(configPath)
	if errors.Is(err, os.ErrNotExist) {
		return []agopluginprotocol.PluginConfig{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open workspace plugin config: %w", err)
	}
	defer file.Close()

	limited := io.LimitReader(file, maxWorkspacePluginConfigBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read workspace plugin config: %w", err)
	}
	if len(data) > maxWorkspacePluginConfigBytes {
		return nil, fmt.Errorf("workspace plugin config exceeds %d bytes", maxWorkspacePluginConfigBytes)
	}
	var plugins []agopluginprotocol.PluginConfig
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&plugins); err != nil {
		return nil, fmt.Errorf("decode workspace plugin config: %w", err)
	}
	if plugins == nil {
		return nil, errors.New("workspace plugin config must be a JSON array")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, errors.New("workspace plugin config contains trailing JSON")
		}
		return nil, fmt.Errorf("decode trailing workspace plugin config: %w", err)
	}
	for index := range plugins {
		plugin := &plugins[index]
		if strings.TrimSpace(plugin.PluginID) == "" {
			return nil, fmt.Errorf("workspace plugin %d is missing pluginId", index)
		}
		if strings.TrimSpace(plugin.EntryURI) == "" {
			return nil, fmt.Errorf("workspace plugin %q is missing entryUri", plugin.PluginID)
		}
		canonical, err := canonicalEntryURI(filepath.Dir(configPath), plugin.EntryURI)
		if err != nil {
			return nil, fmt.Errorf("workspace plugin %q entryUri: %w", plugin.PluginID, err)
		}
		plugin.EntryURI = canonical
	}
	return plugins, nil
}

func canonicalEntryURI(base, entry string) (string, error) {
	parsed, err := url.Parse(entry)
	if err != nil {
		return "", err
	}
	var path string
	if parsed.Scheme == "" {
		path = entry
		if !filepath.IsAbs(path) {
			path = filepath.Join(base, path)
		}
	} else {
		if parsed.Scheme != "file" {
			return "", fmt.Errorf("scheme %q is not allowed", parsed.Scheme)
		}
		if parsed.Host != "" && parsed.Host != "localhost" {
			return "", fmt.Errorf("file URI host %q is not local", parsed.Host)
		}
		if parsed.RawQuery != "" || parsed.Fragment != "" || !filepath.IsAbs(parsed.Path) {
			return "", errors.New("file URI must contain only an absolute path")
		}
		path = filepath.FromSlash(parsed.Path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("canonicalize: %w", err)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%q is not a regular file", canonical)
	}
	return fileURI(canonical), nil
}

func fileURI(path string) string {
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String()
}
