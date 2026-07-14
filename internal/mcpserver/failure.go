package mcpserver

import (
	"strings"

	"claudexflow/internal/claude"
)

const (
	failureNone              = ""
	failureAuthConfiguration = "auth_configuration"
	failureTransport         = "transport"
	failureTimeout           = "timeout"
	failureMaxTurns          = "max_turns"
	failureRefusal           = "refusal"
	failureInvalidOutput     = "invalid_output"
	failureModelMismatch     = "model_mismatch"
	failureScopeViolation    = "scope_violation"
	failureExecution         = "execution"
)

type failureInfo struct {
	Class         string
	RetryEligible bool
	Detail        string
}

func classifyRunFailure(result claude.Result, write bool) failureInfo {
	detail := runFailure(result)
	combined := strings.ToLower(strings.Join([]string{result.ExitError, result.Subtype, result.TerminalReason, result.Text, result.Stderr}, "\n"))
	noObservedWork := result.SessionID == "" && len(result.ChangedPaths) == 0 && totalToolUses(result.ToolUses) == 0
	switch {
	case strings.Contains(combined, "max_turn") || strings.Contains(combined, "max turns"):
		return failureInfo{Class: failureMaxTurns, Detail: detail}
	case strings.Contains(combined, "stop_reason: refusal") || strings.Contains(combined, "terminal reason: refusal") || result.TerminalReason == "refusal":
		return failureInfo{Class: failureRefusal, Detail: detail}
	case strings.Contains(combined, "authentication") || strings.Contains(combined, "auth source") || strings.Contains(combined, "connectors are disabled") || strings.Contains(combined, "oauth") || strings.Contains(combined, "unauthorized") || strings.Contains(combined, "401"):
		return failureInfo{Class: failureAuthConfiguration, RetryEligible: noObservedWork, Detail: detail}
	case strings.Contains(combined, "timeout") || strings.Contains(combined, "deadline exceeded"):
		return failureInfo{Class: failureTimeout, RetryEligible: noObservedWork && !write, Detail: detail}
	case strings.Contains(combined, "connection refused") || strings.Contains(combined, "connection reset") || strings.Contains(combined, "econn") || strings.Contains(combined, "unexpected eof") || strings.Contains(combined, "502") || strings.Contains(combined, "503") || strings.Contains(combined, "504"):
		return failureInfo{Class: failureTransport, RetryEligible: noObservedWork, Detail: detail}
	default:
		return failureInfo{Class: failureExecution, Detail: detail}
	}
}

func executionIdentity(requestedModel, requestedEffort string, result claude.Result) ExecutionIdentity {
	verification := "unverified"
	if result.ResolvedModel != "" {
		if modelMatches(requestedModel, result.ResolvedModel) {
			verification = "verified"
		} else {
			verification = "mismatch"
		}
	}
	return ExecutionIdentity{
		RequestedModel:     requestedModel,
		ResolvedModel:      result.ResolvedModel,
		ModelVerification:  verification,
		RequestedEffort:    requestedEffort,
		EffortVerification: "cli_argument_only",
		AuthSource:         string(result.AuthSource),
	}
}

func modelMatches(requested, resolved string) bool {
	requested = strings.ToLower(strings.TrimSpace(requested))
	resolved = strings.ToLower(strings.TrimSpace(resolved))
	if requested == "" || resolved == "" {
		return false
	}
	if requested == resolved || strings.HasPrefix(resolved, requested+"-") || strings.HasPrefix(resolved, requested+"[") {
		return true
	}
	// Family match: grok-4.5 ↔ grok-4.5-build-*, but never match claude-opus-* to grok-*.
	if strings.HasPrefix(requested, "grok") {
		return strings.Contains(resolved, "grok")
	}
	if strings.HasPrefix(requested, "gpt-5.6") || strings.HasPrefix(requested, "gpt-5") {
		return strings.Contains(resolved, "gpt-5") || strings.Contains(resolved, "sol") || strings.Contains(resolved, "terra") || strings.Contains(resolved, "luna")
	}
	if strings.HasPrefix(requested, "gemini") {
		return strings.Contains(resolved, "gemini")
	}
	switch requested {
	case "opus":
		return strings.Contains(resolved, "opus")
	case "sonnet", "sonnet[1m]":
		return strings.Contains(resolved, "sonnet")
	case "fable", "claude-fable-5":
		return strings.Contains(resolved, "fable")
	default:
		return false
	}
}

func totalToolUses(values map[string]int) int {
	total := 0
	for _, count := range values {
		total += count
	}
	return total
}
