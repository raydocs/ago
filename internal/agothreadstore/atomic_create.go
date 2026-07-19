package agothreadstore

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	"claudexflow/internal/agoprotocol"
)

// ProjectIdentity is the immutable project identity selected when a thread is created.
type ProjectIdentity struct {
	ProjectID   string `json:"project_id"`
	DisplayName string `json:"display_name,omitempty"`
}

// AgentDefinitionSnapshot freezes the executable agent definition for a thread.
type AgentDefinitionSnapshot struct {
	DefinitionID             string                `json:"definition_id"`
	Version                  string                `json:"version"`
	DisplayName              string                `json:"display_name"`
	SystemInstructions       string                `json:"system_instructions,omitempty"`
	SystemInstructionsRef    string                `json:"system_instructions_ref,omitempty"`
	SystemInstructionsDigest string                `json:"system_instructions_digest,omitempty"`
	Capabilities             []string              `json:"capabilities,omitempty"`
	DefaultMode              agoprotocol.AgentMode `json:"default_mode"`
	Provenance               string                `json:"provenance,omitempty"`
}

type AtomicCreateInput struct {
	Spec           ThreadSpec              `json:"spec"`
	Project        ProjectIdentity         `json:"project"`
	Agent          AgentDefinitionSnapshot `json:"agent"`
	Provenance     agoprotocol.Provenance  `json:"provenance,omitempty"`
	InitialMessage json.RawMessage         `json:"initial_message"`
}

func (input *AtomicCreateInput) validate() error {
	if err := input.Spec.Validate(); err != nil {
		return err
	}
	canonicalWorkspace, err := filepath.EvalSymlinks(filepath.Clean(input.Spec.Workspace))
	if err != nil {
		return fmt.Errorf("canonicalize workspace: %w", err)
	}
	if !filepath.IsAbs(canonicalWorkspace) {
		return fmt.Errorf("canonical workspace must be absolute")
	}
	input.Spec.Workspace = canonicalWorkspace
	if input.Project.ProjectID == "" {
		return fmt.Errorf("project.project_id is required")
	}
	if input.Agent.DefinitionID == "" || input.Agent.Version == "" || input.Agent.DisplayName == "" {
		return fmt.Errorf("agent definition_id, version, and display_name are required")
	}
	if input.Agent.SystemInstructions == "" && input.Agent.SystemInstructionsRef == "" && input.Agent.SystemInstructionsDigest == "" {
		return fmt.Errorf("agent system instructions or immutable ref/digest is required")
	}
	if err := input.Agent.DefaultMode.Validate(); err != nil {
		return fmt.Errorf("agent default mode: %w", err)
	}
	if len(input.InitialMessage) == 0 || !json.Valid(input.InitialMessage) {
		return fmt.Errorf("initial_message must be valid JSON")
	}
	return nil
}

// CreateAtomicThread is the authoritative public create operation. Identity,
// initial input, active turn, events, and idempotency result commit together.
func (store *Store) CreateAtomicThread(ctx context.Context, command agoprotocol.Command, input AtomicCreateInput) (MailboxState, error) {
	if err := command.Validate(); err != nil {
		return MailboxState{}, err
	}
	if command.Type != agoprotocol.CommandThreadCreate {
		return MailboxState{}, fmt.Errorf("CreateAtomicThread requires a %q command", agoprotocol.CommandThreadCreate)
	}
	if err := input.validate(); err != nil {
		return MailboxState{}, err
	}
	hash, err := hashMailboxRequest(command, input)
	if err != nil {
		return MailboxState{}, err
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return MailboxState{}, fmt.Errorf("begin atomic thread create: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if existing, found, err := storedMailboxResult(ctx, tx, command, hash); err != nil {
		return MailboxState{}, err
	} else if found {
		return existing, nil
	}
	threadID, err := randomID("T-")
	if err != nil {
		return MailboxState{}, err
	}
	turnID, err := randomID("R-")
	if err != nil {
		return MailboxState{}, err
	}
	queueItemID, err := randomID("Q-")
	if err != nil {
		return MailboxState{}, err
	}
	projectJSON, _ := json.Marshal(input.Project)
	agentJSON, _ := json.Marshal(input.Agent)
	provenanceJSON, _ := json.Marshal(input.Provenance)
	_, err = tx.ExecContext(ctx, `INSERT INTO threads (thread_id,last_sequence,activity,active_turn_id,title,workspace,mode,executor_type,runner_id,project_json,agent_snapshot_json,provenance_json) VALUES (?,3,?,?,?,?,?,?,?,?,?,?)`, threadID, agoprotocol.ActivityRunning, turnID, input.Spec.Title, input.Spec.Workspace, input.Spec.Mode, input.Spec.Executor.Type, input.Spec.Executor.RunnerID, projectJSON, agentJSON, provenanceJSON)
	if err != nil {
		return MailboxState{}, fmt.Errorf("insert atomic thread: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `INSERT INTO thread_catalog (thread_id,project_id,created_at,updated_at) VALUES (?,?,?,?)`, threadID, input.Project.ProjectID, now, now); err != nil {
		return MailboxState{}, fmt.Errorf("insert atomic thread catalog entry: %w", err)
	}
	createdPayload, _ := json.Marshal(input)
	drafts := []EventDraft{{Type: agoprotocol.EventThreadCreated, Visibility: agoprotocol.VisibilityUser, Provenance: input.Provenance, Payload: createdPayload}, mailboxEvent(agoprotocol.EventMessageAccepted, queueItemID, turnID, input.InitialMessage), mailboxEvent(agoprotocol.EventTurnStarted, queueItemID, turnID, nil)}
	drafts[1].Provenance, drafts[2].Provenance = input.Provenance, input.Provenance
	events := make([]agoprotocol.Event, 0, 3)
	for i, draft := range drafts {
		eventID, err := randomID("E-")
		if err != nil {
			return MailboxState{}, err
		}
		event := agoprotocol.Event{SchemaVersion: agoprotocol.SchemaVersion, EventID: eventID, ThreadID: threadID, Sequence: uint64(i + 1), Type: draft.Type, Visibility: draft.Visibility, Provenance: draft.Provenance, Payload: cloneRawMessage(draft.Payload)}
		if err := event.Validate(); err != nil {
			return MailboxState{}, err
		}
		if err := insertEvent(ctx, tx, event); err != nil {
			return MailboxState{}, err
		}
		events = append(events, event)
	}
	state := MailboxState{ThreadID: threadID, LastSequence: 3, Activity: agoprotocol.ActivityRunning, ActiveTurnID: turnID, Queue: []QueueItem{}, Events: events}
	createCommand := command
	createCommand.ThreadID = threadID
	if err := insertMailboxCommand(ctx, tx, createCommand, hash, state); err != nil {
		return MailboxState{}, fmt.Errorf("insert atomic create command: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return MailboxState{}, fmt.Errorf("commit atomic thread create: %w", err)
	}
	return state, nil
}
