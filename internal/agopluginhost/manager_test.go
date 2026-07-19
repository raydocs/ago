package agopluginhost

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"claudexflow/internal/agopluginprotocol"
)

func TestReloadInitializesNextGenerationBeforeReplacingCurrent(t *testing.T) {
	events := []string{}
	first := &fakeRuntime{name: "g1", events: &events}
	second := &fakeRuntime{name: "g2", events: &events}
	manager := NewManager(&fakeFactory{runtimes: []*fakeRuntime{first, second}}, time.Second)

	firstSnapshot, err := manager.Reload(context.Background(), ReloadConfig{}, "startup")
	if err != nil {
		t.Fatalf("first Reload() error = %v", err)
	}
	if firstSnapshot.Generation != 1 || manager.Current().Generation != 1 {
		t.Fatalf("first snapshot = %#v, current = %#v", firstSnapshot, manager.Current())
	}
	secondSnapshot, err := manager.Reload(context.Background(), ReloadConfig{}, "reload")
	if err != nil {
		t.Fatalf("second Reload() error = %v", err)
	}
	if secondSnapshot.Generation != 2 || manager.Current().Generation != 2 {
		t.Fatalf("second snapshot = %#v, current = %#v", secondSnapshot, manager.Current())
	}
	want := []string{"start:g1:1", "initialize:g1:1", "start:g2:2", "initialize:g2:2", "cancel:g1:reload", "dispose:g1:reload", "terminate:g1"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("lifecycle = %#v, want %#v", events, want)
	}
}

func TestFailedReloadKeepsCurrentGenerationAndTerminatesCandidate(t *testing.T) {
	events := []string{}
	first := &fakeRuntime{name: "g1", events: &events}
	failed := &fakeRuntime{name: "g2", events: &events, initializeErr: errors.New("activation failed")}
	manager := NewManager(&fakeFactory{runtimes: []*fakeRuntime{first, failed}}, time.Second)
	if _, err := manager.Reload(context.Background(), ReloadConfig{}, "startup"); err != nil {
		t.Fatal(err)
	}

	if _, err := manager.Reload(context.Background(), ReloadConfig{}, "reload"); err == nil {
		t.Fatal("failed candidate reload succeeded")
	}
	if manager.Current().Generation != 1 {
		t.Fatalf("current generation = %d, want 1", manager.Current().Generation)
	}
	want := []string{"start:g1:1", "initialize:g1:1", "start:g2:2", "initialize:g2:2", "terminate:g2"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("lifecycle = %#v, want %#v", events, want)
	}
}

func TestReloadRejectsMismatchedGenerationAndMissingDefaultPolicy(t *testing.T) {
	for _, test := range []struct {
		name    string
		runtime *fakeRuntime
	}{
		{name: "stale generation", runtime: &fakeRuntime{resultGeneration: 99}},
		{name: "missing default policy", runtime: &fakeRuntime{registrations: []agopluginprotocol.PluginRegistration{{PluginID: "workspace.plugin"}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			events := []string{}
			test.runtime.name = "candidate"
			test.runtime.events = &events
			manager := NewManager(&fakeFactory{runtimes: []*fakeRuntime{test.runtime}}, time.Second)
			if _, err := manager.Reload(context.Background(), ReloadConfig{}, "startup"); err == nil {
				t.Fatal("invalid generation was admitted")
			}
			if manager.Current().Generation != 0 {
				t.Fatalf("invalid generation became current: %#v", manager.Current())
			}
			if events[len(events)-1] != "terminate:candidate" {
				t.Fatalf("candidate was not terminated: %#v", events)
			}
		})
	}
}

func TestShutdownCancelsDisposesAndTerminatesCurrentGeneration(t *testing.T) {
	events := []string{}
	runtime := &fakeRuntime{name: "g1", events: &events}
	manager := NewManager(&fakeFactory{runtimes: []*fakeRuntime{runtime}}, time.Second)
	if _, err := manager.Reload(context.Background(), ReloadConfig{}, "startup"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if manager.Current().Generation != 0 {
		t.Fatalf("current after shutdown = %#v", manager.Current())
	}
	want := []string{"start:g1:1", "initialize:g1:1", "cancel:g1:shutdown", "dispose:g1:shutdown", "terminate:g1"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("lifecycle = %#v, want %#v", events, want)
	}
}

type fakeFactory struct {
	runtimes []*fakeRuntime
	next     int
}

func (factory *fakeFactory) Start(_ context.Context, generation int64) (Runtime, error) {
	runtime := factory.runtimes[factory.next]
	factory.next++
	*runtime.events = append(*runtime.events, "start:"+runtime.name+":"+itoa(generation))
	return runtime, nil
}

type fakeRuntime struct {
	name             string
	events           *[]string
	initializeErr    error
	resultGeneration int64
	registrations    []agopluginprotocol.PluginRegistration
}

func (runtime *fakeRuntime) Initialize(_ context.Context, params agopluginprotocol.InitializeParams) (agopluginprotocol.InitializeResult, error) {
	*runtime.events = append(*runtime.events, "initialize:"+runtime.name+":"+itoa(params.Generation))
	if runtime.initializeErr != nil {
		return agopluginprotocol.InitializeResult{}, runtime.initializeErr
	}
	generation := runtime.resultGeneration
	if generation == 0 {
		generation = params.Generation
	}
	registrations := runtime.registrations
	if registrations == nil {
		registrations = []agopluginprotocol.PluginRegistration{{PluginID: DefaultPermissionPluginID, Hooks: []string{"tool.call"}}}
	}
	return agopluginprotocol.InitializeResult{ProtocolVersion: 1, Generation: generation, Plugins: registrations}, nil
}

func (runtime *fakeRuntime) Invoke(context.Context, string, agopluginprotocol.InvocationParams) (json.RawMessage, error) {
	return json.RawMessage(`null`), nil
}

func (runtime *fakeRuntime) CancelAll(reason string) {
	*runtime.events = append(*runtime.events, "cancel:"+runtime.name+":"+reason)
}
func (runtime *fakeRuntime) Dispose(_ context.Context, reason string) error {
	*runtime.events = append(*runtime.events, "dispose:"+runtime.name+":"+reason)
	return nil
}
func (runtime *fakeRuntime) Terminate() error {
	*runtime.events = append(*runtime.events, "terminate:"+runtime.name)
	return nil
}

func itoa(value int64) string {
	if value == 1 {
		return "1"
	}
	if value == 2 {
		return "2"
	}
	if value == 99 {
		return "99"
	}
	return "0"
}
