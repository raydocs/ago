package agothreadstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
)

func TestRecordVerificationCheckIsImmutableAuditedAndTruthPreserving(t *testing.T) {
	store := openTestStore(t)
	threadID := mustCreateThread(t, store, "verification-check").ThreadID
	input := VerificationCheckInput{
		ThreadID: threadID, IdempotencyKey: "check-1", CheckID: "go-test",
		Command: "go test ./internal/agothreadstore", Status: VerificationUnknown,
		OutputSummary: "command exited without a machine-readable test result",
	}

	check, err := store.RecordVerificationCheck(context.Background(), input)
	if err != nil {
		t.Fatalf("RecordVerificationCheck() error = %v", err)
	}
	if check.RecordID == "" || check.CreatedSequence != 2 || check.Status != VerificationUnknown {
		t.Fatalf("check = %#v; want durable unknown record at sequence 2", check)
	}
	retry, err := store.RecordVerificationCheck(context.Background(), input)
	if err != nil || !reflect.DeepEqual(retry, check) {
		t.Fatalf("exact retry = %#v, %v; want %#v", retry, err, check)
	}
	changed := input
	changed.Status = VerificationPassed
	if _, err := store.RecordVerificationCheck(context.Background(), changed); !isVerificationConflict(err) {
		t.Fatalf("changed retry error = %v, want VerificationConflictError", err)
	}

	checks, err := store.VerificationChecks(context.Background(), threadID)
	if err != nil || !reflect.DeepEqual(checks, []VerificationCheck{check}) {
		t.Fatalf("VerificationChecks() = %#v, %v; want %#v", checks, err, []VerificationCheck{check})
	}
	events, err := store.Replay(context.Background(), threadID, 1, 0)
	if err != nil || len(events) != 1 || events[0].Sequence != check.CreatedSequence || events[0].Visibility != "audit" || events[0].Type != verificationCheckRecordedEvent {
		t.Fatalf("audit events = %#v, %v", events, err)
	}
	var payload map[string]any
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil || payload["status"] != string(VerificationUnknown) {
		t.Fatalf("audit payload = %#v, %v; status must remain explicitly unknown", payload, err)
	}
}

func TestVerificationCheckRejectsUnboundedOrUnsupportedClaims(t *testing.T) {
	store := openTestStore(t)
	threadID := mustCreateThread(t, store, "verification-validation").ThreadID
	base := VerificationCheckInput{ThreadID: threadID, IdempotencyKey: "key", CheckID: "lint", Command: "go vet ./...", Status: VerificationPassed, OutputSummary: "go vet exited 0"}
	tests := map[string]func(*VerificationCheckInput){
		"missing command":    func(in *VerificationCheckInput) { in.Command = "" },
		"missing summary":    func(in *VerificationCheckInput) { in.OutputSummary = "" },
		"unsupported status": func(in *VerificationCheckInput) { in.Status = "looks-good" },
		"oversized command":  func(in *VerificationCheckInput) { in.Command = strings.Repeat("x", maxVerificationCommandBytes+1) },
		"oversized summary": func(in *VerificationCheckInput) {
			in.OutputSummary = strings.Repeat("x", maxVerificationSummaryBytes+1)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			input := base
			input.IdempotencyKey = "key-" + name
			mutate(&input)
			if _, err := store.RecordVerificationCheck(context.Background(), input); err == nil {
				t.Fatal("RecordVerificationCheck() accepted invalid claim")
			}
		})
	}
}

func TestRecordProviderUsagePreservesExactNumbersAndProjectsStatusSeparately(t *testing.T) {
	store := openTestStore(t)
	threadID := mustCreateThread(t, store, "provider-usage").ThreadID
	provisional := usageTestInput(threadID, "usage-provisional", UsageProvisional, "0.10", "0.02")
	final := usageTestInput(threadID, "usage-final", UsageFinal, "0.20", "0.03")
	final.RequestID = "provider-request-2"
	final.Usage.InputTokens = json.Number("9007199254740993")

	first, err := store.RecordProviderUsage(context.Background(), provisional)
	if err != nil {
		t.Fatalf("RecordProviderUsage(provisional) error = %v", err)
	}
	second, err := store.RecordProviderUsage(context.Background(), final)
	if err != nil {
		t.Fatalf("RecordProviderUsage(final) error = %v", err)
	}
	if string(second.Usage.InputTokens) != "9007199254740993" || string(first.Cost.Total) != "0.12" {
		t.Fatalf("records lost exact provider values: %#v %#v", first, second)
	}

	projection, err := store.ProviderUsageProjection(context.Background(), threadID)
	if err != nil {
		t.Fatalf("ProviderUsageProjection() error = %v", err)
	}
	if projection.Provisional.RecordCount != 1 || projection.Final.RecordCount != 1 {
		t.Fatalf("projection status counts = %#v", projection)
	}
	if got := string(projection.Provisional.Cost.Total); got != "0.12" {
		t.Fatalf("provisional total cost = %q, want exact 0.12", got)
	}
	if got := string(projection.Final.Usage.InputTokens); got != "9007199254740993" {
		t.Fatalf("final input tokens = %q, want exact integer", got)
	}
	if got := string(projection.Final.Cost.Total); got != "0.23" {
		t.Fatalf("final total cost = %q, want exact 0.23", got)
	}

	retry, err := store.RecordProviderUsage(context.Background(), provisional)
	if err != nil || !reflect.DeepEqual(retry, first) {
		t.Fatalf("exact usage retry = %#v, %v; want %#v", retry, err, first)
	}
	changed := provisional
	changed.Cost.Total = json.Number("0.13")
	if _, err := store.RecordProviderUsage(context.Background(), changed); !isVerificationConflict(err) {
		t.Fatalf("changed usage retry error = %v, want VerificationConflictError", err)
	}
}

func TestRecordProviderUsagePreservesScientificNotationCost(t *testing.T) {
	store := openTestStore(t)
	threadID := mustCreateThread(t, store, "provider-usage-exponent").ThreadID
	input := usageTestInput(threadID, "usage-exponent", UsageFinal, "1e-7", "2.5e+3")
	record, err := store.RecordProviderUsage(context.Background(), input)
	if err != nil {
		t.Fatalf("RecordProviderUsage() error = %v", err)
	}
	if record.Cost.Input != "1e-7" || record.Cost.Output != "2.5e+3" {
		t.Fatalf("scientific notation was normalized: %#v", record.Cost)
	}
}

func TestProviderUsageRejectsNonFiniteNegativeFractionalTokensAndInvalidStatus(t *testing.T) {
	store := openTestStore(t)
	threadID := mustCreateThread(t, store, "provider-usage-validation").ThreadID
	base := usageTestInput(threadID, "usage", UsageFinal, "0.1", "0.2")
	tests := map[string]func(*ProviderUsageInput){
		"negative tokens":    func(in *ProviderUsageInput) { in.Usage.InputTokens = "-1" },
		"fractional tokens":  func(in *ProviderUsageInput) { in.Usage.OutputTokens = "1.5" },
		"nonfinite tokens":   func(in *ProviderUsageInput) { in.Usage.TotalTokens = "NaN" },
		"negative cost":      func(in *ProviderUsageInput) { in.Cost.Input = "-0.01" },
		"nonfinite cost":     func(in *ProviderUsageInput) { in.Cost.Total = "+Inf" },
		"empty numeric":      func(in *ProviderUsageInput) { in.Cost.Output = "" },
		"unsupported status": func(in *ProviderUsageInput) { in.Status = "estimated" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			input := base
			input.IdempotencyKey = "usage-" + name
			mutate(&input)
			if _, err := store.RecordProviderUsage(context.Background(), input); err == nil {
				t.Fatal("RecordProviderUsage() accepted invalid provider numeric data")
			}
		})
	}
}

func TestVerificationSidecarIsLazyIdempotentAndSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sidecar.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	threadID := mustCreateThread(t, store, "sidecar-reopen").ThreadID
	var count int
	if err := store.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='verification_checks'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("sidecar was not lazy: count=%d err=%v", count, err)
	}
	if err := store.EnsureVerificationSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureVerificationSchema(context.Background()); err != nil {
		t.Fatalf("second EnsureVerificationSchema() error = %v", err)
	}
	input := usageTestInput(threadID, "reopen", UsageFinal, "0.01", "0.02")
	recorded, err := store.RecordProviderUsage(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	ledger, err := reopened.ProviderUsageLedger(context.Background(), threadID)
	if err != nil || !reflect.DeepEqual(ledger, []ProviderUsageRecord{recorded}) {
		t.Fatalf("reopened ledger = %#v, %v; want %#v", ledger, err, []ProviderUsageRecord{recorded})
	}
}

func TestVerificationSidecarMigratesIndependentVersionZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sidecar-migration.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.db.Exec(`CREATE TABLE verification_schema (singleton INTEGER PRIMARY KEY, version INTEGER NOT NULL); INSERT INTO verification_schema VALUES (1,0)`); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureVerificationSchema(context.Background()); err != nil {
		t.Fatalf("EnsureVerificationSchema() migration error = %v", err)
	}
	var version int
	if err := store.db.QueryRow(`SELECT version FROM verification_schema WHERE singleton=1`).Scan(&version); err != nil || version != currentVerificationSchemaVersion {
		t.Fatalf("sidecar version = %d, %v; want %d", version, err, currentVerificationSchemaVersion)
	}
	mainVersion, err := store.SchemaVersion(context.Background())
	if err != nil || mainVersion != CurrentStoreSchemaVersion {
		t.Fatalf("main schema changed = %d, %v", mainVersion, err)
	}
}

func TestVerificationSidecarRowsAreDatabaseImmutable(t *testing.T) {
	store := openTestStore(t)
	threadID := mustCreateThread(t, store, "sidecar-immutable").ThreadID
	check, err := store.RecordVerificationCheck(context.Background(), VerificationCheckInput{ThreadID: threadID, IdempotencyKey: "check", CheckID: "test", Command: "go test", Status: VerificationPassed, OutputSummary: "exit 0"})
	if err != nil {
		t.Fatal(err)
	}
	usage, err := store.RecordProviderUsage(context.Background(), usageTestInput(threadID, "usage", UsageFinal, "0.1", "0.2"))
	if err != nil {
		t.Fatal(err)
	}
	for name, query := range map[string]string{
		"update check": `UPDATE verification_checks SET idempotency_key='changed' WHERE record_id='` + check.RecordID + `'`,
		"delete check": `DELETE FROM verification_checks WHERE record_id='` + check.RecordID + `'`,
		"update usage": `UPDATE provider_usage_records SET idempotency_key='changed' WHERE record_id='` + usage.RecordID + `'`,
		"delete usage": `DELETE FROM provider_usage_records WHERE record_id='` + usage.RecordID + `'`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := store.db.Exec(query); err == nil {
				t.Fatal("immutable sidecar row was mutated")
			}
		})
	}
}

func TestProviderUsageRollsBackRecordAndSequenceWhenAuditAppendFails(t *testing.T) {
	store := openTestStore(t)
	threadID := mustCreateThread(t, store, "usage-atomic-rollback").ThreadID
	if err := store.EnsureVerificationSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`CREATE TRIGGER reject_provider_usage_event BEFORE INSERT ON events
WHEN json_extract(NEW.event_json,'$.type')='provider.usage-recorded'
BEGIN SELECT RAISE(ABORT,'reject usage audit for test'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordProviderUsage(context.Background(), usageTestInput(threadID, "rollback", UsageFinal, "0.1", "0.2")); err == nil {
		t.Fatal("RecordProviderUsage() succeeded despite rejected audit event")
	}
	var rows int
	if err := store.db.QueryRow(`SELECT count(*) FROM provider_usage_records WHERE thread_id=?`, threadID).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("rolled-back usage rows = %d, %v; want 0", rows, err)
	}
	thread, err := store.Thread(context.Background(), threadID)
	if err != nil || thread.LastSequence != 1 {
		t.Fatalf("sequence after rollback = %d, %v; want 1", thread.LastSequence, err)
	}
}

func TestVerificationAndUsageConcurrentAppendsHaveAtomicThreadSequences(t *testing.T) {
	store := openTestStore(t)
	threadID := mustCreateThread(t, store, "verification-concurrency").ThreadID
	const writes = 24
	var wg sync.WaitGroup
	errs := make(chan error, writes)
	sequences := make(chan uint64, writes)
	for i := 0; i < writes; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			if index%2 == 0 {
				record, err := store.RecordVerificationCheck(context.Background(), VerificationCheckInput{ThreadID: threadID, IdempotencyKey: fmt.Sprintf("check-%d", index), CheckID: fmt.Sprintf("check-%d", index), Command: "go test", Status: VerificationPassed, OutputSummary: "exit 0"})
				if err == nil {
					sequences <- record.CreatedSequence
				}
				errs <- err
				return
			}
			input := usageTestInput(threadID, fmt.Sprintf("usage-%d", index), UsageFinal, "0.1", "0.2")
			input.RequestID = fmt.Sprintf("request-%d", index)
			record, err := store.RecordProviderUsage(context.Background(), input)
			if err == nil {
				sequences <- record.CreatedSequence
			}
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	close(sequences)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent append error = %v", err)
		}
	}
	got := make([]int, 0, writes)
	for sequence := range sequences {
		got = append(got, int(sequence))
	}
	sort.Ints(got)
	for index, sequence := range got {
		if want := index + 2; sequence != want {
			t.Fatalf("sequences = %v; index %d got %d want %d", got, index, sequence, want)
		}
	}
	thread, err := store.Thread(context.Background(), threadID)
	if err != nil || thread.LastSequence != writes+1 {
		t.Fatalf("thread sequence = %d, %v; want %d", thread.LastSequence, err, writes+1)
	}
}

func usageTestInput(threadID, key string, status UsageRecordStatus, inputCost, outputCost string) ProviderUsageInput {
	return ProviderUsageInput{
		ThreadID: threadID, IdempotencyKey: key, Provider: "test-provider", Model: "test-model", RequestID: "provider-request-1", Status: status,
		Usage: ProviderTokenUsage{InputTokens: "10", OutputTokens: "2", CacheReadTokens: "3", CacheWriteTokens: "4", TotalTokens: "19"},
		Cost:  ProviderCost{Input: json.Number(inputCost), Output: json.Number(outputCost), CacheRead: "0", CacheWrite: "0", Total: json.Number(addTestDecimal(inputCost, outputCost))},
	}
}

func addTestDecimal(left, right string) string {
	// Tests only use two decimal places.
	var leftWhole, leftFraction, rightWhole, rightFraction int
	fmt.Sscanf(left, "%d.%d", &leftWhole, &leftFraction)
	fmt.Sscanf(right, "%d.%d", &rightWhole, &rightFraction)
	return fmt.Sprintf("%d.%02d", leftWhole+rightWhole+(leftFraction+rightFraction)/100, (leftFraction+rightFraction)%100)
}

func isVerificationConflict(err error) bool {
	var conflict VerificationConflictError
	return errors.As(err, &conflict)
}
