package agopluginhost

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"claudexflow/internal/agopluginprotocol"
)

func TestDiscoverWorkspacePluginsCanonicalizesEntriesAndIsStrict(t *testing.T) {
	workspace := t.TempDir()
	ago := filepath.Join(workspace, ".ago")
	if err := os.Mkdir(ago, 0o755); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(ago, "plugin.js")
	if err := os.WriteFile(entry, []byte("// plugin"), 0o644); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(ago, "plugins.json")
	if err := os.WriteFile(config, []byte(`[{"pluginId":"demo","entryUri":"plugin.js","config":{}}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	plugins, err := discoverWorkspacePlugins(workspace)
	if err != nil {
		t.Fatalf("discoverWorkspacePlugins() error = %v", err)
	}
	canonicalEntry, err := filepath.EvalSymlinks(entry)
	if err != nil {
		t.Fatal(err)
	}
	if len(plugins) != 1 || plugins[0].EntryURI != fileURI(canonicalEntry) {
		t.Fatalf("plugins = %#v, want canonical entry %q", plugins, fileURI(canonicalEntry))
	}

	for _, invalid := range []string{
		`[{"pluginId":"demo","entryUri":"plugin.js","unknown":true}]`,
		`[{"pluginId":"demo","entryUri":"https://example.com/p.js"}]`,
		`[{"pluginId":"","entryUri":"plugin.js"}]`,
		`[] {}`,
	} {
		if err := os.WriteFile(config, []byte(invalid), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := discoverWorkspacePlugins(workspace); err == nil {
			t.Errorf("discoverWorkspacePlugins() accepted %s", invalid)
		}
	}
}

func TestWorkspaceRegistryConcurrentGetCreatesOneGeneration(t *testing.T) {
	workspace := t.TempDir()
	runtime := &workspaceRuntime{}
	var constructions atomic.Int32
	registry := NewWorkspaceRegistry(func(string) *Manager {
		constructions.Add(1)
		return NewManager(workspaceFactory{runtime: runtime}, 0)
	}, ReloadConfig{})

	const callers = 20
	results := make(chan *Manager, callers)
	errs := make(chan error, callers)
	var group sync.WaitGroup
	for range callers {
		group.Add(1)
		go func() {
			defer group.Done()
			manager, err := registry.Get(context.Background(), workspace)
			results <- manager
			errs <- err
		}()
	}
	group.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}
	}
	var first *Manager
	for manager := range results {
		if first == nil {
			first = manager
		} else if manager != first {
			t.Fatal("Get() returned different managers for one workspace")
		}
	}
	if constructions.Load() != 1 || runtime.initializations.Load() != 1 {
		t.Fatalf("constructions = %d, initializations = %d; want 1, 1", constructions.Load(), runtime.initializations.Load())
	}
	canonicalWorkspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.workspaceURI != fileURI(canonicalWorkspace) {
		t.Fatalf("WorkspaceURI = %q, want %q", runtime.workspaceURI, fileURI(canonicalWorkspace))
	}
	if err := registry.Close(context.Background()); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if runtime.terminations.Load() != 1 {
		t.Fatalf("terminations = %d, want 1", runtime.terminations.Load())
	}
}

type workspaceFactory struct{ runtime Runtime }

func (factory workspaceFactory) Start(context.Context, int64) (Runtime, error) {
	return factory.runtime, nil
}

type workspaceRuntime struct {
	initializations atomic.Int32
	terminations    atomic.Int32
	workspaceURI    string
}

func (runtime *workspaceRuntime) Initialize(_ context.Context, params agopluginprotocol.InitializeParams) (agopluginprotocol.InitializeResult, error) {
	runtime.initializations.Add(1)
	if params.WorkspaceURI != nil {
		runtime.workspaceURI = *params.WorkspaceURI
	}
	return agopluginprotocol.InitializeResult{ProtocolVersion: 1, Generation: params.Generation, Plugins: []agopluginprotocol.PluginRegistration{{PluginID: DefaultPermissionPluginID}}}, nil
}
func (*workspaceRuntime) Invoke(context.Context, string, agopluginprotocol.InvocationParams) (json.RawMessage, error) {
	return nil, nil
}
func (*workspaceRuntime) CancelAll(string)                      {}
func (*workspaceRuntime) Dispose(context.Context, string) error { return nil }
func (runtime *workspaceRuntime) Terminate() error {
	runtime.terminations.Add(1)
	return nil
}
