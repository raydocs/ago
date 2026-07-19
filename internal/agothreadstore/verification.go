package agothreadstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"

	"claudexflow/internal/agoprotocol"
)

const (
	currentVerificationSchemaVersion = 1
	maxVerificationCommandBytes      = 4096
	maxVerificationSummaryBytes      = 16 * 1024
	maxProviderNumberBytes           = 128

	verificationCheckRecordedEvent = agoprotocol.EventVerificationRecorded
	providerUsageRecordedEvent     = agoprotocol.EventProviderUsageRecorded
)

type VerificationStatus string

const (
	VerificationPassed  VerificationStatus = "passed"
	VerificationFailed  VerificationStatus = "failed"
	VerificationUnknown VerificationStatus = "unknown"
)

type VerificationCheckInput struct {
	ThreadID       string             `json:"thread_id"`
	IdempotencyKey string             `json:"idempotency_key"`
	CheckID        string             `json:"check_id"`
	Command        string             `json:"command"`
	Status         VerificationStatus `json:"status"`
	OutputSummary  string             `json:"output_summary"`
}

type VerificationCheck struct {
	RecordID        string `json:"record_id"`
	CreatedSequence uint64 `json:"created_sequence"`
	VerificationCheckInput
}

type UsageRecordStatus string

const (
	UsageProvisional UsageRecordStatus = "provisional"
	UsageFinal       UsageRecordStatus = "final"
)

// ProviderTokenUsage and ProviderCost retain the provider's JSON number text.
// This avoids float64 rounding and makes a lexical change on retry detectable.
type ProviderTokenUsage struct {
	InputTokens        json.Number `json:"input_tokens"`
	OutputTokens       json.Number `json:"output_tokens"`
	CacheReadTokens    json.Number `json:"cache_read_tokens"`
	CacheWriteTokens   json.Number `json:"cache_write_tokens"`
	TotalTokens        json.Number `json:"total_tokens"`
	CacheWrite1HTokens json.Number `json:"cache_write_1h_tokens,omitempty"`
	ReasoningTokens    json.Number `json:"reasoning_tokens,omitempty"`
}

type ProviderCost struct {
	Input      json.Number `json:"input"`
	Output     json.Number `json:"output"`
	CacheRead  json.Number `json:"cache_read"`
	CacheWrite json.Number `json:"cache_write"`
	Total      json.Number `json:"total"`
}

type ProviderUsageInput struct {
	ThreadID       string             `json:"thread_id"`
	IdempotencyKey string             `json:"idempotency_key"`
	Provider       string             `json:"provider"`
	Model          string             `json:"model"`
	RequestID      string             `json:"request_id"`
	Status         UsageRecordStatus  `json:"status"`
	Usage          ProviderTokenUsage `json:"usage"`
	Cost           ProviderCost       `json:"cost"`
}

type ProviderUsageRecord struct {
	RecordID        string `json:"record_id"`
	CreatedSequence uint64 `json:"created_sequence"`
	ProviderUsageInput
}

type ProviderUsageAggregate struct {
	RecordCount uint64             `json:"record_count"`
	Usage       ProviderTokenUsage `json:"usage"`
	Cost        ProviderCost       `json:"cost"`
}

type ProviderUsageProjection struct {
	ThreadID    string                 `json:"thread_id"`
	Provisional ProviderUsageAggregate `json:"provisional"`
	Final       ProviderUsageAggregate `json:"final"`
}

type VerificationConflictError struct{ Reason string }

func (err VerificationConflictError) Error() string {
	return "verification ledger conflict: " + err.Reason
}

// EnsureVerificationSchema installs the independently-versioned sidecar
// schema. Record and projection methods call it lazily; the store root may call
// it eagerly during startup without changing the main store schema version.
func (store *Store) EnsureVerificationSchema(ctx context.Context) error {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin verification schema migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS verification_schema (
 singleton INTEGER PRIMARY KEY CHECK(singleton=1),
 version INTEGER NOT NULL CHECK(version>=0)
);
INSERT OR IGNORE INTO verification_schema(singleton,version) VALUES(1,0);
`); err != nil {
		return fmt.Errorf("initialize verification schema metadata: %w", err)
	}
	var version int
	if err := tx.QueryRowContext(ctx, `SELECT version FROM verification_schema WHERE singleton=1`).Scan(&version); err != nil {
		return fmt.Errorf("read verification schema version: %w", err)
	}
	if version > currentVerificationSchemaVersion {
		return fmt.Errorf("verification schema version %d is newer than supported version %d", version, currentVerificationSchemaVersion)
	}
	if version < 1 {
		if _, err := tx.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS verification_checks (
 record_id TEXT PRIMARY KEY,
 thread_id TEXT NOT NULL REFERENCES threads(thread_id),
 idempotency_key TEXT NOT NULL,
 request_hash TEXT NOT NULL,
 record_json BLOB NOT NULL,
 created_sequence INTEGER NOT NULL CHECK(created_sequence>=1),
 UNIQUE(thread_id,idempotency_key),
 UNIQUE(thread_id,created_sequence)
);
CREATE INDEX IF NOT EXISTS verification_checks_thread_sequence
 ON verification_checks(thread_id,created_sequence);
CREATE TABLE IF NOT EXISTS provider_usage_records (
 record_id TEXT PRIMARY KEY,
 thread_id TEXT NOT NULL REFERENCES threads(thread_id),
 idempotency_key TEXT NOT NULL,
 request_hash TEXT NOT NULL,
 record_json BLOB NOT NULL,
 created_sequence INTEGER NOT NULL CHECK(created_sequence>=1),
 UNIQUE(thread_id,idempotency_key),
 UNIQUE(thread_id,created_sequence)
);
CREATE INDEX IF NOT EXISTS provider_usage_thread_sequence
 ON provider_usage_records(thread_id,created_sequence);
UPDATE verification_schema SET version=1 WHERE singleton=1;
`); err != nil {
			return fmt.Errorf("migrate verification schema to version 1: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
CREATE TRIGGER IF NOT EXISTS verification_checks_no_update
 BEFORE UPDATE ON verification_checks BEGIN SELECT RAISE(ABORT,'verification checks are immutable'); END;
CREATE TRIGGER IF NOT EXISTS verification_checks_no_delete
 BEFORE DELETE ON verification_checks BEGIN SELECT RAISE(ABORT,'verification checks are immutable'); END;
CREATE TRIGGER IF NOT EXISTS provider_usage_records_no_update
 BEFORE UPDATE ON provider_usage_records BEGIN SELECT RAISE(ABORT,'provider usage records are immutable'); END;
CREATE TRIGGER IF NOT EXISTS provider_usage_records_no_delete
 BEFORE DELETE ON provider_usage_records BEGIN SELECT RAISE(ABORT,'provider usage records are immutable'); END;
`); err != nil {
		return fmt.Errorf("install verification immutability triggers: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit verification schema migration: %w", err)
	}
	return nil
}

func (store *Store) RecordVerificationCheck(ctx context.Context, input VerificationCheckInput) (VerificationCheck, error) {
	requestHash, err := validateVerificationCheck(input)
	if err != nil {
		return VerificationCheck{}, err
	}
	if err := store.EnsureVerificationSchema(ctx); err != nil {
		return VerificationCheck{}, err
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return VerificationCheck{}, fmt.Errorf("begin verification check: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if existing, found, err := queryVerificationCheck(ctx, tx, input.ThreadID, input.IdempotencyKey); err != nil {
		return VerificationCheck{}, err
	} else if found {
		existingHash, _ := hashJSON(existing.VerificationCheckInput)
		if existingHash != requestHash {
			return VerificationCheck{}, VerificationConflictError{Reason: "idempotency key was already used for different verification content"}
		}
		return existing, nil
	}
	recordID, sequence, err := nextVerificationIdentity(ctx, tx, input.ThreadID, "V-")
	if err != nil {
		return VerificationCheck{}, err
	}
	record := VerificationCheck{RecordID: recordID, CreatedSequence: sequence, VerificationCheckInput: input}
	encoded, err := json.Marshal(record)
	if err != nil {
		return VerificationCheck{}, fmt.Errorf("encode verification check: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO verification_checks(record_id,thread_id,idempotency_key,request_hash,record_json,created_sequence) VALUES(?,?,?,?,?,?)`, recordID, input.ThreadID, input.IdempotencyKey, requestHash, encoded, sequence); err != nil {
		return VerificationCheck{}, fmt.Errorf("insert verification check: %w", err)
	}
	if err := appendGitEvent(ctx, tx, input.ThreadID, sequence, verificationCheckRecordedEvent, agoprotocol.VisibilityAudit, record); err != nil {
		return VerificationCheck{}, err
	}
	if err := tx.Commit(); err != nil {
		return VerificationCheck{}, fmt.Errorf("commit verification check: %w", err)
	}
	return record, nil
}

func (store *Store) VerificationChecks(ctx context.Context, threadID string) ([]VerificationCheck, error) {
	if !boundedRequired(threadID) {
		return nil, fmt.Errorf("thread ID is required")
	}
	if err := store.EnsureVerificationSchema(ctx); err != nil {
		return nil, err
	}
	rows, err := store.db.QueryContext(ctx, `SELECT record_json,request_hash FROM verification_checks WHERE thread_id=? ORDER BY created_sequence`, threadID)
	if err != nil {
		return nil, fmt.Errorf("query verification checks: %w", err)
	}
	defer rows.Close()
	var checks []VerificationCheck
	for rows.Next() {
		var encoded []byte
		var storedHash string
		if err := rows.Scan(&encoded, &storedHash); err != nil {
			return nil, err
		}
		var check VerificationCheck
		if err := json.Unmarshal(encoded, &check); err != nil {
			return nil, fmt.Errorf("decode verification check: %w", err)
		}
		actualHash, _ := hashJSON(check.VerificationCheckInput)
		if actualHash != storedHash {
			return nil, fmt.Errorf("verification check integrity check failed")
		}
		checks = append(checks, check)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return checks, nil
}

func (store *Store) RecordProviderUsage(ctx context.Context, input ProviderUsageInput) (ProviderUsageRecord, error) {
	requestHash, err := validateProviderUsage(input)
	if err != nil {
		return ProviderUsageRecord{}, err
	}
	if err := store.EnsureVerificationSchema(ctx); err != nil {
		return ProviderUsageRecord{}, err
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return ProviderUsageRecord{}, fmt.Errorf("begin provider usage: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if existing, found, err := queryProviderUsage(ctx, tx, input.ThreadID, input.IdempotencyKey); err != nil {
		return ProviderUsageRecord{}, err
	} else if found {
		existingHash, _ := hashJSON(existing.ProviderUsageInput)
		if existingHash != requestHash {
			return ProviderUsageRecord{}, VerificationConflictError{Reason: "idempotency key was already used for different provider usage content"}
		}
		return existing, nil
	}
	recordID, sequence, err := nextVerificationIdentity(ctx, tx, input.ThreadID, "U-")
	if err != nil {
		return ProviderUsageRecord{}, err
	}
	record := ProviderUsageRecord{RecordID: recordID, CreatedSequence: sequence, ProviderUsageInput: input}
	encoded, err := json.Marshal(record)
	if err != nil {
		return ProviderUsageRecord{}, fmt.Errorf("encode provider usage: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO provider_usage_records(record_id,thread_id,idempotency_key,request_hash,record_json,created_sequence) VALUES(?,?,?,?,?,?)`, recordID, input.ThreadID, input.IdempotencyKey, requestHash, encoded, sequence); err != nil {
		return ProviderUsageRecord{}, fmt.Errorf("insert provider usage: %w", err)
	}
	if err := appendGitEvent(ctx, tx, input.ThreadID, sequence, providerUsageRecordedEvent, agoprotocol.VisibilityAudit, record); err != nil {
		return ProviderUsageRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return ProviderUsageRecord{}, fmt.Errorf("commit provider usage: %w", err)
	}
	return record, nil
}

func (store *Store) ProviderUsageLedger(ctx context.Context, threadID string) ([]ProviderUsageRecord, error) {
	if !boundedRequired(threadID) {
		return nil, fmt.Errorf("thread ID is required")
	}
	if err := store.EnsureVerificationSchema(ctx); err != nil {
		return nil, err
	}
	rows, err := store.db.QueryContext(ctx, `SELECT record_json,request_hash FROM provider_usage_records WHERE thread_id=? ORDER BY created_sequence`, threadID)
	if err != nil {
		return nil, fmt.Errorf("query provider usage ledger: %w", err)
	}
	defer rows.Close()
	var records []ProviderUsageRecord
	for rows.Next() {
		var encoded []byte
		var storedHash string
		if err := rows.Scan(&encoded, &storedHash); err != nil {
			return nil, err
		}
		var record ProviderUsageRecord
		if err := json.Unmarshal(encoded, &record); err != nil {
			return nil, fmt.Errorf("decode provider usage: %w", err)
		}
		actualHash, _ := hashJSON(record.ProviderUsageInput)
		if actualHash != storedHash {
			return nil, fmt.Errorf("provider usage integrity check failed")
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return records, nil
}

func (store *Store) ProviderUsageProjection(ctx context.Context, threadID string) (ProviderUsageProjection, error) {
	records, err := store.ProviderUsageLedger(ctx, threadID)
	if err != nil {
		return ProviderUsageProjection{}, err
	}
	projection := ProviderUsageProjection{ThreadID: threadID, Provisional: zeroProviderAggregate(), Final: zeroProviderAggregate()}
	for _, record := range records {
		target := &projection.Provisional
		if record.Status == UsageFinal {
			target = &projection.Final
		}
		if err := addProviderUsage(target, record); err != nil {
			return ProviderUsageProjection{}, err
		}
	}
	return projection, nil
}

func validateVerificationCheck(input VerificationCheckInput) (string, error) {
	if !boundedRequired(input.ThreadID) || !boundedRequired(input.IdempotencyKey) || !boundedRequired(input.CheckID) {
		return "", fmt.Errorf("verification thread, idempotency key, and check ID are required")
	}
	if input.Command == "" || len(input.Command) > maxVerificationCommandBytes {
		return "", fmt.Errorf("verification command must contain 1..%d bytes", maxVerificationCommandBytes)
	}
	if input.OutputSummary == "" || len(input.OutputSummary) > maxVerificationSummaryBytes {
		return "", fmt.Errorf("verification output summary must contain 1..%d bytes", maxVerificationSummaryBytes)
	}
	switch input.Status {
	case VerificationPassed, VerificationFailed, VerificationUnknown:
	default:
		return "", fmt.Errorf("unsupported verification status %q", input.Status)
	}
	return hashJSON(input)
}

func validateProviderUsage(input ProviderUsageInput) (string, error) {
	if !boundedRequired(input.ThreadID) || !boundedRequired(input.IdempotencyKey) || !boundedRequired(input.Provider) || !boundedRequired(input.Model) || !boundedRequired(input.RequestID) {
		return "", fmt.Errorf("provider usage identity is required")
	}
	if input.Status != UsageProvisional && input.Status != UsageFinal {
		return "", fmt.Errorf("unsupported provider usage status %q", input.Status)
	}
	for name, value := range map[string]json.Number{
		"input_tokens": input.Usage.InputTokens, "output_tokens": input.Usage.OutputTokens,
		"cache_read_tokens": input.Usage.CacheReadTokens, "cache_write_tokens": input.Usage.CacheWriteTokens,
		"total_tokens": input.Usage.TotalTokens,
	} {
		if !validProviderInteger(value) {
			return "", fmt.Errorf("%s must be a finite nonnegative integer", name)
		}
	}
	for name, value := range map[string]json.Number{"cache_write_1h_tokens": input.Usage.CacheWrite1HTokens, "reasoning_tokens": input.Usage.ReasoningTokens} {
		if value != "" && !validProviderInteger(value) {
			return "", fmt.Errorf("%s must be a finite nonnegative integer", name)
		}
	}
	for name, value := range map[string]json.Number{
		"input_cost": input.Cost.Input, "output_cost": input.Cost.Output, "cache_read_cost": input.Cost.CacheRead,
		"cache_write_cost": input.Cost.CacheWrite, "total_cost": input.Cost.Total,
	} {
		if !validProviderDecimal(value) {
			return "", fmt.Errorf("%s must be a finite nonnegative decimal", name)
		}
	}
	return hashJSON(input)
}

var (
	providerIntegerPattern = regexp.MustCompile(`^(?:0|[1-9][0-9]*)$`)
	providerDecimalPattern = regexp.MustCompile(`^(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?(?:0|[1-9][0-9]*))?$`)
)

func validProviderInteger(value json.Number) bool {
	return len(value) <= maxProviderNumberBytes && providerIntegerPattern.MatchString(string(value))
}

func validProviderDecimal(value json.Number) bool {
	return len(value) <= maxProviderNumberBytes && providerDecimalPattern.MatchString(string(value))
}

func hashJSON(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode immutable ledger request: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

type verificationQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func queryVerificationCheck(ctx context.Context, queryer verificationQueryer, threadID, key string) (VerificationCheck, bool, error) {
	var encoded []byte
	var storedHash string
	err := queryer.QueryRowContext(ctx, `SELECT record_json,request_hash FROM verification_checks WHERE thread_id=? AND idempotency_key=?`, threadID, key).Scan(&encoded, &storedHash)
	if errors.Is(err, sql.ErrNoRows) {
		return VerificationCheck{}, false, nil
	}
	if err != nil {
		return VerificationCheck{}, false, fmt.Errorf("read verification retry: %w", err)
	}
	var record VerificationCheck
	if err := json.Unmarshal(encoded, &record); err != nil {
		return VerificationCheck{}, false, fmt.Errorf("decode verification retry: %w", err)
	}
	actualHash, _ := hashJSON(record.VerificationCheckInput)
	if actualHash != storedHash {
		return VerificationCheck{}, false, fmt.Errorf("verification check integrity check failed")
	}
	return record, true, nil
}

func queryProviderUsage(ctx context.Context, queryer verificationQueryer, threadID, key string) (ProviderUsageRecord, bool, error) {
	var encoded []byte
	var storedHash string
	err := queryer.QueryRowContext(ctx, `SELECT record_json,request_hash FROM provider_usage_records WHERE thread_id=? AND idempotency_key=?`, threadID, key).Scan(&encoded, &storedHash)
	if errors.Is(err, sql.ErrNoRows) {
		return ProviderUsageRecord{}, false, nil
	}
	if err != nil {
		return ProviderUsageRecord{}, false, fmt.Errorf("read provider usage retry: %w", err)
	}
	var record ProviderUsageRecord
	if err := json.Unmarshal(encoded, &record); err != nil {
		return ProviderUsageRecord{}, false, fmt.Errorf("decode provider usage retry: %w", err)
	}
	actualHash, _ := hashJSON(record.ProviderUsageInput)
	if actualHash != storedHash {
		return ProviderUsageRecord{}, false, fmt.Errorf("provider usage integrity check failed")
	}
	return record, true, nil
}

func nextVerificationIdentity(ctx context.Context, tx *sql.Tx, threadID, prefix string) (string, uint64, error) {
	recordID, err := randomID(prefix)
	if err != nil {
		return "", 0, err
	}
	var sequence uint64
	if err := tx.QueryRowContext(ctx, `SELECT last_sequence+1 FROM threads WHERE thread_id=?`, threadID).Scan(&sequence); errors.Is(err, sql.ErrNoRows) {
		return "", 0, fmt.Errorf("thread %q does not exist", threadID)
	} else if err != nil {
		return "", 0, fmt.Errorf("read verification thread sequence: %w", err)
	}
	return recordID, sequence, nil
}

func zeroProviderAggregate() ProviderUsageAggregate {
	return ProviderUsageAggregate{
		Usage: ProviderTokenUsage{InputTokens: "0", OutputTokens: "0", CacheReadTokens: "0", CacheWriteTokens: "0", TotalTokens: "0"},
		Cost:  ProviderCost{Input: "0", Output: "0", CacheRead: "0", CacheWrite: "0", Total: "0"},
	}
}

func addProviderUsage(target *ProviderUsageAggregate, record ProviderUsageRecord) error {
	target.RecordCount++
	usageTargets := []*json.Number{&target.Usage.InputTokens, &target.Usage.OutputTokens, &target.Usage.CacheReadTokens, &target.Usage.CacheWriteTokens, &target.Usage.TotalTokens}
	usageValues := []json.Number{record.Usage.InputTokens, record.Usage.OutputTokens, record.Usage.CacheReadTokens, record.Usage.CacheWriteTokens, record.Usage.TotalTokens}
	for index := range usageTargets {
		sum, err := addExactDecimal(*usageTargets[index], usageValues[index])
		if err != nil {
			return err
		}
		*usageTargets[index] = sum
	}
	costTargets := []*json.Number{&target.Cost.Input, &target.Cost.Output, &target.Cost.CacheRead, &target.Cost.CacheWrite, &target.Cost.Total}
	costValues := []json.Number{record.Cost.Input, record.Cost.Output, record.Cost.CacheRead, record.Cost.CacheWrite, record.Cost.Total}
	for index := range costTargets {
		sum, err := addExactDecimal(*costTargets[index], costValues[index])
		if err != nil {
			return err
		}
		*costTargets[index] = sum
	}
	return nil
}

func addExactDecimal(left, right json.Number) (json.Number, error) {
	a, ok := new(big.Rat).SetString(string(left))
	if !ok {
		return "", fmt.Errorf("invalid stored decimal %q", left)
	}
	b, ok := new(big.Rat).SetString(string(right))
	if !ok {
		return "", fmt.Errorf("invalid stored decimal %q", right)
	}
	sum := new(big.Rat).Add(a, b)
	denominator := new(big.Int).Set(sum.Denom())
	two, five := big.NewInt(2), big.NewInt(5)
	var twos, fives int
	zero := big.NewInt(0)
	for new(big.Int).Mod(denominator, two).Cmp(zero) == 0 {
		denominator.Div(denominator, two)
		twos++
	}
	for new(big.Int).Mod(denominator, five).Cmp(zero) == 0 {
		denominator.Div(denominator, five)
		fives++
	}
	if denominator.Cmp(big.NewInt(1)) != 0 {
		return "", fmt.Errorf("stored decimal does not have a finite expansion")
	}
	places := twos
	if fives > places {
		places = fives
	}
	value := sum.FloatString(places)
	if strings.Contains(value, ".") {
		value = strings.TrimRight(strings.TrimRight(value, "0"), ".")
	}
	return json.Number(value), nil
}
