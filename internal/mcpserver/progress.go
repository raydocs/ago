package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type progressEvent struct {
	Timestamp string `json:"timestamp"`
	Kind      string `json:"kind"`
	WorkerID  string `json:"worker_id"`
	Model     string `json:"model"`
	Message   string `json:"message"`
	ElapsedMS int64  `json:"elapsed_ms"`
	RootID    string `json:"root_session_id,omitempty"`
	ParentID  string `json:"parent_session_id,omitempty"`
}

type workerProgress struct {
	ctx              context.Context
	req              *mcp.CallToolRequest
	id, model        string
	start            time.Time
	stop, done       chan struct{}
	once             sync.Once
	rootID, parentID string
}

func (s *Server) beginWorkerProgress(ctx context.Context, req *mcp.CallToolRequest, id, model string) *workerProgress {
	rootID, parentID := s.threadBinding()
	p := &workerProgress{ctx: ctx, req: req, id: id, model: model, start: time.Now(), stop: make(chan struct{}), done: make(chan struct{}), rootID: rootID, parentID: parentID}
	p.emit(1, "Worker started; waiting for first model/tool result")
	go func() {
		defer close(p.done)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				elapsed := time.Since(p.start)
				value := float64(1 + int(elapsed/(5*time.Second)))
				if value > 90 {
					value = 90
				}
				p.emit(value, fmt.Sprintf("Worker running · %ds elapsed", int(elapsed.Seconds())))
			case <-p.stop:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	return p
}

func (p *workerProgress) finish(message string) {
	p.once.Do(func() {
		close(p.stop)
		<-p.done
		p.emit(100, message)
	})
}

func (p *workerProgress) emit(value float64, message string) {
	if p.req != nil && p.req.Params != nil && p.req.Session != nil {
		if token := p.req.Params.GetProgressToken(); token != nil {
			_ = p.req.Session.NotifyProgress(p.ctx, &mcp.ProgressNotificationParams{ProgressToken: token, Progress: value, Total: 100, Message: message})
		}
	}
	appendProgressEvent(progressEvent{Timestamp: time.Now().UTC().Format(time.RFC3339Nano), Kind: "worker_progress", WorkerID: p.id, Model: p.model, Message: message, ElapsedMS: time.Since(p.start).Milliseconds(), RootID: p.rootID, ParentID: p.parentID})
}

func appendProgressEvent(event progressEvent) {
	dir := os.Getenv("CLAUDEX_PROGRESS_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return
		}
		dir = filepath.Join(home, ".config", "claudex", "progress")
	}
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	raw, err := json.Marshal(event)
	if err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, "events.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(raw, '\n'))
}
