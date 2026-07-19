// Package agopluginprotocol defines the versioned JSONL protocol spoken between
// the Ago parent and its trusted plugin child.
package agopluginprotocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
)

const Version1 = 1

const (
	MessageRequest         = "request"
	MessageResponse        = "response"
	MessageNotification    = "notification"
	MethodInitialize       = "initialize"
	MethodHookInvoke       = "hook.invoke"
	MethodToolExecute      = "tool.execute"
	MethodCommandExecute   = "command.execute"
	MethodHostDispose      = "host.dispose"
	MethodInvocationCancel = "invocation.cancel"
	MethodUIRequest        = "ui.request"
	MethodAIAsk            = "ai.ask"
	MethodLog              = "log"
)

const (
	MaxAIAskQuestionBytes = 4096
	MaxAIAskContextBytes  = 16384
	MaxAIAskOptions       = 16
	MaxAIAskOptionBytes   = 1024
	MaxAIAskReasonBytes   = 4096
)

type AIAnswer string

const (
	AIAnswerYes       AIAnswer = "yes"
	AIAnswerNo        AIAnswer = "no"
	AIAnswerUncertain AIAnswer = "uncertain"
)

type AIAskParams struct {
	Question       string   `json:"question"`
	Context        string   `json:"context,omitempty"`
	Options        []string `json:"options,omitempty"`
	Generation     int64    `json:"generation"`
	PluginID       string   `json:"pluginId"`
	InvocationID   string   `json:"invocationId"`
	ThreadID       string   `json:"threadId"`
	TurnID         string   `json:"turnId"`
	DeadlineUnixMs int64    `json:"deadlineUnixMs"`
}
type AIAskResult struct {
	Answer      AIAnswer `json:"answer"`
	Probability float64  `json:"probability"`
	Reason      string   `json:"reason"`
}

func DecodeAIAskParams(raw json.RawMessage) (AIAskParams, error) {
	var p AIAskParams
	if err := decodeStrict(raw, &p); err != nil {
		return p, err
	}
	if len(p.Question) == 0 || len(p.Question) > MaxAIAskQuestionBytes || len(p.Context) > MaxAIAskContextBytes || len(p.Options) > MaxAIAskOptions || p.Generation <= 0 || p.PluginID == "" || p.InvocationID == "" || p.ThreadID == "" || p.TurnID == "" || p.DeadlineUnixMs <= 0 {
		return p, fmt.Errorf("invalid ai.ask parameters")
	}
	for _, option := range p.Options {
		if option == "" || len(option) > MaxAIAskOptionBytes {
			return p, fmt.Errorf("invalid ai.ask option")
		}
	}
	return p, nil
}
func DecodeAIAskResult(raw json.RawMessage) (AIAskResult, error) {
	var r AIAskResult
	if err := decodeStrict(raw, &r); err != nil {
		return r, err
	}
	if (r.Answer != AIAnswerYes && r.Answer != AIAnswerNo && r.Answer != AIAnswerUncertain) || math.IsNaN(r.Probability) || math.IsInf(r.Probability, 0) || r.Probability < 0 || r.Probability > 1 || len(r.Reason) == 0 || len(r.Reason) > MaxAIAskReasonBytes {
		return r, fmt.Errorf("invalid ai.ask result")
	}
	return r, nil
}
func decodeStrict(raw []byte, dst any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return fmt.Errorf("trailing JSON value")
	}
	return nil
}

type Envelope struct {
	Type   string          `json:"type"`
	ID     string          `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	OK     *bool           `json:"ok,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *RPCError       `json:"error,omitempty"`
}

type PluginConfig struct {
	PluginID string          `json:"pluginId"`
	EntryURI string          `json:"entryUri"`
	Config   json.RawMessage `json:"config"`
}
type Limits struct {
	MaxMessageBytes int `json:"maxMessageBytes"`
	MaxInflight     int `json:"maxInflight"`
}
type Capabilities struct {
	UI         []UIKind `json:"ui"`
	RenderMode string   `json:"renderMode"`
}
type InitializeParams struct {
	SupportedProtocolVersions []int          `json:"supportedProtocolVersions"`
	Generation                int64          `json:"generation"`
	WorkspaceURI              *string        `json:"workspaceUri"`
	Plugins                   []PluginConfig `json:"plugins,omitempty"`
	Capabilities              Capabilities   `json:"capabilities"`
	Limits                    Limits         `json:"limits"`
}
type ToolRegistration struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}
type CommandRegistration struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Category    string `json:"category,omitempty"`
	Description string `json:"description,omitempty"`
}
type PluginRegistration struct {
	PluginID string                `json:"pluginId"`
	Tools    []ToolRegistration    `json:"tools,omitempty"`
	Commands []CommandRegistration `json:"commands,omitempty"`
	Hooks    []string              `json:"hooks,omitempty"`
}
type InitializeResult struct {
	ProtocolVersion int                  `json:"protocolVersion"`
	Generation      int64                `json:"generation"`
	Plugins         []PluginRegistration `json:"plugins,omitempty"`
}

type InvocationParams struct {
	Generation     int64           `json:"generation"`
	InvocationID   string          `json:"invocationId"`
	ThreadID       string          `json:"threadId"`
	TurnID         string          `json:"turnId"`
	DeadlineUnixMs int64           `json:"deadlineUnixMs"`
	Payload        json.RawMessage `json:"payload,omitempty"`
}
type CancellationParams struct {
	Generation   int64  `json:"generation"`
	InvocationID string `json:"invocationId"`
	Reason       string `json:"reason"`
}

type HostDisposeParams struct {
	Generation int64  `json:"generation"`
	Reason     string `json:"reason"`
}

type UIKind string

const (
	UINotify  UIKind = "notify"
	UIConfirm UIKind = "confirm"
	UIInput   UIKind = "input"
	UISelect  UIKind = "select"
)

type UIRequest struct {
	Kind         UIKind   `json:"kind"`
	Title        string   `json:"title,omitempty"`
	Message      string   `json:"message,omitempty"`
	ConfirmLabel string   `json:"confirmLabel,omitempty"`
	HelpText     string   `json:"helpText,omitempty"`
	InitialValue string   `json:"initialValue,omitempty"`
	SubmitLabel  string   `json:"submitLabel,omitempty"`
	Options      []string `json:"options,omitempty"`
}
type UIRequestParams struct {
	Generation     int64     `json:"generation"`
	PluginID       string    `json:"pluginId"`
	InvocationID   string    `json:"invocationId"`
	DeadlineUnixMs int64     `json:"deadlineUnixMs"`
	Request        UIRequest `json:"request"`
}
type UIStatus string

const (
	UIStatusOK          UIStatus = "ok"
	UIStatusCancelled   UIStatus = "cancelled"
	UIStatusUnavailable UIStatus = "unavailable"
	UIStatusTimeout     UIStatus = "timeout"
)

type UIResult struct {
	Status UIStatus        `json:"status"`
	Value  json.RawMessage `json:"value,omitempty"`
}

func HeadlessUIResult(kind UIKind) UIResult {
	switch kind {
	case UINotify:
		return UIResult{Status: UIStatusOK}
	case UIConfirm:
		return UIResult{Status: UIStatusOK, Value: json.RawMessage(`false`)}
	default:
		return UIResult{Status: UIStatusUnavailable}
	}
}

type LogParams struct {
	Generation   int64  `json:"generation"`
	PluginID     string `json:"pluginId"`
	InvocationID string `json:"invocationId,omitempty"`
	Level        string `json:"level"`
	Message      string `json:"message"`
}

const (
	CodeInvalidRequest      = "INVALID_REQUEST"
	CodeIncompatibleVersion = "INCOMPATIBLE_VERSION"
	CodeNotFound            = "NOT_FOUND"
	CodeInvalidResult       = "INVALID_RESULT"
	CodeCancelled           = "CANCELLED"
	CodeTimeout             = "TIMEOUT"
	CodeUnavailable         = "UNAVAILABLE"
	CodeOverloaded          = "OVERLOADED"
	CodePluginError         = "PLUGIN_ERROR"
	CodeStaleGeneration     = "STALE_GENERATION"
)

type RPCError struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string             { return e.Message }
func rpcError(code, message string) *RPCError { return &RPCError{Code: code, Message: message} }
