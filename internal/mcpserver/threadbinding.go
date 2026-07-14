package mcpserver

import (
	"os"
	"strings"

	"claudexflow/internal/sessionbind"
)

// threadBinding resolves lazily because SessionStart hooks may record the
// active Claude process after the MCP server itself has already launched.
func (s *Server) threadBinding() (rootSessionID, parentSessionID string) {
	rootSessionID = strings.TrimSpace(os.Getenv("CLAUDEX_THREAD_ROOT_SESSION_ID"))
	parentSessionID = strings.TrimSpace(os.Getenv("CLAUDEX_THREAD_PARENT_SESSION_ID"))
	if rootSessionID == "" {
		if binding, ok := sessionbind.Resolve(s.parentPID, s.root); ok {
			rootSessionID = binding.SessionID
		}
	}
	if parentSessionID == "" {
		parentSessionID = rootSessionID
	}
	return rootSessionID, parentSessionID
}
