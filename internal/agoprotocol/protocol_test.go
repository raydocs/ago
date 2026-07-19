package agoprotocol

import "testing"

func TestCommandValidateRequiresStableEnvelope(t *testing.T) {
	valid := Command{
		SchemaVersion:  SchemaVersion,
		CommandID:      "cmd-1",
		IdempotencyKey: "request-1",
		ActorID:        "user-1",
		Type:           CommandMessageAppend,
		ThreadID:       "thread-1",
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid command rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*Command)
	}{
		{name: "schema", mutate: func(command *Command) { command.SchemaVersion = 0 }},
		{name: "command id", mutate: func(command *Command) { command.CommandID = "" }},
		{name: "idempotency key", mutate: func(command *Command) { command.IdempotencyKey = "" }},
		{name: "actor", mutate: func(command *Command) { command.ActorID = "" }},
		{name: "type", mutate: func(command *Command) { command.Type = "" }},
		{name: "thread", mutate: func(command *Command) { command.ThreadID = "" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := valid
			test.mutate(&candidate)
			if err := candidate.Validate(); err == nil {
				t.Fatalf("command without %s was accepted", test.name)
			}
		})
	}
}

func TestThreadCreateDoesNotRequireExistingThreadID(t *testing.T) {
	command := Command{
		SchemaVersion:  SchemaVersion,
		CommandID:      "cmd-create",
		IdempotencyKey: "request-create",
		ActorID:        "user-1",
		Type:           CommandThreadCreate,
	}
	if err := command.Validate(); err != nil {
		t.Fatalf("thread creation command rejected: %v", err)
	}
}

func TestEventValidateRequiresOrderedEnvelope(t *testing.T) {
	event := Event{
		SchemaVersion: SchemaVersion,
		EventID:       "event-1",
		ThreadID:      "thread-1",
		Sequence:      1,
		Type:          EventThreadCreated,
		Visibility:    VisibilityUser,
	}
	if err := event.Validate(); err != nil {
		t.Fatalf("valid event rejected: %v", err)
	}

	event.Sequence = 0
	if err := event.Validate(); err == nil {
		t.Fatal("event with sequence zero was accepted")
	}
}

func TestInterThreadMessageRequiresAuthenticatedProvenance(t *testing.T) {
	event := Event{
		SchemaVersion: SchemaVersion,
		EventID:       "event-message",
		ThreadID:      "thread-child",
		Sequence:      1,
		Type:          EventMessageAccepted,
		Visibility:    VisibilityUser,
		Provenance: Provenance{
			RootThreadID:    "thread-root",
			ParentThreadID:  "thread-root",
			SourceThreadID:  "thread-root",
			ReplyToThreadID: "thread-root",
		},
	}
	if err := event.Validate(); err != nil {
		t.Fatalf("message with provenance rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*Provenance)
	}{
		{name: "root", mutate: func(provenance *Provenance) { provenance.RootThreadID = "" }},
		{name: "source", mutate: func(provenance *Provenance) { provenance.SourceThreadID = "" }},
		{name: "reply route", mutate: func(provenance *Provenance) { provenance.ReplyToThreadID = "" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := event
			test.mutate(&candidate.Provenance)
			if err := candidate.Validate(); err == nil {
				t.Fatalf("inter-thread message without %s provenance was accepted", test.name)
			}
		})
	}
}

func TestExecutorTargetValidationPreservesPlacementSemantics(t *testing.T) {
	tests := []struct {
		name    string
		target  ExecutorTarget
		wantErr bool
	}{
		{name: "local", target: ExecutorTarget{Type: ExecutorLocal}},
		{name: "orb", target: ExecutorTarget{Type: ExecutorOrb}},
		{name: "runner", target: ExecutorTarget{Type: ExecutorRunner, RunnerID: "build-mac"}},
		{name: "runner missing id", target: ExecutorTarget{Type: ExecutorRunner}, wantErr: true},
		{name: "runner invalid id", target: ExecutorTarget{Type: ExecutorRunner, RunnerID: "Build Mac"}, wantErr: true},
		{name: "local with runner id", target: ExecutorTarget{Type: ExecutorLocal, RunnerID: "build-mac"}, wantErr: true},
		{name: "unknown", target: ExecutorTarget{Type: "automatic"}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.target.Validate()
			if (err != nil) != test.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestQueueClassValidationRejectsImplicitPriority(t *testing.T) {
	for _, class := range []QueueClass{QueueNormal, QueueSteer} {
		if err := class.Validate(); err != nil {
			t.Fatalf("valid queue class %q rejected: %v", class, err)
		}
	}
	if err := QueueClass("urgent").Validate(); err == nil {
		t.Fatal("unknown queue priority was accepted")
	}
}
