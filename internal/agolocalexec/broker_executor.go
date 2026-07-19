package agolocalexec

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"claudexflow/internal/agocoordinator"
	"claudexflow/internal/agoprotocol"
)

type BrokerExecutor struct {
	Supervisor       string
	Command          string
	Arguments        []string
	ReadRoots        []string
	DefaultDeadline  time.Duration
	Output           OutputBudget
	Protocol         string
	Provider         string
	Model            string
	ProviderCallback ProviderCallback
}

func (executor BrokerExecutor) Run(ctx context.Context, request agocoordinator.TurnRequest) error {
	if executor.Protocol != "" || executor.Provider != "" || executor.Model != "" {
		execution, err := executor.Start(ctx, request)
		if err != nil {
			return err
		}
		for range execution.Events() {
		}
		_ = execution.CloseInput()
		return execution.Wait()
	}
	if !canonicalPath(executor.Supervisor) {
		return fmt.Errorf("supervisor must be canonical and absolute")
	}
	plan, cleanup, err := executor.buildPlan(ctx, request)
	if err != nil {
		return err
	}
	defer cleanup()
	result, err := ExecuteBroker(ctx, executor.Supervisor, plan)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("sandboxed local executor exited %d (stdout=%q stderr=%q, dropped=%d/%d)",
			result.ExitCode, joinedOutput(result.Stdout), joinedOutput(result.Stderr), result.Stdout.DroppedBytes, result.Stderr.DroppedBytes)
	}
	return nil
}

func (executor BrokerExecutor) Start(ctx context.Context, request agocoordinator.TurnRequest) (agocoordinator.Execution, error) {
	if executor.Protocol != "pi-jsonl-v1" || strings.TrimSpace(executor.Provider) == "" || strings.TrimSpace(executor.Model) == "" {
		return nil, fmt.Errorf("pi session protocol, provider, and model are required")
	}
	var content struct {
		Text string `json:"text"`
	}
	dec := json.NewDecoder(bytes.NewReader(request.Content))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&content); err != nil {
		return nil, fmt.Errorf("decode turn content: %w", err)
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return nil, fmt.Errorf("turn content must contain one object")
	}
	plan, cleanup, err := executor.buildPlan(ctx, request)
	if err != nil {
		return nil, err
	}
	transcript, err := piTranscript(request)
	if err != nil {
		cleanup()
		return nil, err
	}
	plan.Stdin, err = piBootstrap(plan.WorkingDir, executor.Provider, executor.Model, transcript, request.Tools, content.Text)
	if err != nil {
		cleanup()
		return nil, err
	}
	plan.Protocol = ProtocolBudget{ID: executor.Protocol, MaxFrameBytes: 1 << 20, MaxEvents: 10000, MaxEventBytes: 64 << 20, AbortGrace: 2 * time.Second}
	plan, err = BindSeatbeltProfile(plan)
	if err != nil {
		cleanup()
		return nil, err
	}
	session, err := StartBrokerWithProvider(ctx, executor.Supervisor, plan, executor.ProviderCallback)
	if err != nil {
		cleanup()
		return nil, err
	}
	e := &brokerExecution{session: session, events: make(chan agocoordinator.ExecutionEvent), cleanup: cleanup}
	go e.decodeEvents()
	return e, nil
}

type brokerExecution struct {
	session   *BrokerSession
	events    chan agocoordinator.ExecutionEvent
	cleanup   func()
	index     uint64
	decodeErr error
	mu        sync.Mutex
	waitOnce  sync.Once
	waitErr   error
}

func (e *brokerExecution) Events() <-chan agocoordinator.ExecutionEvent { return e.events }
func (e *brokerExecution) decodeEvents() {
	defer close(e.events)
	for line := range e.session.Events() {
		eventType, payload, err := decodePiEvent(line)
		if err != nil {
			e.mu.Lock()
			e.decodeErr = err
			e.mu.Unlock()
			e.session.live.Close()
			for range e.session.Events() {
			}
			return
		}
		e.index++
		e.events <- agocoordinator.ExecutionEvent{Index: e.index, Type: eventType, Payload: payload}
	}
}
func (e *brokerExecution) Send(ctx context.Context, c agocoordinator.ExecutionControl) error {
	line, err := encodePiControl(c)
	if err != nil {
		return err
	}
	return e.session.Send(ctx, line)
}
func (e *brokerExecution) CloseInput() error { return e.session.CloseInput() }
func (e *brokerExecution) Wait() error {
	e.waitOnce.Do(func() {
		e.waitErr = e.session.Wait()
		e.mu.Lock()
		if e.decodeErr != nil {
			e.waitErr = e.decodeErr
		}
		e.mu.Unlock()
		e.cleanup()
	})
	return e.waitErr
}

func piBootstrap(cwd, provider, model string, transcript []json.RawMessage, tools []agocoordinator.ExternalTool, prompt string) ([]byte, error) {
	if transcript == nil {
		transcript = []json.RawMessage{}
	}
	if tools == nil {
		tools = []agocoordinator.ExternalTool{}
	}
	seen := make(map[string]bool)
	for _, tool := range tools {
		if strings.TrimSpace(tool.Name) == "" || strings.TrimSpace(tool.Description) == "" || !json.Valid(tool.InputSchema) || seen[tool.Name] {
			return nil, fmt.Errorf("invalid or duplicate external tool %q", tool.Name)
		}
		seen[tool.Name] = true
	}
	initialize, err := json.Marshal(map[string]any{"type": "initialize", "transcript": transcript, "cwd": cwd, "provider": provider, "model": model, "tools": tools})
	if err != nil {
		return nil, fmt.Errorf("encode Pi initialize: %w", err)
	}
	if len(initialize) > 1<<20 {
		return nil, fmt.Errorf("Pi initialize frame exceeds protocol budget")
	}
	promptLine, err := json.Marshal(map[string]any{"type": "prompt", "text": prompt})
	if err != nil {
		return nil, fmt.Errorf("encode Pi prompt: %w", err)
	}
	return append(append(initialize, '\n'), append(promptLine, '\n')...), nil
}

func piTranscript(request agocoordinator.TurnRequest) ([]json.RawMessage, error) {
	projection := request.Context
	allowedTools := map[string]bool{"ago_echo": true}
	for _, tool := range request.Tools {
		allowedTools[tool.Name] = true
	}
	transcript := make([]json.RawMessage, 0, len(projection.Tail)+1)
	boundary := uint64(0)
	if projection.Compaction != nil {
		if projection.Compaction.ThreadID != request.ThreadID || projection.Compaction.ThroughSequence == 0 || strings.TrimSpace(projection.Compaction.Summary) == "" {
			return nil, fmt.Errorf("invalid Ago compaction projection")
		}
		boundary = projection.Compaction.ThroughSequence
		summary, err := json.Marshal(map[string]any{"role": "summary", "text": projection.Compaction.Summary, "at": boundary})
		if err != nil {
			return nil, err
		}
		transcript = append(transcript, summary)
	}
	previous := boundary
	currentAccepted := 0
	pendingToolCalls := make(map[string]string)
	for _, event := range projection.Tail {
		if err := event.Validate(); err != nil {
			return nil, fmt.Errorf("invalid projected event: %w", err)
		}
		if event.ThreadID != request.ThreadID || event.Sequence <= previous {
			return nil, fmt.Errorf("invalid projected event ordering")
		}
		previous = event.Sequence
		switch event.Type {
		case agoprotocol.EventMessageAccepted:
			fields, err := decodeJSONObject(event.Payload)
			if err != nil || requireExactFields(fields, []string{"queue_item_id", "turn_id", "content"}) != nil {
				return nil, fmt.Errorf("invalid accepted-message projection")
			}
			var turnID string
			if json.Unmarshal(fields["turn_id"], &turnID) != nil || strings.TrimSpace(turnID) == "" {
				return nil, fmt.Errorf("invalid accepted-message turn")
			}
			contentFields, err := decodeJSONObject(fields["content"])
			if err != nil || requireExactFields(contentFields, []string{"text"}) != nil {
				return nil, fmt.Errorf("invalid accepted-message content")
			}
			var text string
			if json.Unmarshal(contentFields["text"], &text) != nil {
				return nil, fmt.Errorf("invalid accepted-message text")
			}
			if turnID == request.TurnID {
				currentAccepted++
				var current struct {
					Text string `json:"text"`
				}
				if json.Unmarshal(request.Content, &current) != nil || current.Text != text {
					return nil, fmt.Errorf("current prompt does not match durable accepted message")
				}
				continue
			}
			message, _ := json.Marshal(map[string]any{"role": "user", "text": text, "at": event.Sequence})
			transcript = append(transcript, message)
		case agoprotocol.EventAssistantCompleted:
			fields, err := decodeJSONObject(event.Payload)
			if err != nil || requireExactFields(fields, []string{"turn_id", "executor_event_index", "event"}) != nil {
				return nil, fmt.Errorf("invalid assistant projection envelope")
			}
			eventFields, err := decodeJSONObject(fields["event"])
			if err != nil || requireExactFields(eventFields, []string{"type", "message"}) != nil {
				return nil, fmt.Errorf("invalid assistant projection event")
			}
			var eventType string
			if json.Unmarshal(eventFields["type"], &eventType) != nil || eventType != "assistant_completed" {
				return nil, fmt.Errorf("invalid assistant projection type")
			}
			messageFields, err := decodeJSONObject(eventFields["message"])
			if err != nil || requireExactFields(messageFields, []string{"role", "api", "provider", "model", "stopReason", "content", "at"}) != nil {
				return nil, fmt.Errorf("invalid canonical assistant message")
			}
			var role string
			if json.Unmarshal(messageFields["role"], &role) != nil || role != "assistant" {
				return nil, fmt.Errorf("invalid canonical assistant role")
			}
			var content []json.RawMessage
			if json.Unmarshal(messageFields["content"], &content) != nil {
				return nil, fmt.Errorf("invalid canonical assistant content")
			}
			for _, block := range content {
				blockFields, err := decodeJSONObject(block)
				if err != nil {
					return nil, fmt.Errorf("invalid canonical assistant content block")
				}
				var blockType string
				if json.Unmarshal(blockFields["type"], &blockType) != nil {
					return nil, fmt.Errorf("invalid canonical assistant content type")
				}
				if blockType != "toolCall" {
					continue
				}
				var callID, name string
				if json.Unmarshal(blockFields["callId"], &callID) != nil || json.Unmarshal(blockFields["name"], &name) != nil || callID == "" || !allowedTools[name] {
					return nil, fmt.Errorf("invalid canonical assistant tool call")
				}
				if _, duplicate := pendingToolCalls[callID]; duplicate {
					return nil, fmt.Errorf("duplicate canonical assistant tool call %q", callID)
				}
				pendingToolCalls[callID] = name
			}
			transcript = append(transcript, append(json.RawMessage(nil), eventFields["message"]...))
		case agoprotocol.EventToolResultPrepared:
			fields, err := decodeJSONObject(event.Payload)
			if err != nil || requireExactFields(fields, []string{"turn_id", "call_id", "name", "output", "error"}) != nil {
				return nil, fmt.Errorf("invalid durable tool result")
			}
			var callID, name, output string
			var failed bool
			if json.Unmarshal(fields["call_id"], &callID) != nil || json.Unmarshal(fields["name"], &name) != nil || json.Unmarshal(fields["output"], &output) != nil || json.Unmarshal(fields["error"], &failed) != nil || callID == "" || !allowedTools[name] || pendingToolCalls[callID] != name {
				return nil, fmt.Errorf("invalid durable tool result fields")
			}
			delete(pendingToolCalls, callID)
			message, _ := json.Marshal(map[string]any{"role": "tool", "callId": callID, "name": name, "text": output, "error": failed, "at": event.Sequence})
			transcript = append(transcript, message)
		case agoprotocol.EventThreadCreated, agoprotocol.EventMessageQueued, agoprotocol.EventMessageQueueEdited,
			agoprotocol.EventMessageSteered, agoprotocol.EventMessageDequeued, agoprotocol.EventTurnStarted,
			agoprotocol.EventTurnCompleted, agoprotocol.EventTurnFailed, agoprotocol.EventTurnCancelRequested,
			agoprotocol.EventTurnCancelled, agoprotocol.EventAgentStarted, agoprotocol.EventAssistantTextDelta,
			agoprotocol.EventToolRequested, agoprotocol.EventToolCompleted, agoprotocol.EventToolFailed, agoprotocol.EventToolAcknowledged, agoprotocol.EventAgentStopped, agoprotocol.EventAgentSettled,
			agoprotocol.EventAgentQueueUpdated, agoprotocol.EventPluginDialogRequested, agoprotocol.EventPluginDialogResolved:
			// Explicitly non-transcript events.
		case agoprotocol.EventCompactionRecorded:
			return nil, fmt.Errorf("compaction event leaked into projected tail")
		default:
			return nil, fmt.Errorf("unsupported projected event type %q", event.Type)
		}
	}
	if currentAccepted != 1 {
		return nil, fmt.Errorf("projected context contains %d current accepted messages", currentAccepted)
	}
	if len(pendingToolCalls) != 0 {
		return nil, fmt.Errorf("projected context contains unresolved assistant tool calls")
	}
	return transcript, nil
}

func decodePiEvent(line []byte) (string, json.RawMessage, error) {
	fields, err := decodeJSONObject(line)
	if err != nil {
		return "", nil, fmt.Errorf("invalid Pi event frame: %w", err)
	}
	var eventType string
	if err := json.Unmarshal(fields["type"], &eventType); err != nil {
		return "", nil, fmt.Errorf("invalid Pi event type")
	}
	var expected []string
	switch eventType {
	case "started", "settled":
		expected = []string{"type"}
	case "text":
		expected = []string{"type", "delta"}
	case "assistant_completed":
		expected = []string{"type", "message", "provider_usage"}
	case "tool_invocation":
		expected = []string{"type", "callId", "name", "input"}
	case "tool_finished":
		expected = []string{"type", "callId", "error"}
	case "stopped":
		expected = []string{"type", "reason"}
	case "queue":
		expected = []string{"type", "steering", "followUp"}
	default:
		return "", nil, fmt.Errorf("unsupported Pi event type %q", eventType)
	}
	if err := requireExactFields(fields, expected); err != nil {
		return "", nil, err
	}
	if eventType == "assistant_completed" {
		if err := validateProviderUsageFrame(fields["provider_usage"]); err != nil {
			return "", nil, err
		}
	}
	return eventType, append(json.RawMessage(nil), line...), nil
}

func validateProviderUsageFrame(raw json.RawMessage) error {
	fields, err := decodeJSONObject(raw)
	if err != nil {
		return fmt.Errorf("invalid provider_usage: %w", err)
	}
	if err := requireExactFields(fields, []string{"provider", "model", "request_id", "status", "usage", "cost"}); err != nil {
		return fmt.Errorf("invalid provider_usage: %w", err)
	}
	for _, name := range []string{"provider", "model"} {
		var value string
		if json.Unmarshal(fields[name], &value) != nil || value == "" {
			return fmt.Errorf("invalid provider_usage %s", name)
		}
	}
	var requestID *string
	if json.Unmarshal(fields["request_id"], &requestID) != nil || requestID != nil && *requestID == "" {
		return fmt.Errorf("invalid provider_usage request_id")
	}
	var status string
	if json.Unmarshal(fields["status"], &status) != nil || status != "final" && status != "provisional" {
		return fmt.Errorf("invalid provider_usage status")
	}
	usage, err := decodeJSONObject(fields["usage"])
	if err != nil {
		return fmt.Errorf("invalid provider_usage usage: %w", err)
	}
	requiredUsage := []string{"input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens", "total_tokens"}
	allowedUsage := append(append([]string(nil), requiredUsage...), "cache_write_1h_tokens", "reasoning_tokens")
	if err := requireAllowedFields(usage, requiredUsage, allowedUsage); err != nil {
		return fmt.Errorf("invalid provider_usage usage: %w", err)
	}
	for name, value := range usage {
		if !validNonnegativeInteger(value) {
			return fmt.Errorf("invalid provider_usage usage.%s", name)
		}
	}
	cost, err := decodeJSONObject(fields["cost"])
	if err != nil {
		return fmt.Errorf("invalid provider_usage cost: %w", err)
	}
	if err := requireExactFields(cost, []string{"input", "output", "cache_read", "cache_write", "total"}); err != nil {
		return fmt.Errorf("invalid provider_usage cost: %w", err)
	}
	for name, value := range cost {
		var number json.Number
		decoder := json.NewDecoder(bytes.NewReader(value))
		decoder.UseNumber()
		if decoder.Decode(&number) != nil {
			return fmt.Errorf("invalid provider_usage cost.%s", name)
		}
		parsed, parseErr := number.Float64()
		if parseErr != nil || parsed < 0 || parsed > 1.7976931348623157e308 {
			return fmt.Errorf("invalid provider_usage cost.%s", name)
		}
	}
	return nil
}

func validNonnegativeInteger(raw json.RawMessage) bool {
	var number json.Number
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&number) != nil {
		return false
	}
	value, err := number.Int64()
	return err == nil && value >= 0
}

func requireAllowedFields(fields map[string]json.RawMessage, required, allowed []string) error {
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, field := range allowed {
		allowedSet[field] = struct{}{}
	}
	for _, field := range required {
		if _, found := fields[field]; !found {
			return fmt.Errorf("unknown or missing protocol field")
		}
	}
	for field := range fields {
		if _, found := allowedSet[field]; !found {
			return fmt.Errorf("unknown or missing protocol field")
		}
	}
	return nil
}

func encodePiControl(control agocoordinator.ExecutionControl) ([]byte, error) {
	fields := map[string]json.RawMessage{}
	if len(control.Payload) > 0 {
		var err error
		fields, err = decodeJSONObject(control.Payload)
		if err != nil {
			return nil, fmt.Errorf("invalid control payload: %w", err)
		}
	}
	if _, exists := fields["type"]; exists {
		return nil, fmt.Errorf("control payload cannot replace type")
	}
	typeJSON, _ := json.Marshal(control.Type)
	fields["type"] = typeJSON
	var expected []string
	switch control.Type {
	case "abort":
		expected = []string{"type"}
	case "steer", "follow_up":
		expected = []string{"type", "text"}
	case "tool_result":
		expected = []string{"type", "callId", "name", "output"}
		if _, present := fields["error"]; present {
			expected = append(expected, "error")
		}
	default:
		return nil, fmt.Errorf("unsupported Pi control type %q", control.Type)
	}
	if err := requireExactFields(fields, expected); err != nil {
		return nil, err
	}
	return json.Marshal(fields)
}

func decodeJSONObject(encoded []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var fields map[string]json.RawMessage
	if err := decoder.Decode(&fields); err != nil {
		return nil, err
	}
	if fields == nil {
		return nil, fmt.Errorf("JSON object is required")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return nil, fmt.Errorf("exactly one JSON object is required")
	}
	return fields, nil
}

func requireExactFields(fields map[string]json.RawMessage, expected []string) error {
	if len(fields) != len(expected) {
		return fmt.Errorf("unknown or missing protocol field")
	}
	for _, field := range expected {
		if _, exists := fields[field]; !exists {
			return fmt.Errorf("missing protocol field %q", field)
		}
	}
	return nil
}

func (executor BrokerExecutor) buildPlan(ctx context.Context, request agocoordinator.TurnRequest) (LaunchPlan, func(), error) {
	if !canonicalPath(executor.Command) {
		return LaunchPlan{}, func() {}, fmt.Errorf("executor command must be canonical and absolute")
	}
	workspace, err := filepath.EvalSymlinks(request.Workspace)
	if err != nil {
		return LaunchPlan{}, func() {}, fmt.Errorf("resolve executor workspace: %w", err)
	}
	if !canonicalPath(workspace) {
		return LaunchPlan{}, func() {}, fmt.Errorf("executor workspace must resolve to a canonical absolute path")
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return LaunchPlan{}, func() {}, fmt.Errorf("encode turn request: %w", err)
	}
	payload = append(payload, '\n')
	jobRoot, err := os.MkdirTemp("", "ago-job-")
	if err != nil {
		return LaunchPlan{}, func() {}, fmt.Errorf("create job root: %w", err)
	}
	rawJobRoot := jobRoot
	jobRoot, err = filepath.EvalSymlinks(rawJobRoot)
	if err != nil {
		_ = os.RemoveAll(rawJobRoot)
		return LaunchPlan{}, func() {}, fmt.Errorf("resolve job root: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(jobRoot) }
	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		cleanup()
		return LaunchPlan{}, func() {}, fmt.Errorf("create approval nonce: %w", err)
	}
	deadline := executor.DefaultDeadline
	if deadline <= 0 {
		deadline = 30 * time.Minute
	}
	if at, ok := ctx.Deadline(); ok {
		deadline = time.Until(at)
	}
	output := executor.Output
	if output.HeadBytes <= 0 || output.TailBytes <= 0 {
		output = OutputBudget{HeadBytes: 64 << 10, TailBytes: 64 << 10}
	}
	plan := LaunchPlan{
		Origin:      "model:" + request.ThreadID + "/" + request.TurnID,
		Executable:  executor.Command,
		Arguments:   append([]string(nil), executor.Arguments...),
		Stdin:       payload,
		WorkingDir:  workspace,
		Environment: restrictedTurnEnvironment(request),
		ReadRoots: []string{
			workspace, executor.Command, filepath.Dir(executor.Command),
			"/System", "/usr/lib", "/usr/bin", "/bin", "/usr/share", "/private/var/db/dyld", "/dev/null", "/dev/urandom",
		},
		WriteRoots:    []string{workspace, jobRoot},
		SyntheticHome: filepath.Join(jobRoot, "home"),
		SyntheticTemp: filepath.Join(jobRoot, "tmp"),
		ProfileID:     "ago.model.v1",
		Network:       NetworkDisabled,
		Deadline:      deadline,
		Output:        output,
		ApprovalNonce: hex.EncodeToString(nonceBytes),
	}
	plan.ReadRoots = append(plan.ReadRoots, executor.ReadRoots...)
	bound, err := BindSeatbeltProfile(plan)
	if err != nil {
		cleanup()
		return LaunchPlan{}, func() {}, fmt.Errorf("bind executor sandbox profile: %w", err)
	}
	return bound, cleanup, nil
}

func restrictedTurnEnvironment(request agocoordinator.TurnRequest) map[string]string {
	return map[string]string{
		"AGO_AGENT_MODE": string(request.Mode),
		"AGO_THREAD_ID":  request.ThreadID,
		"AGO_TURN_ID":    request.TurnID,
		"LANG":           "C.UTF-8",
		"PATH":           "/usr/bin:/bin",
	}
}

func canonicalPath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path
}

func joinedOutput(output CollectedOutput) string {
	if output.DroppedBytes == 0 {
		return string(output.Head) + string(output.Tail)
	}
	return string(output.Head) + fmt.Sprintf("…[%d bytes dropped]…", output.DroppedBytes) + string(output.Tail)
}

var _ agocoordinator.Executor = BrokerExecutor{}
var _ agocoordinator.SessionExecutor = BrokerExecutor{}
