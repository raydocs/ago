package threadgraph

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

type state struct {
	Schema int                   `json:"schema"`
	Files  map[string]FileCursor `json:"files"`
}

var stateMu sync.Mutex

func DefaultStatePath() (string, error) {
	if path := os.Getenv("CLAUDEX_THREAD_GRAPH_STATE"); path != "" {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "claudex", "thread-graph-state.json"), nil
}

func pathKey(path string) string {
	sum := sha256.Sum256([]byte(path))
	return hex.EncodeToString(sum[:])
}

func loadCursor(statePath, transcriptPath string) (FileCursor, error) {
	stateMu.Lock()
	defer stateMu.Unlock()
	current, err := loadState(statePath)
	if err != nil {
		return FileCursor{}, err
	}
	return current.Files[pathKey(transcriptPath)], nil
}

func storeCursor(statePath, transcriptPath string, cursor FileCursor) error {
	stateMu.Lock()
	defer stateMu.Unlock()
	current, err := loadState(statePath)
	if err != nil {
		return err
	}
	current.Files[pathKey(transcriptPath)] = cursor
	return saveState(statePath, current)
}

func loadState(path string) (state, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return state{Schema: stateSchema, Files: map[string]FileCursor{}}, nil
		}
		return state{}, err
	}
	var current state
	if err := json.Unmarshal(raw, &current); err != nil {
		return state{}, fmt.Errorf("parse graph state %s: %w", path, err)
	}
	if current.Schema == 0 {
		current.Schema = stateSchema
	}
	if current.Files == nil {
		current.Files = map[string]FileCursor{}
	}
	return current, nil
}

func saveState(path string, current state) error {
	if current.Schema == 0 {
		current.Schema = stateSchema
	}
	if current.Files == nil {
		current.Files = map[string]FileCursor{}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(current, "", "  ")
	if err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}
