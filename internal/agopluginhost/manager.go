package agopluginhost

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"claudexflow/internal/agopluginprotocol"
)

const DefaultPermissionPluginID = "ago.permission.default"

type Runtime interface {
	Initialize(context.Context, agopluginprotocol.InitializeParams) (agopluginprotocol.InitializeResult, error)
	Invoke(context.Context, string, agopluginprotocol.InvocationParams) (json.RawMessage, error)
	CancelAll(reason string)
	Dispose(context.Context, string) error
	Terminate() error
}

type Factory interface {
	Start(context.Context, int64) (Runtime, error)
}

type ReloadConfig struct {
	WorkspaceURI *string
	Plugins      []agopluginprotocol.PluginConfig
	Capabilities agopluginprotocol.Capabilities
	Limits       agopluginprotocol.Limits
}

type Snapshot struct {
	Generation    int64
	Registrations []agopluginprotocol.PluginRegistration
}

type activeGeneration struct {
	snapshot Snapshot
	runtime  Runtime
}

type Manager struct {
	factory        Factory
	cleanupTimeout time.Duration

	reloadMu    sync.Mutex
	mu          sync.RWMutex
	next        int64
	current     activeGeneration
	config      ReloadConfig
	invocations map[string]InvocationContext
	retire      func(generation int64, reason string)
}

type InvocationContext struct {
	ThreadID string
	TurnID   string
}

func NewManager(factory Factory, cleanupTimeout time.Duration) *Manager {
	if cleanupTimeout <= 0 {
		cleanupTimeout = 2 * time.Second
	}
	return &Manager{factory: factory, cleanupTimeout: cleanupTimeout, next: 1, invocations: make(map[string]InvocationContext)}
}

func (manager *Manager) Invocation(invocationID string) (InvocationContext, bool) {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	value, found := manager.invocations[invocationID]
	return value, found
}

func (manager *Manager) SetGenerationRetirer(retire func(generation int64, reason string)) {
	manager.mu.Lock()
	manager.retire = retire
	manager.mu.Unlock()
}

func (manager *Manager) trackInvocation(invocationID string, value InvocationContext) func() {
	if value.ThreadID == "" || value.TurnID == "" {
		return func() {}
	}
	manager.mu.Lock()
	manager.invocations[invocationID] = value
	manager.mu.Unlock()
	return func() {
		manager.mu.Lock()
		delete(manager.invocations, invocationID)
		manager.mu.Unlock()
	}
}

func (manager *Manager) Current() Snapshot {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	return cloneSnapshot(manager.current.snapshot)
}

func (manager *Manager) Reload(ctx context.Context, config ReloadConfig, reason string) (Snapshot, error) {
	manager.reloadMu.Lock()
	defer manager.reloadMu.Unlock()
	for _, plugin := range config.Plugins {
		if plugin.PluginID == DefaultPermissionPluginID {
			return Snapshot{}, fmt.Errorf("workspace plugins cannot replace %s", DefaultPermissionPluginID)
		}
	}

	manager.mu.Lock()
	generation := manager.next
	manager.next++
	manager.mu.Unlock()
	candidate, err := manager.factory.Start(ctx, generation)
	if err != nil {
		return Snapshot{}, fmt.Errorf("start plugin generation %d: %w", generation, err)
	}
	result, err := candidate.Initialize(ctx, agopluginprotocol.InitializeParams{
		SupportedProtocolVersions: []int{agopluginprotocol.Version1},
		Generation:                generation,
		WorkspaceURI:              config.WorkspaceURI,
		Plugins:                   config.Plugins,
		Capabilities:              config.Capabilities,
		Limits:                    config.Limits,
	})
	if err != nil {
		_ = candidate.Terminate()
		return Snapshot{}, fmt.Errorf("initialize plugin generation %d: %w", generation, err)
	}
	if err := validateInitialization(generation, result); err != nil {
		_ = candidate.Terminate()
		return Snapshot{}, err
	}
	snapshot := Snapshot{Generation: generation, Registrations: append([]agopluginprotocol.PluginRegistration(nil), result.Plugins...)}

	manager.mu.Lock()
	previous := manager.current
	manager.current = activeGeneration{snapshot: snapshot, runtime: candidate}
	manager.config = cloneReloadConfig(config)
	manager.mu.Unlock()
	if previous.runtime != nil {
		manager.cleanup(previous.snapshot.Generation, previous.runtime, reason)
	}
	return cloneSnapshot(snapshot), nil
}

func (manager *Manager) Shutdown(ctx context.Context) error {
	manager.reloadMu.Lock()
	defer manager.reloadMu.Unlock()
	manager.mu.Lock()
	current := manager.current
	manager.current = activeGeneration{}
	manager.mu.Unlock()
	if current.runtime == nil {
		return nil
	}
	current.runtime.CancelAll("shutdown")
	manager.retireGeneration(current.snapshot.Generation, "shutdown")
	err := manager.dispose(ctx, current.runtime, "shutdown")
	terminateErr := current.runtime.Terminate()
	if err != nil {
		return fmt.Errorf("dispose plugin generation %d: %w", current.snapshot.Generation, err)
	}
	if terminateErr != nil {
		return fmt.Errorf("terminate plugin generation %d: %w", current.snapshot.Generation, terminateErr)
	}
	return nil
}

func (manager *Manager) cleanup(generation int64, runtime Runtime, reason string) {
	runtime.CancelAll(reason)
	manager.retireGeneration(generation, reason)
	_ = manager.dispose(context.Background(), runtime, reason)
	_ = runtime.Terminate()
}

func (manager *Manager) retireGeneration(generation int64, reason string) {
	manager.mu.RLock()
	retire := manager.retire
	manager.mu.RUnlock()
	if retire != nil {
		retire(generation, reason)
	}
}

func (manager *Manager) dispose(parent context.Context, runtime Runtime, reason string) error {
	ctx, cancel := context.WithTimeout(parent, manager.cleanupTimeout)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runtime.Dispose(ctx, reason) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func validateInitialization(generation int64, result agopluginprotocol.InitializeResult) error {
	if result.ProtocolVersion != agopluginprotocol.Version1 {
		return fmt.Errorf("plugin generation %d selected unsupported protocol version %d", generation, result.ProtocolVersion)
	}
	if result.Generation != generation {
		return fmt.Errorf("plugin generation %d returned stale generation %d", generation, result.Generation)
	}
	defaultIndex := -1
	seen := make(map[string]struct{}, len(result.Plugins))
	toolNames := make(map[string]string)
	commandIDs := make(map[string]string)
	for index, plugin := range result.Plugins {
		if plugin.PluginID == "" {
			return fmt.Errorf("plugin generation %d returned an empty plugin ID", generation)
		}
		if _, exists := seen[plugin.PluginID]; exists {
			return fmt.Errorf("plugin generation %d returned duplicate plugin %q", generation, plugin.PluginID)
		}
		seen[plugin.PluginID] = struct{}{}
		if plugin.PluginID == DefaultPermissionPluginID {
			defaultIndex = index
		}
		for _, tool := range plugin.Tools {
			if owner, exists := toolNames[tool.Name]; exists || tool.Name == "" {
				return fmt.Errorf("plugin generation %d has colliding tool %q from %q and %q", generation, tool.Name, owner, plugin.PluginID)
			}
			toolNames[tool.Name] = plugin.PluginID
		}
		for _, command := range plugin.Commands {
			canonical := plugin.PluginID + ":" + command.ID
			if owner, exists := commandIDs[canonical]; exists || command.ID == "" {
				return fmt.Errorf("plugin generation %d has colliding command %q from %q and %q", generation, canonical, owner, plugin.PluginID)
			}
			commandIDs[canonical] = plugin.PluginID
		}
	}
	if defaultIndex < 0 || defaultIndex != len(result.Plugins)-1 {
		return fmt.Errorf("plugin generation %d must register %s last", generation, DefaultPermissionPluginID)
	}
	return nil
}

func cloneSnapshot(snapshot Snapshot) Snapshot {
	snapshot.Registrations = append([]agopluginprotocol.PluginRegistration(nil), snapshot.Registrations...)
	return snapshot
}

func cloneReloadConfig(config ReloadConfig) ReloadConfig {
	config.Plugins = append([]agopluginprotocol.PluginConfig(nil), config.Plugins...)
	config.Capabilities.UI = append([]agopluginprotocol.UIKind(nil), config.Capabilities.UI...)
	return config
}
