package mcpserver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Durable open-route index so claudex --resume (new MCP process) can still
// record_route_outcome for route_ids created before the process exit.
// Terminal outcomes remain on the append-only ledger; open routes live here.

type openRoutesFile struct {
	UpdatedAt string                  `json:"updated_at"`
	Routes    map[string]RouteRecord  `json:"routes"`
}

func defaultOpenRoutesPath() string {
	if value := strings.TrimSpace(os.Getenv("CLAUDEX_OPEN_ROUTES_PATH")); value != "" {
		return value
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "claudex", "open-routes.json")
}

func (s *Server) openRoutesPath() string {
	if s == nil {
		return defaultOpenRoutesPath()
	}
	if p := strings.TrimSpace(s.openRoutesPathOverride); p != "" {
		return p
	}
	return defaultOpenRoutesPath()
}

func (s *Server) loadOpenRoutesIntoMemory() {
	path := s.openRoutesPath()
	if path == "" {
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var file openRoutesFile
	if json.Unmarshal(raw, &file) != nil || file.Routes == nil {
		return
	}
	if s.routes == nil {
		s.routes = map[string]*RouteRecord{}
	}
	for id, rec := range file.Routes {
		if id == "" || rec.State != "open" {
			continue
		}
		if _, exists := s.routes[id]; exists {
			continue
		}
		cp := rec
		s.routes[id] = &cp
	}
}

func (s *Server) persistOpenRoute(record RouteRecord) {
	path := s.openRoutesPath()
	if path == "" || record.RouteID == "" {
		return
	}
	_ = withOpenRoutesLock(path, func(file *openRoutesFile) error {
		if file.Routes == nil {
			file.Routes = map[string]RouteRecord{}
		}
		if record.State == "open" {
			file.Routes[record.RouteID] = record
		} else {
			delete(file.Routes, record.RouteID)
		}
		file.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		return nil
	})
}

func (s *Server) dropOpenRoute(routeID string) {
	path := s.openRoutesPath()
	if path == "" || routeID == "" {
		return
	}
	_ = withOpenRoutesLock(path, func(file *openRoutesFile) error {
		if file.Routes != nil {
			delete(file.Routes, routeID)
		}
		file.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		return nil
	})
}

func withOpenRoutesLock(path string, fn func(*openRoutesFile) error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	lockPath := path + ".lock"
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	// Best-effort exclusive section via O_EXCL temp write; flock may be unavailable
	// on some FS — use atomic rename for the payload regardless.
	file := openRoutesFile{Routes: map[string]RouteRecord{}}
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &file)
		if file.Routes == nil {
			file.Routes = map[string]RouteRecord{}
		}
	}
	if err := fn(&file); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".open-routes-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}
