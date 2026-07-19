package agorelay

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strings"
	"sync"

	"claudexflow/internal/agobridge"
	_ "modernc.org/sqlite"
)

const (
	RoleDaemon  = "daemon"
	RoleBrowser = "browser"

	ActionProjection    = "thread.projection"
	ActionSubmit        = "thread.submit"
	ActionArchive       = "thread.archive"
	ActionAuthChallenge = "auth.challenge"
	ActionAuthAssertion = "auth.assertion"

	maxStoredPayloadBytes = 512 << 10
)

var (
	ErrUnauthorized = errors.New("agorelay: unauthorized")
	ErrConflict     = errors.New("agorelay: conflict")
	ErrInvalid      = errors.New("agorelay: invalid request")
	ErrPending      = errors.New("agorelay: result pending")
	ErrNotFound     = errors.New("agorelay: not found")
)

type Store struct {
	db      *sql.DB
	writeMu sync.Mutex
}

type Credential struct {
	AccountID  string   `json:"account_id"`
	DeviceID   string   `json:"device_id"`
	Role       string   `json:"role"`
	Generation uint64   `json:"generation"`
	Token      string   `json:"token"`
	Projects   []string `json:"projects"`
}

type Principal struct {
	AccountID  string
	DeviceID   string
	Role       string
	Generation uint64
	Projects   map[string]struct{}
}

type EnqueueRequest struct {
	Nonce              string          `json:"nonce"`
	ProjectID          string          `json:"project_id"`
	ThreadID           string          `json:"thread_id"`
	Action             string          `json:"action"`
	AuthorizationToken string          `json:"authorization_token,omitempty"`
	Payload            json.RawMessage `json:"payload"`
}

type EnqueueResult struct {
	Sequence uint64 `json:"sequence"`
	Nonce    string `json:"nonce"`
}

type Result struct {
	Sequence uint64                      `json:"sequence"`
	Pending  bool                        `json:"pending"`
	Response *agobridge.ResponseEnvelope `json:"response,omitempty"`
}

func Open(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("agorelay: database path is required")
	}
	info, err := os.Lstat(path)
	if err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
			return nil, errors.New("agorelay: database must be a private regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	} else {
		file, createErr := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if createErr != nil {
			return nil, createErr
		}
		if closeErr := file.Close(); closeErr != nil {
			return nil, closeErr
		}
	}
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "_txlock=immediate&_pragma=busy_timeout(5000)"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(16)
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; PRAGMA busy_timeout=5000; PRAGMA foreign_keys=ON;`); err != nil {
		db.Close()
		return nil, err
	}
	schema := `
CREATE TABLE IF NOT EXISTS credentials (
 account_id TEXT NOT NULL, device_id TEXT NOT NULL, role TEXT NOT NULL, generation INTEGER NOT NULL,
 token_hash TEXT NOT NULL UNIQUE, projects_json BLOB NOT NULL, active INTEGER NOT NULL,
 PRIMARY KEY(account_id,device_id,role,generation)
);
CREATE INDEX IF NOT EXISTS credentials_active_hash ON credentials(token_hash,role,active);
CREATE TABLE IF NOT EXISTS device_state (
 account_id TEXT NOT NULL, device_id TEXT NOT NULL, cursor INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(account_id,device_id)
);
CREATE TABLE IF NOT EXISTS sequence_counters (
 account_id TEXT NOT NULL, device_id TEXT NOT NULL, last_sequence INTEGER NOT NULL,
 PRIMARY KEY(account_id,device_id)
);
CREATE TABLE IF NOT EXISTS requests (
 account_id TEXT NOT NULL, device_id TEXT NOT NULL, sequence INTEGER NOT NULL, nonce TEXT NOT NULL,
 project_id TEXT NOT NULL, thread_id TEXT NOT NULL, action TEXT NOT NULL, authorization_token TEXT NOT NULL,
 payload BLOB NOT NULL, request_digest TEXT NOT NULL, response_json BLOB,
 PRIMARY KEY(account_id,device_id,sequence), UNIQUE(account_id,device_id,nonce)
);
`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	_ = os.Chmod(path, 0o600)
	return &Store{db: db}, nil
}

func (store *Store) Close() error { return store.db.Close() }

func (store *Store) RotateCredential(ctx context.Context, credential Credential) error {
	store.writeMu.Lock()
	defer store.writeMu.Unlock()
	if !validID(credential.AccountID) || !validID(credential.DeviceID) ||
		(credential.Role != RoleDaemon && credential.Role != RoleBrowser) || credential.Generation == 0 ||
		len(credential.Token) < 3 || len(credential.Token) > 8192 || len(credential.Projects) == 0 {
		return ErrInvalid
	}
	projects := make(map[string]struct{}, len(credential.Projects))
	for _, project := range credential.Projects {
		if !validID(project) {
			return ErrInvalid
		}
		if _, exists := projects[project]; exists {
			return ErrInvalid
		}
		projects[project] = struct{}{}
	}
	projectsJSON, _ := json.Marshal(credential.Projects)
	hash := tokenHash(credential.Token)
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var current sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT MAX(generation) FROM credentials WHERE account_id=? AND device_id=? AND role=?`, credential.AccountID, credential.DeviceID, credential.Role).Scan(&current); err != nil {
		return err
	}
	if current.Valid && credential.Generation <= uint64(current.Int64) {
		if credential.Generation != uint64(current.Int64) {
			return ErrConflict
		}
		var existingHash string
		var existingProjects []byte
		var active int
		err := tx.QueryRowContext(ctx, `SELECT token_hash,projects_json,active FROM credentials WHERE account_id=? AND device_id=? AND role=? AND generation=?`, credential.AccountID, credential.DeviceID, credential.Role, credential.Generation).Scan(&existingHash, &existingProjects, &active)
		if err != nil || active != 1 || existingHash != hash || !bytes.Equal(existingProjects, projectsJSON) {
			return ErrConflict
		}
		return nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE credentials SET active=0 WHERE account_id=? AND device_id=? AND role=?`, credential.AccountID, credential.DeviceID, credential.Role); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO credentials(account_id,device_id,role,generation,token_hash,projects_json,active) VALUES(?,?,?,?,?,?,1)`, credential.AccountID, credential.DeviceID, credential.Role, credential.Generation, hash, projectsJSON); err != nil {
		return err
	}
	return tx.Commit()
}

func (store *Store) Authenticate(ctx context.Context, token, role string) (Principal, error) {
	if token == "" || (role != RoleDaemon && role != RoleBrowser) {
		return Principal{}, ErrUnauthorized
	}
	var principal Principal
	var projectsJSON []byte
	err := store.db.QueryRowContext(ctx, `SELECT account_id,device_id,role,generation,projects_json FROM credentials WHERE token_hash=? AND role=? AND active=1`, tokenHash(token), role).Scan(&principal.AccountID, &principal.DeviceID, &principal.Role, &principal.Generation, &projectsJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return Principal{}, ErrUnauthorized
	}
	if err != nil {
		return Principal{}, err
	}
	var projects []string
	if err := json.Unmarshal(projectsJSON, &projects); err != nil {
		return Principal{}, errors.New("agorelay: corrupt credential projects")
	}
	principal.Projects = make(map[string]struct{}, len(projects))
	for _, project := range projects {
		if !validID(project) {
			return Principal{}, errors.New("agorelay: corrupt credential project")
		}
		principal.Projects[project] = struct{}{}
	}
	return principal, nil
}

func (store *Store) Enqueue(ctx context.Context, principal Principal, request EnqueueRequest) (EnqueueResult, error) {
	store.writeMu.Lock()
	defer store.writeMu.Unlock()
	if principal.Role != RoleBrowser || !validEnqueue(request) {
		return EnqueueResult{}, ErrInvalid
	}
	if _, ok := principal.Projects[request.ProjectID]; !ok {
		return EnqueueResult{}, ErrUnauthorized
	}
	digest := enqueueDigest(request)
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return EnqueueResult{}, err
	}
	defer tx.Rollback()
	var existingSequence uint64
	var existingDigest string
	err = tx.QueryRowContext(ctx, `SELECT sequence,request_digest FROM requests WHERE account_id=? AND device_id=? AND nonce=?`, principal.AccountID, principal.DeviceID, request.Nonce).Scan(&existingSequence, &existingDigest)
	if err == nil {
		if existingDigest != digest {
			return EnqueueResult{}, ErrConflict
		}
		return EnqueueResult{Sequence: existingSequence, Nonce: request.Nonce}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return EnqueueResult{}, err
	}
	var sequence uint64
	err = tx.QueryRowContext(ctx, `INSERT INTO sequence_counters(account_id,device_id,last_sequence) VALUES(?,?,1)
ON CONFLICT(account_id,device_id) DO UPDATE SET last_sequence=last_sequence+1 RETURNING last_sequence`, principal.AccountID, principal.DeviceID).Scan(&sequence)
	if err != nil {
		return EnqueueResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO requests(account_id,device_id,sequence,nonce,project_id,thread_id,action,authorization_token,payload,request_digest) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		principal.AccountID, principal.DeviceID, sequence, request.Nonce, request.ProjectID, request.ThreadID, request.Action, request.AuthorizationToken, []byte(request.Payload), digest); err != nil {
		return EnqueueResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO device_state(account_id,device_id,cursor) VALUES(?,?,0) ON CONFLICT DO NOTHING`, principal.AccountID, principal.DeviceID); err != nil {
		return EnqueueResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return EnqueueResult{}, err
	}
	return EnqueueResult{Sequence: sequence, Nonce: request.Nonce}, nil
}

func (store *Store) Poll(ctx context.Context, principal Principal, poll agobridge.PollEnvelope) (agobridge.PollResult, error) {
	store.writeMu.Lock()
	defer store.writeMu.Unlock()
	if principal.Role != RoleDaemon || poll.AccountID != principal.AccountID || poll.DeviceID != principal.DeviceID || len(poll.Responses) > 1 {
		return agobridge.PollResult{}, ErrUnauthorized
	}
	if len(poll.Responses) == 1 {
		encoded, err := json.Marshal(poll.Responses[0])
		if err != nil || len(encoded) > maxStoredPayloadBytes {
			return agobridge.PollResult{}, ErrInvalid
		}
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return agobridge.PollResult{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO device_state(account_id,device_id,cursor) VALUES(?,?,0) ON CONFLICT DO NOTHING`, principal.AccountID, principal.DeviceID); err != nil {
		return agobridge.PollResult{}, err
	}
	var cursor uint64
	if err := tx.QueryRowContext(ctx, `SELECT cursor FROM device_state WHERE account_id=? AND device_id=?`, principal.AccountID, principal.DeviceID).Scan(&cursor); err != nil {
		return agobridge.PollResult{}, err
	}
	if len(poll.Responses) == 0 {
		if poll.Cursor != cursor {
			return agobridge.PollResult{}, ErrConflict
		}
	} else {
		response := poll.Responses[0]
		if response.AccountID != principal.AccountID || response.DeviceID != principal.DeviceID || response.Sequence != poll.Cursor ||
			(poll.Cursor != cursor && poll.Cursor != cursor+1) {
			return agobridge.PollResult{}, ErrConflict
		}
		var nonce string
		var existing []byte
		if err := tx.QueryRowContext(ctx, `SELECT nonce,response_json FROM requests WHERE account_id=? AND device_id=? AND sequence=?`, principal.AccountID, principal.DeviceID, response.Sequence).Scan(&nonce, &existing); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return agobridge.PollResult{}, ErrConflict
			}
			return agobridge.PollResult{}, err
		}
		if nonce != response.Nonce || (len(response.Payload) > 0 && !json.Valid(response.Payload)) {
			return agobridge.PollResult{}, ErrConflict
		}
		encoded, err := json.Marshal(response)
		if err != nil {
			return agobridge.PollResult{}, ErrInvalid
		}
		if len(existing) != 0 {
			if !bytes.Equal(existing, encoded) {
				return agobridge.PollResult{}, ErrConflict
			}
		} else {
			if response.Sequence != cursor+1 {
				return agobridge.PollResult{}, ErrConflict
			}
			if _, err := tx.ExecContext(ctx, `UPDATE requests SET response_json=? WHERE account_id=? AND device_id=? AND sequence=? AND response_json IS NULL`, encoded, principal.AccountID, principal.DeviceID, response.Sequence); err != nil {
				return agobridge.PollResult{}, err
			}
			cursor = response.Sequence
			if _, err := tx.ExecContext(ctx, `UPDATE device_state SET cursor=? WHERE account_id=? AND device_id=?`, cursor, principal.AccountID, principal.DeviceID); err != nil {
				return agobridge.PollResult{}, err
			}
		}
	}
	result := agobridge.PollResult{AccountID: principal.AccountID, DeviceID: principal.DeviceID, AcknowledgedThrough: cursor}
	var request agobridge.RequestEnvelope
	err = tx.QueryRowContext(ctx, `SELECT sequence,nonce,project_id,thread_id,action,authorization_token,payload FROM requests WHERE account_id=? AND device_id=? AND sequence=?`, principal.AccountID, principal.DeviceID, cursor+1).Scan(
		&request.Sequence, &request.Nonce, &request.ProjectID, &request.ThreadID, &request.Action, &request.AuthorizationToken, &request.Payload)
	if err == nil {
		if _, ok := principal.Projects[request.ProjectID]; !ok {
			return agobridge.PollResult{}, ErrUnauthorized
		}
		request.AccountID, request.DeviceID = principal.AccountID, principal.DeviceID
		result.Requests = []agobridge.RequestEnvelope{request}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return agobridge.PollResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return agobridge.PollResult{}, err
	}
	return result, nil
}

func (store *Store) Result(ctx context.Context, principal Principal, sequence uint64) (Result, error) {
	if principal.Role != RoleBrowser || sequence == 0 {
		return Result{}, ErrUnauthorized
	}
	var project string
	var encoded []byte
	err := store.db.QueryRowContext(ctx, `SELECT project_id,response_json FROM requests WHERE account_id=? AND device_id=? AND sequence=?`, principal.AccountID, principal.DeviceID, sequence).Scan(&project, &encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return Result{}, ErrNotFound
	}
	if err != nil {
		return Result{}, err
	}
	if _, ok := principal.Projects[project]; !ok {
		return Result{}, ErrUnauthorized
	}
	result := Result{Sequence: sequence, Pending: len(encoded) == 0}
	if result.Pending {
		return result, nil
	}
	var response agobridge.ResponseEnvelope
	if err := json.Unmarshal(encoded, &response); err != nil {
		return Result{}, errors.New("agorelay: corrupt response")
	}
	result.Response = &response
	return result, nil
}

func validEnqueue(request EnqueueRequest) bool {
	if !validID(request.Nonce) || !validID(request.ProjectID) || !validID(request.ThreadID) || len(request.AuthorizationToken) > 8192 ||
		len(request.Payload) == 0 || len(request.Payload) > maxStoredPayloadBytes || !json.Valid(request.Payload) {
		return false
	}
	return request.Action == ActionProjection || request.Action == ActionSubmit || request.Action == ActionArchive ||
		request.Action == ActionAuthChallenge || request.Action == ActionAuthAssertion
}

func validID(value string) bool {
	return value != "" && len(value) <= 512 && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\x00\r\n")
}

func enqueueDigest(request EnqueueRequest) string {
	encoded, _ := json.Marshal(request)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func tokenHash(token string) string {
	digest := sha256.Sum256([]byte("agorelay-token-v1\x00" + token))
	return hex.EncodeToString(digest[:])
}
