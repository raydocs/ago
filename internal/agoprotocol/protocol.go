package agoprotocol

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

const SchemaVersion = 1

type CommandType string

const (
	CommandThreadCreate        CommandType = "thread.create"
	CommandMessageAppend       CommandType = "message.append"
	CommandMessageSubmit       CommandType = "message.submit"
	CommandMessageEditQueued   CommandType = "message.edit-queued"
	CommandMessageSteer        CommandType = "message.steer"
	CommandMessageDequeue      CommandType = "message.dequeue"
	CommandSafePoint           CommandType = "turn.safe-point"
	CommandTurnComplete        CommandType = "turn.complete"
	CommandTurnFail            CommandType = "turn.fail"
	CommandTurnInterrupt       CommandType = "turn.interrupt"
	CommandTurnCancel          CommandType = "turn.cancel"
	CommandTurnSettleCancelled CommandType = "turn.settle-cancelled"
	CommandTurnEventAppend     CommandType = "turn.event-append"
	CommandThreadCompact       CommandType = "thread.compact"
	CommandThreadArchive       CommandType = "thread.archive"
	CommandThreadUnarchive     CommandType = "thread.unarchive"
)

type Command struct {
	SchemaVersion    int             `json:"schema_version"`
	CommandID        string          `json:"command_id"`
	IdempotencyKey   string          `json:"idempotency_key"`
	ActorID          string          `json:"actor_id"`
	Type             CommandType     `json:"type"`
	ThreadID         string          `json:"thread_id,omitempty"`
	ExpectedSequence *uint64         `json:"expected_sequence,omitempty"`
	Payload          json.RawMessage `json:"payload,omitempty"`
}

func (command Command) Validate() error {
	if command.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported command schema version %d", command.SchemaVersion)
	}
	if command.CommandID == "" {
		return fmt.Errorf("command_id is required")
	}
	if command.IdempotencyKey == "" {
		return fmt.Errorf("idempotency_key is required")
	}
	if command.ActorID == "" {
		return fmt.Errorf("actor_id is required")
	}
	if command.Type == "" {
		return fmt.Errorf("command type is required")
	}
	if command.Type != CommandThreadCreate && command.ThreadID == "" {
		return fmt.Errorf("thread_id is required for %s", command.Type)
	}
	return nil
}

type EventType string

const (
	EventThreadCreated         EventType = "thread.created"
	EventMessageQueued         EventType = "message.queued"
	EventMessageQueueEdited    EventType = "message.queue-edited"
	EventMessageSteered        EventType = "message.steered"
	EventMessageDequeued       EventType = "message.dequeued"
	EventMessageAccepted       EventType = "message.accepted"
	EventTurnStarted           EventType = "turn.started"
	EventTurnCompleted         EventType = "turn.completed"
	EventTurnFailed            EventType = "turn.failed"
	EventTurnCancelRequested   EventType = "turn.cancel-requested"
	EventTurnCancelled         EventType = "turn.cancelled"
	EventAgentStarted          EventType = "agent.started"
	EventAssistantTextDelta    EventType = "assistant.text-delta"
	EventAssistantCompleted    EventType = "assistant.completed"
	EventToolRequested         EventType = "tool.requested"
	EventToolCompleted         EventType = "tool.completed"
	EventToolFailed            EventType = "tool.failed"
	EventToolResultPrepared    EventType = "tool.result-prepared"
	EventToolAcknowledged      EventType = "tool.acknowledged"
	EventAgentStopped          EventType = "agent.stopped"
	EventAgentSettled          EventType = "agent.settled"
	EventAgentQueueUpdated     EventType = "agent.queue-updated"
	EventCompactionRecorded    EventType = "compaction.recorded"
	EventPluginDialogRequested EventType = "plugin.dialog.requested"
	EventPluginDialogResolved  EventType = "plugin.dialog.resolved"
	EventThreadArchived        EventType = "thread.archived"
	EventThreadUnarchived      EventType = "thread.unarchived"
	EventVerificationRecorded  EventType = "verification.check-recorded"
	EventProviderUsageRecorded EventType = "provider.usage-recorded"
)

type Visibility string

const (
	VisibilityInternal Visibility = "internal"
	VisibilityUser     Visibility = "user"
	VisibilityAudit    Visibility = "audit"
)

type Provenance struct {
	RootThreadID    string `json:"root_thread_id,omitempty"`
	ParentThreadID  string `json:"parent_thread_id,omitempty"`
	SourceThreadID  string `json:"source_thread_id,omitempty"`
	ReplyToThreadID string `json:"reply_to_thread_id,omitempty"`
}

type Event struct {
	SchemaVersion int             `json:"schema_version"`
	EventID       string          `json:"event_id"`
	ThreadID      string          `json:"thread_id"`
	Sequence      uint64          `json:"sequence"`
	Type          EventType       `json:"type"`
	Visibility    Visibility      `json:"visibility"`
	Provenance    Provenance      `json:"provenance,omitempty"`
	Payload       json.RawMessage `json:"payload,omitempty"`
}

func (event Event) Validate() error {
	if event.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported event schema version %d", event.SchemaVersion)
	}
	if event.EventID == "" {
		return fmt.Errorf("event_id is required")
	}
	if event.ThreadID == "" {
		return fmt.Errorf("thread_id is required")
	}
	if event.Sequence == 0 {
		return fmt.Errorf("sequence must start at one")
	}
	if event.Type == "" {
		return fmt.Errorf("event type is required")
	}
	switch event.Visibility {
	case VisibilityInternal, VisibilityUser, VisibilityAudit:
	default:
		return fmt.Errorf("unsupported event visibility %q", event.Visibility)
	}
	if event.Type == EventMessageAccepted && event.Provenance.ParentThreadID != "" {
		if event.Provenance.RootThreadID == "" {
			return fmt.Errorf("root_thread_id is required for an inter-thread message")
		}
		if event.Provenance.SourceThreadID == "" {
			return fmt.Errorf("source_thread_id is required for an inter-thread message")
		}
		if event.Provenance.ReplyToThreadID == "" {
			return fmt.Errorf("reply_to_thread_id is required for an inter-thread message")
		}
	}
	return nil
}

type ExecutorType string

const (
	ExecutorLocal  ExecutorType = "local"
	ExecutorOrb    ExecutorType = "orb"
	ExecutorRunner ExecutorType = "runner"
)

var runnerIDPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9.-]{0,61}[A-Za-z0-9])?$`)

type ExecutorTarget struct {
	Type     ExecutorType `json:"type"`
	RunnerID string       `json:"runner_id,omitempty"`
}

func (target ExecutorTarget) Validate() error {
	switch target.Type {
	case ExecutorLocal, ExecutorOrb:
		if target.RunnerID != "" {
			return fmt.Errorf("runner_id is only valid for a runner executor")
		}
	case ExecutorRunner:
		if target.RunnerID == "" {
			return fmt.Errorf("runner_id is required for a runner executor")
		}
		if !runnerIDPattern.MatchString(target.RunnerID) || strings.Contains(target.RunnerID, "..") {
			return fmt.Errorf("runner_id must be hostname-valid")
		}
	default:
		return fmt.Errorf("unsupported executor type %q", target.Type)
	}
	return nil
}

type QueueClass string

const (
	QueueNormal QueueClass = "normal"
	QueueSteer  QueueClass = "steer"
)

func (class QueueClass) Validate() error {
	switch class {
	case QueueNormal, QueueSteer:
		return nil
	default:
		return fmt.Errorf("unsupported queue class %q", class)
	}
}

type Activity string

const (
	ActivityIdle             Activity = "idle"
	ActivityRunning          Activity = "running"
	ActivityAwaitingApproval Activity = "awaiting-approval"
	ActivityError            Activity = "error"
)

type QueueItemState string

const (
	QueueItemPending          QueueItemState = "pending"
	QueueItemInterruptPending QueueItemState = "interrupt-pending"
)

type AgentMode string

const (
	AgentModeLow    AgentMode = "low"
	AgentModeMedium AgentMode = "medium"
	AgentModeHigh   AgentMode = "high"
	AgentModeUltra  AgentMode = "ultra"
)

func (mode AgentMode) Validate() error {
	switch mode {
	case AgentModeLow, AgentModeMedium, AgentModeHigh, AgentModeUltra:
		return nil
	default:
		return fmt.Errorf("unsupported agent mode %q", mode)
	}
}
