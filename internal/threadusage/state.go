package threadusage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

const stateSchema = 1

// FileCursor tracks append-only progress for one transcript path.
type FileCursor struct {
	Offset  int64 `json:"offset"`
	Size    int64 `json:"size"`
	MtimeNs int64 `json:"mtime_ns"`
}

// State is the on-disk cursor store. Paths are hashed; no message content is stored.
type State struct {
	Schema int                   `json:"schema"`
	Files  map[string]FileCursor `json:"files"`
}

var stateMu sync.Mutex

// DefaultStatePath returns ~/.config/claudex/thread-usage-state.json (or env override).
func DefaultStatePath() (string, error) {
	if path := os.Getenv("CLAUDEX_THREAD_USAGE_STATE"); path != "" {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "claudex", "thread-usage-state.json"), nil
}

// PathKey hashes a transcript path so the state file never needs to hold secrets from path names.
func PathKey(path string) string {
	sum := sha256.Sum256([]byte(path))
	return hex.EncodeToString(sum[:])
}

// LoadState reads the cursor state file. Missing files yield an empty state.
func LoadState(path string) (State, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return State{Schema: stateSchema, Files: map[string]FileCursor{}}, nil
		}
		return State{}, err
	}
	var st State
	if err := json.Unmarshal(raw, &st); err != nil {
		return State{}, fmt.Errorf("parse usage state %s: %w", path, err)
	}
	if st.Files == nil {
		st.Files = map[string]FileCursor{}
	}
	if st.Schema == 0 {
		st.Schema = stateSchema
	}
	return st, nil
}

// SaveState writes state atomically with 0600 permissions.
func SaveState(path string, st State) error {
	if st.Schema == 0 {
		st.Schema = stateSchema
	}
	if st.Files == nil {
		st.Files = map[string]FileCursor{}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadCursor returns the stored cursor for a transcript path.
func LoadCursor(statePath, transcriptPath string) (FileCursor, error) {
	stateMu.Lock()
	defer stateMu.Unlock()
	st, err := LoadState(statePath)
	if err != nil {
		return FileCursor{}, err
	}
	return st.Files[PathKey(transcriptPath)], nil
}

// StoreCursor updates one transcript cursor atomically.
func StoreCursor(statePath, transcriptPath string, cursor FileCursor) error {
	stateMu.Lock()
	defer stateMu.Unlock()
	st, err := LoadState(statePath)
	if err != nil {
		return err
	}
	st.Files[PathKey(transcriptPath)] = cursor
	return SaveState(statePath, st)
}
