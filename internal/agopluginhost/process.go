package agopluginhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"claudexflow/internal/agopluginprotocol"
)

type ProcessOptions struct {
	MaxMessageBytes int
	ExitGrace       time.Duration
	UI              func(context.Context, agopluginprotocol.UIRequestParams) agopluginprotocol.UIResult
	Log             func(agopluginprotocol.LogParams)
	AIAsk           func(context.Context, agopluginprotocol.AIAskParams) (agopluginprotocol.AIAskResult, error)
}

type ProcessFactory struct {
	bun     string
	script  string
	options ProcessOptions
}

func NewProcessFactory(bun, script string, options ProcessOptions) *ProcessFactory {
	if options.MaxMessageBytes <= 0 {
		options.MaxMessageBytes = agopluginprotocol.DefaultMaxFrameBytes
	}
	if options.ExitGrace <= 0 {
		options.ExitGrace = 5 * time.Second
	}
	return &ProcessFactory{bun: bun, script: script, options: options}
}

func (factory *ProcessFactory) Start(_ context.Context, generation int64) (Runtime, error) {
	command := exec.Command(factory.bun, factory.script)
	command.Env = os.Environ()
	command.Stderr = os.Stderr
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("open plugin child stdin: %w", err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("open plugin child stdout: %w", err)
	}
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start plugin child: %w", err)
	}
	runtime := &ProcessRuntime{
		generation: generation,
		command:    command,
		stdin:      stdin,
		decoder:    agopluginprotocol.NewDecoder(stdout, factory.options.MaxMessageBytes),
		options:    factory.options,
		pending:    make(map[string]chan agopluginprotocol.Envelope),
		active:     make(map[string]activeInvocation),
		plugins:    make(map[string]struct{}),
		exited:     make(chan struct{}),
	}
	runtime.retirement, runtime.retire = context.WithCancel(context.Background())
	go runtime.readLoop()
	go func() {
		runtime.finish(command.Wait())
	}()
	return runtime, nil
}

type ProcessRuntime struct {
	generation int64
	command    *exec.Cmd
	stdin      io.WriteCloser
	decoder    *agopluginprotocol.Decoder
	options    ProcessOptions

	writeMu    sync.Mutex
	mu         sync.Mutex
	pending    map[string]chan agopluginprotocol.Envelope
	active     map[string]activeInvocation
	plugins    map[string]struct{}
	retirement context.Context
	retire     context.CancelFunc
	exitErr    error
	exited     chan struct{}
	exitOnce   sync.Once
	requestID  atomic.Uint64
}

type activeInvocation struct {
	ctx    context.Context
	cancel context.CancelFunc
	params agopluginprotocol.InvocationParams
}

func (runtime *ProcessRuntime) Initialize(ctx context.Context, params agopluginprotocol.InitializeParams) (agopluginprotocol.InitializeResult, error) {
	raw, err := runtime.call(ctx, agopluginprotocol.MethodInitialize, params)
	if err != nil {
		return agopluginprotocol.InitializeResult{}, err
	}
	var result agopluginprotocol.InitializeResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return agopluginprotocol.InitializeResult{}, fmt.Errorf("decode plugin initialization: %w", err)
	}
	runtime.mu.Lock()
	for _, plugin := range result.Plugins {
		runtime.plugins[plugin.PluginID] = struct{}{}
	}
	runtime.mu.Unlock()
	return result, nil
}

func (runtime *ProcessRuntime) Invoke(ctx context.Context, method string, params agopluginprotocol.InvocationParams) (json.RawMessage, error) {
	invocationCtx, cancel := context.WithCancel(ctx)
	runtime.mu.Lock()
	if _, exists := runtime.active[params.InvocationID]; exists {
		runtime.mu.Unlock()
		cancel()
		return nil, fmt.Errorf("plugin invocation %q is already active", params.InvocationID)
	}
	runtime.active[params.InvocationID] = activeInvocation{ctx: invocationCtx, cancel: cancel, params: params}
	runtime.mu.Unlock()
	defer func() {
		cancel()
		runtime.mu.Lock()
		delete(runtime.active, params.InvocationID)
		runtime.mu.Unlock()
	}()
	raw, err := runtime.call(invocationCtx, method, params)
	if ctx.Err() != nil {
		_ = runtime.send(agopluginprotocol.Envelope{Type: agopluginprotocol.MessageNotification, Method: agopluginprotocol.MethodInvocationCancel, Params: mustJSON(agopluginprotocol.CancellationParams{Generation: runtime.generation, InvocationID: params.InvocationID, Reason: ctx.Err().Error()})})
	}
	return raw, err
}

func (runtime *ProcessRuntime) CancelAll(reason string) {
	runtime.retire()
	runtime.mu.Lock()
	invocations := make([]string, 0, len(runtime.active))
	for invocationID := range runtime.active {
		invocations = append(invocations, invocationID)
	}
	runtime.mu.Unlock()
	for _, invocationID := range invocations {
		_ = runtime.send(agopluginprotocol.Envelope{Type: agopluginprotocol.MessageNotification, Method: agopluginprotocol.MethodInvocationCancel, Params: mustJSON(agopluginprotocol.CancellationParams{Generation: runtime.generation, InvocationID: invocationID, Reason: reason})})
	}
}

func (runtime *ProcessRuntime) Dispose(ctx context.Context, reason string) error {
	_, err := runtime.call(ctx, agopluginprotocol.MethodHostDispose, agopluginprotocol.HostDisposeParams{Generation: runtime.generation, Reason: reason})
	return err
}

func (runtime *ProcessRuntime) Terminate() error {
	_ = runtime.stdin.Close()
	if runtime.command.Process != nil {
		_ = syscall.Kill(-runtime.command.Process.Pid, syscall.SIGTERM)
	}
	timer := time.NewTimer(runtime.options.ExitGrace)
	defer timer.Stop()
	select {
	case <-runtime.exited:
		return nil
	case <-timer.C:
		if runtime.command.Process != nil {
			_ = syscall.Kill(-runtime.command.Process.Pid, syscall.SIGKILL)
		}
		<-runtime.exited
		return nil
	}
}

func (runtime *ProcessRuntime) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := "parent-" + strconv.FormatUint(runtime.requestID.Add(1), 10)
	responses := make(chan agopluginprotocol.Envelope, 1)
	runtime.mu.Lock()
	runtime.pending[id] = responses
	runtime.mu.Unlock()
	defer func() {
		runtime.mu.Lock()
		delete(runtime.pending, id)
		runtime.mu.Unlock()
	}()
	if err := runtime.send(agopluginprotocol.Envelope{Type: agopluginprotocol.MessageRequest, ID: id, Method: method, Params: mustJSON(params)}); err != nil {
		return nil, err
	}
	select {
	case response := <-responses:
		if response.OK == nil {
			return nil, fmt.Errorf("plugin child returned a malformed response")
		}
		if !*response.OK {
			if response.Error != nil {
				return nil, response.Error
			}
			return nil, fmt.Errorf("plugin child request failed without an error")
		}
		return response.Result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-runtime.exited:
		runtime.mu.Lock()
		err := runtime.exitErr
		runtime.mu.Unlock()
		if err == nil {
			err = io.EOF
		}
		return nil, fmt.Errorf("plugin child exited: %w", err)
	}
}

func (runtime *ProcessRuntime) send(envelope agopluginprotocol.Envelope) error {
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	runtime.writeMu.Lock()
	defer runtime.writeMu.Unlock()
	if _, err := runtime.stdin.Write(append(encoded, '\n')); err != nil {
		return fmt.Errorf("write plugin child request: %w", err)
	}
	return nil
}

func (runtime *ProcessRuntime) readLoop() {
	for {
		envelope, err := runtime.decoder.Decode()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				runtime.mu.Lock()
				runtime.exitErr = err
				runtime.mu.Unlock()
				if runtime.command.Process != nil {
					_ = syscall.Kill(-runtime.command.Process.Pid, syscall.SIGKILL)
				}
			}
			return
		}
		switch envelope.Type {
		case agopluginprotocol.MessageResponse:
			runtime.mu.Lock()
			responses := runtime.pending[envelope.ID]
			runtime.mu.Unlock()
			if responses != nil {
				responses <- envelope
			}
		case agopluginprotocol.MessageRequest:
			if envelope.Method == agopluginprotocol.MethodUIRequest {
				go runtime.handleUI(envelope)
			} else if envelope.Method == agopluginprotocol.MethodAIAsk {
				runtime.dispatchAIAsk(envelope)
			} else {
				runtime.respondError(envelope.ID, agopluginprotocol.CodeNotFound, "unknown reverse request")
			}
		case agopluginprotocol.MessageNotification:
			if envelope.Method == agopluginprotocol.MethodLog && runtime.options.Log != nil {
				var params agopluginprotocol.LogParams
				if json.Unmarshal(envelope.Params, &params) == nil {
					runtime.options.Log(params)
				}
			}
		}
	}
}

func (runtime *ProcessRuntime) handleAIAsk(envelope agopluginprotocol.Envelope) {
	params, invocation, ok := runtime.prepareAIAsk(envelope)
	if !ok {
		return
	}
	runtime.executeAIAsk(envelope, params, invocation)
}

func (runtime *ProcessRuntime) dispatchAIAsk(envelope agopluginprotocol.Envelope) {
	params, invocation, ok := runtime.prepareAIAsk(envelope)
	if !ok {
		return
	}
	go runtime.executeAIAsk(envelope, params, invocation)
}

func (runtime *ProcessRuntime) prepareAIAsk(envelope agopluginprotocol.Envelope) (agopluginprotocol.AIAskParams, activeInvocation, bool) {
	params, err := agopluginprotocol.DecodeAIAskParams(envelope.Params)
	if err != nil {
		runtime.respondError(envelope.ID, agopluginprotocol.CodeInvalidRequest, "invalid ai.ask request")
		return agopluginprotocol.AIAskParams{}, activeInvocation{}, false
	}
	if params.Generation != runtime.generation {
		runtime.respondError(envelope.ID, agopluginprotocol.CodeStaleGeneration, "stale runtime generation")
		return agopluginprotocol.AIAskParams{}, activeInvocation{}, false
	}
	if runtime.options.AIAsk == nil {
		runtime.respondError(envelope.ID, agopluginprotocol.CodeUnavailable, "ai.ask is unavailable")
		return agopluginprotocol.AIAskParams{}, activeInvocation{}, false
	}
	runtime.mu.Lock()
	invocation, active := runtime.active[params.InvocationID]
	_, knownPlugin := runtime.plugins[params.PluginID]
	runtime.mu.Unlock()
	if !active {
		runtime.respondError(envelope.ID, agopluginprotocol.CodeCancelled, "invocation is not active")
		return agopluginprotocol.AIAskParams{}, activeInvocation{}, false
	}
	if !knownPlugin || params.Generation != invocation.params.Generation || params.ThreadID != invocation.params.ThreadID || params.TurnID != invocation.params.TurnID || params.DeadlineUnixMs != invocation.params.DeadlineUnixMs {
		runtime.respondError(envelope.ID, agopluginprotocol.CodeInvalidRequest, "ai.ask correlation mismatch")
		return agopluginprotocol.AIAskParams{}, activeInvocation{}, false
	}
	return params, invocation, true
}

func (runtime *ProcessRuntime) executeAIAsk(envelope agopluginprotocol.Envelope, params agopluginprotocol.AIAskParams, invocation activeInvocation) {
	ctx, cancel := context.WithDeadline(invocation.ctx, time.UnixMilli(params.DeadlineUnixMs))
	defer cancel()
	go func() {
		select {
		case <-invocation.ctx.Done():
			cancel()
		case <-runtime.retirement.Done():
			cancel()
		case <-ctx.Done():
		}
	}()
	result, callbackErr := runtime.options.AIAsk(ctx, params)
	if callbackErr != nil {
		code := agopluginprotocol.CodePluginError
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			code = agopluginprotocol.CodeTimeout
		} else if errors.Is(ctx.Err(), context.Canceled) {
			code = agopluginprotocol.CodeCancelled
		}
		runtime.respondError(envelope.ID, code, "ai.ask failed")
		return
	}
	if err := ctx.Err(); err != nil {
		code := agopluginprotocol.CodeCancelled
		if errors.Is(err, context.DeadlineExceeded) {
			code = agopluginprotocol.CodeTimeout
		}
		runtime.respondError(envelope.ID, code, "ai.ask expired")
		return
	}
	raw, err := json.Marshal(result)
	if err != nil {
		runtime.respondError(envelope.ID, agopluginprotocol.CodeInvalidResult, "invalid ai.ask result")
		return
	}
	if _, err := agopluginprotocol.DecodeAIAskResult(raw); err != nil {
		runtime.respondError(envelope.ID, agopluginprotocol.CodeInvalidResult, "invalid ai.ask result")
		return
	}
	ok := true
	_ = runtime.send(agopluginprotocol.Envelope{Type: agopluginprotocol.MessageResponse, ID: envelope.ID, OK: &ok, Result: raw})
}

func (runtime *ProcessRuntime) handleUI(envelope agopluginprotocol.Envelope) {
	var params agopluginprotocol.UIRequestParams
	if err := json.Unmarshal(envelope.Params, &params); err != nil {
		runtime.respondError(envelope.ID, agopluginprotocol.CodeInvalidRequest, "invalid UI request")
		return
	}
	result := agopluginprotocol.HeadlessUIResult(params.Request.Kind)
	if runtime.options.UI != nil {
		ctx := context.Background()
		cancel := func() {}
		if params.DeadlineUnixMs > 0 {
			ctx, cancel = context.WithDeadline(ctx, time.UnixMilli(params.DeadlineUnixMs))
		}
		result = runtime.options.UI(ctx, params)
		cancel()
	}
	ok := true
	_ = runtime.send(agopluginprotocol.Envelope{Type: agopluginprotocol.MessageResponse, ID: envelope.ID, OK: &ok, Result: mustJSON(result)})
}

func (runtime *ProcessRuntime) respondError(id, code, message string) {
	ok := false
	_ = runtime.send(agopluginprotocol.Envelope{Type: agopluginprotocol.MessageResponse, ID: id, OK: &ok, Error: &agopluginprotocol.RPCError{Code: code, Message: message}})
}

func (runtime *ProcessRuntime) finish(err error) {
	runtime.exitOnce.Do(func() {
		runtime.mu.Lock()
		if runtime.exitErr == nil {
			runtime.exitErr = err
		}
		runtime.mu.Unlock()
		close(runtime.exited)
	})
}

func mustJSON(value any) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return encoded
}
