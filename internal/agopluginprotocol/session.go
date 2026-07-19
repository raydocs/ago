package agopluginprotocol

import "encoding/json"

type SessionConfig struct {
	SupportedProtocolVersions []int
	Generation                int64
	Registrations             []PluginRegistration
}
type Session struct {
	config      SessionConfig
	initialized bool
	requestIDs  map[string]struct{}
}

func NewSession(config SessionConfig) *Session {
	if len(config.SupportedProtocolVersions) == 0 {
		config.SupportedProtocolVersions = []int{Version1}
	}
	return &Session{config: config, requestIDs: map[string]struct{}{}}
}

func (s *Session) Accept(e Envelope) (json.RawMessage, error) {
	if e.Type != MessageRequest && e.Type != MessageNotification && e.Type != MessageResponse {
		return nil, rpcError(CodeInvalidRequest, "unknown message type")
	}
	if !s.initialized && (e.Type != MessageRequest || e.Method != MethodInitialize) {
		return nil, rpcError(CodeInvalidRequest, "initialize must be the first message")
	}
	if e.Type == MessageRequest {
		if e.ID == "" || e.Method == "" {
			return nil, rpcError(CodeInvalidRequest, "request ID and method are required")
		}
		if _, ok := s.requestIDs[e.ID]; ok {
			return nil, rpcError(CodeInvalidRequest, "duplicate request ID")
		}
		s.requestIDs[e.ID] = struct{}{}
	}
	if e.Method == MethodInitialize {
		if e.Type != MessageRequest || s.initialized {
			return nil, rpcError(CodeInvalidRequest, "invalid initialize request")
		}
		return s.initialize(e.Params)
	}
	if e.Type == MessageResponse {
		if e.ID == "" || e.Method != "" || e.OK == nil {
			return nil, rpcError(CodeInvalidRequest, "response ID and ok are required and method is forbidden")
		}
		if (*e.OK && e.Error != nil) || (!*e.OK && e.Error == nil) {
			return nil, rpcError(CodeInvalidRequest, "response result does not match ok")
		}
		return nil, nil
	}
	expected := map[string]string{MethodHookInvoke: MessageRequest, MethodToolExecute: MessageRequest, MethodCommandExecute: MessageRequest, MethodHostDispose: MessageRequest, MethodInvocationCancel: MessageNotification, MethodUIRequest: MessageRequest, MethodAIAsk: MessageRequest, MethodLog: MessageNotification}
	want, ok := expected[e.Method]
	if !ok {
		return nil, rpcError(CodeNotFound, "unknown method")
	}
	if e.Type != want {
		return nil, rpcError(CodeInvalidRequest, "method used with wrong message type")
	}
	var generation int64
	switch e.Method {
	case MethodHostDispose:
		var p HostDisposeParams
		if !decode(e.Params, &p) || p.Reason == "" {
			return nil, rpcError(CodeInvalidRequest, "invalid host disposal")
		}
		generation = p.Generation
	case MethodInvocationCancel:
		var p CancellationParams
		if !decode(e.Params, &p) || p.InvocationID == "" || p.Reason == "" {
			return nil, rpcError(CodeInvalidRequest, "invalid cancellation")
		}
		generation = p.Generation
	case MethodUIRequest:
		var p UIRequestParams
		if !decode(e.Params, &p) || p.PluginID == "" || p.InvocationID == "" || p.DeadlineUnixMs == 0 || !validUIKind(p.Request.Kind) {
			return nil, rpcError(CodeInvalidRequest, "invalid UI request")
		}
		generation = p.Generation
	case MethodAIAsk:
		p, err := DecodeAIAskParams(e.Params)
		if err != nil {
			return nil, rpcError(CodeInvalidRequest, "invalid AI ask")
		}
		generation = p.Generation
	case MethodLog:
		var p LogParams
		if !decode(e.Params, &p) {
			return nil, rpcError(CodeInvalidRequest, "invalid log")
		}
		generation = p.Generation
	default:
		var p InvocationParams
		if !decode(e.Params, &p) || p.InvocationID == "" || p.ThreadID == "" || p.TurnID == "" || p.DeadlineUnixMs == 0 {
			return nil, rpcError(CodeInvalidRequest, "invalid invocation")
		}
		generation = p.Generation
	}
	if generation != s.config.Generation {
		return nil, rpcError(CodeStaleGeneration, "stale runtime generation")
	}
	return nil, nil
}
func (s *Session) initialize(raw json.RawMessage) (json.RawMessage, error) {
	var p InitializeParams
	if !decode(raw, &p) {
		return nil, rpcError(CodeInvalidRequest, "invalid initialize parameters")
	}
	version := negotiate(s.config.SupportedProtocolVersions, p.SupportedProtocolVersions)
	if version == 0 {
		return nil, rpcError(CodeIncompatibleVersion, "no compatible protocol version")
	}
	if p.Generation != s.config.Generation {
		return nil, rpcError(CodeStaleGeneration, "stale runtime generation")
	}
	s.initialized = true
	result, _ := json.Marshal(InitializeResult{ProtocolVersion: version, Generation: s.config.Generation, Plugins: s.config.Registrations})
	return result, nil
}
func decode(raw json.RawMessage, dst any) bool {
	return len(raw) > 0 && json.Unmarshal(raw, dst) == nil
}
func validUIKind(k UIKind) bool {
	return k == UINotify || k == UIConfirm || k == UIInput || k == UISelect
}
func negotiate(a, b []int) int {
	best := 0
	for _, x := range a {
		for _, y := range b {
			if x == y && x > best {
				best = x
			}
		}
	}
	return best
}
