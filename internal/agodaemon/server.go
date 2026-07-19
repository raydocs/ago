package agodaemon

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"claudexflow/internal/agoattachments"
	"claudexflow/internal/agocoordinator"
	"claudexflow/internal/agogit"
	"claudexflow/internal/agopluginhost"
	"claudexflow/internal/agopluginprotocol"
	"claudexflow/internal/agoprotocol"
	"claudexflow/internal/agothreadstore"
	"golang.org/x/sys/unix"
)

const maxRequestBytes = 4 * 1024 * 1024

type Server struct {
	store        *agothreadstore.Store
	coordinator  *agocoordinator.Coordinator
	dialogs      DialogResolver
	plugins      PluginCommands
	gitRefresher GitRefresher
	attachments  *agoattachments.Store
}

type DialogResolver interface {
	ResolveDialog(context.Context, agothreadstore.ResolveDialogInput) (agothreadstore.PluginDialog, error)
}

type PluginCommands interface {
	PluginRegistrations(context.Context, string) (agopluginhost.Snapshot, error)
	ExecutePluginCommand(context.Context, string, string, string, any) (json.RawMessage, error)
}

// GitRefresher is the narrow daemon dependency used to durably refresh a diff.
type GitRefresher interface {
	Refresh(context.Context, agogit.RefreshInput) (agothreadstore.GitSnapshot, error)
	Mutate(context.Context, agogit.MutationInput) (agogit.MutationResult, error)
	Revert(context.Context, agogit.RevertInput) (agogit.MutationResult, error)
}

func New(store *agothreadstore.Store, coordinator *agocoordinator.Coordinator) *Server {
	return &Server{store: store, coordinator: coordinator}
}

func NewWithDialogs(store *agothreadstore.Store, coordinator *agocoordinator.Coordinator, dialogs DialogResolver) *Server {
	return &Server{store: store, coordinator: coordinator, dialogs: dialogs}
}

func NewWithRuntime(store *agothreadstore.Store, coordinator *agocoordinator.Coordinator, dialogs DialogResolver, plugins PluginCommands) *Server {
	return &Server{store: store, coordinator: coordinator, dialogs: dialogs, plugins: plugins}
}

// WithGitRefresher configures diff refresh without changing existing constructors.
func (server *Server) WithGitRefresher(refresher GitRefresher) *Server {
	server.gitRefresher = refresher
	return server
}

// WithAttachments adds immutable attachment upload and submit resolution while
// preserving all existing constructors for callers that do not configure it.
func (server *Server) WithAttachments(store *agoattachments.Store) *Server {
	server.attachments = store
	return server
}

func (server *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", server.health)
	mux.HandleFunc("POST /v1/threads", server.createThread)
	mux.HandleFunc("GET /v1/threads", server.listThreads)
	mux.HandleFunc("GET /v1/threads/{threadID}", server.thread)
	mux.HandleFunc("GET /v1/threads/{threadID}/projection", server.clientProjection)
	mux.HandleFunc("GET /v1/threads/{threadID}/events", server.events)
	mux.HandleFunc("POST /v1/threads/{threadID}/attachments", server.uploadAttachment)
	mux.HandleFunc("POST /v1/threads/{threadID}/messages", server.submit)
	mux.HandleFunc("POST /v1/threads/{threadID}/archive", server.setThreadArchived(true))
	mux.HandleFunc("POST /v1/threads/{threadID}/unarchive", server.setThreadArchived(false))
	mux.HandleFunc("POST /v1/threads/{threadID}/diff/refresh", server.refreshDiff)
	mux.HandleFunc("POST /v1/threads/{threadID}/diff/stage", server.mutateDiff(agogit.MutationStage))
	mux.HandleFunc("POST /v1/threads/{threadID}/diff/unstage", server.mutateDiff(agogit.MutationUnstage))
	mux.HandleFunc("POST /v1/threads/{threadID}/diff/revert", server.revertDiff)
	mux.HandleFunc("POST /v1/threads/{threadID}/diff/comments", server.addDiffComment)
	mux.HandleFunc("PATCH /v1/threads/{threadID}/queue/{queueItemID}", server.editQueued)
	mux.HandleFunc("DELETE /v1/threads/{threadID}/queue/{queueItemID}", server.dequeue)
	mux.HandleFunc("POST /v1/threads/{threadID}/queue/{queueItemID}/steer", server.steer)
	mux.HandleFunc("POST /v1/threads/{threadID}/turns/{turnID}/interrupt", server.interrupt)
	mux.HandleFunc("POST /v1/threads/{threadID}/turns/{turnID}/cancel", server.cancel)
	mux.HandleFunc("GET /v1/threads/{threadID}/dialogs", server.listDialogs)
	mux.HandleFunc("POST /v1/threads/{threadID}/dialogs/{dialogID}/resolve", server.resolveDialog)
	mux.HandleFunc("GET /v1/threads/{threadID}/plugins", server.listPlugins)
	mux.HandleFunc("POST /v1/threads/{threadID}/plugin-commands/{commandID}", server.executePluginCommand)
	return mux
}

// RequireBearerToken protects every route in next without reading rejected
// request bodies. It is intended for the optional loopback TCP transport; the
// private Unix-socket transport continues to use Handler directly.
func RequireBearerToken(next http.Handler, token string) (http.Handler, error) {
	if next == nil {
		return nil, fmt.Errorf("authenticated handler is required")
	}
	if err := validateBearerToken(token); err != nil {
		return nil, err
	}
	expected := sha256.Sum256([]byte("Bearer " + token))
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		values := request.Header.Values("Authorization")
		authorized := 0
		if len(values) == 1 {
			provided := sha256.Sum256([]byte(values[0]))
			authorized = subtle.ConstantTimeCompare(expected[:], provided[:])
		}
		if authorized != 1 {
			writer.Header().Set("WWW-Authenticate", fmt.Sprintf("Bearer realm=%q", "ago"))
			writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(writer, request)
	}), nil
}

func validateBearerToken(token string) error {
	if len(token) < 32 {
		return fmt.Errorf("TCP bearer token must contain at least 32 bytes")
	}
	unique := make(map[byte]struct{}, 16)
	for index := 0; index < len(token); index++ {
		if token[index] < 0x21 || token[index] > 0x7e {
			return fmt.Errorf("TCP bearer token must contain only visible ASCII without whitespace")
		}
		unique[token[index]] = struct{}{}
	}
	if len(unique) < 8 {
		return fmt.Errorf("TCP bearer token does not have sufficient entropy")
	}
	return nil
}

type addDiffCommentRequest struct {
	CommentID        string `json:"comment_id"`
	ExpectedSequence uint64 `json:"expected_sequence"`
	SnapshotRevision uint64 `json:"snapshot_revision"`
	SnapshotDigest   string `json:"snapshot_digest"`
	FileID           string `json:"file_id"`
	HunkID           string `json:"hunk_id,omitempty"`
	ActorID          string `json:"actor_id"`
	Body             string `json:"body"`
}

func (server *Server) addDiffComment(writer http.ResponseWriter, request *http.Request) {
	var input addDiffCommentRequest
	if err := decodeRequest(writer, request, &input); err != nil {
		writeError(writer, err)
		return
	}
	if input.CommentID == "" || input.ExpectedSequence == 0 || input.SnapshotRevision == 0 || input.SnapshotDigest == "" || input.FileID == "" || input.ActorID == "" || strings.TrimSpace(input.Body) == "" {
		writeError(writer, fmt.Errorf("complete snapshot-fenced diff comment is required"))
		return
	}
	if existing, found, err := server.store.FindGitComment(request.Context(), request.PathValue("threadID"), input.CommentID); err != nil {
		writeError(writer, err)
		return
	} else if found {
		if existing.SnapshotRevision != input.SnapshotRevision || existing.SnapshotDigest != input.SnapshotDigest || existing.FileID != input.FileID || existing.HunkID != input.HunkID || existing.Actor != input.ActorID || existing.Body != input.Body {
			writeError(writer, agothreadstore.GitCommentConflictError{Reason: "comment_id already belongs to a different request"})
			return
		}
		writeJSON(writer, http.StatusAccepted, existing)
		return
	}
	projection, err := server.store.ClientProjection(request.Context(), request.PathValue("threadID"), input.ExpectedSequence, 1)
	if err != nil {
		writeError(writer, err)
		return
	}
	if projection.SnapshotSequence != input.ExpectedSequence {
		writeError(writer, agothreadstore.ConflictError{CurrentSequence: projection.SnapshotSequence, ExpectedSequence: input.ExpectedSequence})
		return
	}
	if projection.Diff.Snapshot == nil || projection.Diff.Snapshot.Revision != input.SnapshotRevision || projection.Diff.Snapshot.Digest != input.SnapshotDigest {
		writeError(writer, fmt.Errorf("snapshot identity mismatch"))
		return
	}
	comment, err := server.store.AddGitComment(request.Context(), agothreadstore.GitCommentInput{
		ThreadID: request.PathValue("threadID"), CommentID: input.CommentID,
		ExpectedSequence:   &input.ExpectedSequence,
		SnapshotGeneration: projection.Diff.Snapshot.ExecutorGeneration, SnapshotRevision: input.SnapshotRevision,
		SnapshotDigest: input.SnapshotDigest, FileID: input.FileID, HunkID: input.HunkID, Actor: input.ActorID, Body: input.Body,
	})
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusAccepted, comment)
}

type revertDiffRequest struct {
	CommandID                string `json:"command_id"`
	IdempotencyKey           string `json:"idempotency_key"`
	ActorID                  string `json:"actor_id"`
	ExpectedSequence         uint64 `json:"expected_sequence"`
	ExpectedSnapshotRevision uint64 `json:"expected_snapshot_revision"`
	ExpectedSnapshotDigest   string `json:"expected_snapshot_digest"`
	ReceiptID                string `json:"receipt_id"`
}

func (server *Server) revertDiff(writer http.ResponseWriter, request *http.Request) {
	var input revertDiffRequest
	if err := decodeRequest(writer, request, &input); err != nil {
		writeError(writer, err)
		return
	}
	if input.CommandID == "" || input.IdempotencyKey == "" || input.ActorID == "" || input.ExpectedSequence == 0 || input.ExpectedSnapshotRevision == 0 || input.ExpectedSnapshotDigest == "" || input.ReceiptID == "" {
		writeError(writer, fmt.Errorf("complete receipt revert identity is required"))
		return
	}
	if server.gitRefresher == nil {
		writeError(writer, fmt.Errorf("git receipt revert is unavailable"))
		return
	}
	thread, err := server.store.Thread(request.Context(), request.PathValue("threadID"))
	if err != nil {
		writeError(writer, err)
		return
	}
	result, err := server.gitRefresher.Revert(request.Context(), agogit.RevertInput{
		ThreadID: thread.ThreadID, Workspace: thread.Workspace, Executor: thread.Executor,
		EnvironmentID: "thread:" + thread.ThreadID, ExecutorGeneration: initialExecutorGeneration,
		ActorID: input.ActorID, IdempotencyKey: input.IdempotencyKey, CommandID: input.CommandID,
		ExpectedSequence: input.ExpectedSequence, ExpectedSnapshotRevision: input.ExpectedSnapshotRevision,
		ExpectedSnapshotDigest: input.ExpectedSnapshotDigest, ReceiptID: input.ReceiptID,
	})
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

type mutateDiffRequest struct {
	CommandID                string   `json:"command_id"`
	IdempotencyKey           string   `json:"idempotency_key"`
	ActorID                  string   `json:"actor_id"`
	ExpectedSequence         uint64   `json:"expected_sequence"`
	ExpectedSnapshotRevision uint64   `json:"expected_snapshot_revision"`
	ExpectedSnapshotDigest   string   `json:"expected_snapshot_digest"`
	SelectedUnitIDs          []string `json:"selected_unit_ids"`
}

func (server *Server) mutateDiff(kind agogit.MutationKind) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		var input mutateDiffRequest
		if err := decodeRequest(writer, request, &input); err != nil {
			writeError(writer, err)
			return
		}
		if input.CommandID == "" || input.IdempotencyKey == "" || input.ActorID == "" || input.ExpectedSequence == 0 || input.ExpectedSnapshotRevision == 0 || input.ExpectedSnapshotDigest == "" || len(input.SelectedUnitIDs) == 0 {
			writeError(writer, fmt.Errorf("complete Git mutation identity is required"))
			return
		}
		if server.gitRefresher == nil {
			writeError(writer, fmt.Errorf("git mutation is unavailable"))
			return
		}
		thread, err := server.store.Thread(request.Context(), request.PathValue("threadID"))
		if err != nil {
			writeError(writer, err)
			return
		}
		result, err := server.gitRefresher.Mutate(request.Context(), agogit.MutationInput{
			ThreadID: thread.ThreadID, Workspace: thread.Workspace, Executor: thread.Executor,
			EnvironmentID: "thread:" + thread.ThreadID, ExecutorGeneration: initialExecutorGeneration,
			ActorID: input.ActorID, IdempotencyKey: input.IdempotencyKey, CommandID: input.CommandID,
			ExpectedSequence:         input.ExpectedSequence,
			ExpectedSnapshotRevision: input.ExpectedSnapshotRevision, ExpectedSnapshotDigest: input.ExpectedSnapshotDigest,
			Kind: kind, SelectedUnitIDs: input.SelectedUnitIDs,
		})
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, result)
	}
}

type refreshDiffRequest struct {
	IdempotencyKey string `json:"idempotency_key"`
}

const initialExecutorGeneration uint64 = 1

func (server *Server) refreshDiff(writer http.ResponseWriter, request *http.Request) {
	var input refreshDiffRequest
	if err := decodeRequest(writer, request, &input); err != nil {
		writeError(writer, err)
		return
	}
	if strings.TrimSpace(input.IdempotencyKey) == "" {
		writeError(writer, fmt.Errorf("idempotency_key is required"))
		return
	}
	if server.gitRefresher == nil {
		writeError(writer, fmt.Errorf("git refresh is unavailable"))
		return
	}
	thread, err := server.store.Thread(request.Context(), request.PathValue("threadID"))
	if err != nil {
		writeError(writer, err)
		return
	}
	// Environment identity remains daemon-owned until executor generations are
	// represented durably; clients cannot select or override either value.
	snapshot, err := server.gitRefresher.Refresh(request.Context(), agogit.RefreshInput{
		ThreadID: thread.ThreadID, Workspace: thread.Workspace, Executor: thread.Executor,
		EnvironmentID: "thread:" + thread.ThreadID, ExecutorGeneration: initialExecutorGeneration,
		IdempotencyKey: input.IdempotencyKey,
	})
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusAccepted, snapshot)
}

type pluginCommandRequest struct {
	TurnID string `json:"turn_id"`
	Input  any    `json:"input,omitempty"`
}

func (server *Server) listPlugins(writer http.ResponseWriter, request *http.Request) {
	if server.plugins == nil {
		writeError(writer, fmt.Errorf("plugin commands are unavailable"))
		return
	}
	snapshot, err := server.plugins.PluginRegistrations(request.Context(), request.PathValue("threadID"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, snapshot)
}

func (server *Server) executePluginCommand(writer http.ResponseWriter, request *http.Request) {
	if server.plugins == nil {
		writeError(writer, fmt.Errorf("plugin commands are unavailable"))
		return
	}
	var input pluginCommandRequest
	if err := decodeRequest(writer, request, &input); err != nil {
		writeError(writer, err)
		return
	}
	raw, err := server.plugins.ExecutePluginCommand(request.Context(), request.PathValue("threadID"), input.TurnID, request.PathValue("commandID"), input.Input)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"result": raw})
}

type commandEnvelope struct {
	CommandID        string  `json:"command_id"`
	IdempotencyKey   string  `json:"idempotency_key"`
	ActorID          string  `json:"actor_id"`
	ExpectedSequence *uint64 `json:"expected_sequence,omitempty"`
}

type createThreadRequest struct {
	commandEnvelope
	Spec           agothreadstore.ThreadSpec              `json:"spec"`
	Project        agothreadstore.ProjectIdentity         `json:"project"`
	Agent          agothreadstore.AgentDefinitionSnapshot `json:"agent"`
	InitialMessage json.RawMessage                        `json:"initial_message"`
}

type submitRequest struct {
	commandEnvelope
	Content json.RawMessage        `json:"content"`
	Class   agoprotocol.QueueClass `json:"class"`
}

type editQueuedRequest struct {
	commandEnvelope
	Content json.RawMessage `json:"content"`
}

type steerRequest struct {
	commandEnvelope
	ExpectedTurnID string `json:"expected_turn_id"`
}

type interruptRequest struct {
	commandEnvelope
	Content json.RawMessage `json:"content"`
}

type resolveDialogRequest struct {
	ResolverID       string          `json:"resolver_id"`
	ExpectedRevision uint64          `json:"expected_revision"`
	ExpectedSequence *uint64         `json:"expected_sequence,omitempty"`
	Response         json.RawMessage `json:"response"`
}

func (server *Server) listDialogs(writer http.ResponseWriter, request *http.Request) {
	dialogs, err := server.store.ListPendingDialogs(request.Context(), request.PathValue("threadID"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"dialogs": dialogs})
}

func (server *Server) resolveDialog(writer http.ResponseWriter, request *http.Request) {
	if server.dialogs == nil {
		writeError(writer, fmt.Errorf("interactive plugin dialogs are unavailable"))
		return
	}
	var input resolveDialogRequest
	if err := decodeRequest(writer, request, &input); err != nil {
		writeError(writer, err)
		return
	}
	threadID, dialogID := request.PathValue("threadID"), request.PathValue("dialogID")
	all, err := server.store.ListDialogs(request.Context(), threadID)
	if err != nil {
		writeError(writer, err)
		return
	}
	found := false
	for _, dialog := range all {
		found = found || dialog.DialogID == dialogID
	}
	if !found {
		writeError(writer, fmt.Errorf("dialog %q does not belong to thread %q", dialogID, threadID))
		return
	}
	resolved, err := server.dialogs.ResolveDialog(request.Context(), agothreadstore.ResolveDialogInput{
		DialogID: dialogID, ResolverID: input.ResolverID, ExpectedRevision: input.ExpectedRevision,
		ExpectedSequence: input.ExpectedSequence, Response: input.Response,
	})
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusAccepted, resolved)
}

func (server *Server) health(writer http.ResponseWriter, request *http.Request) {
	version, err := server.store.SchemaVersion(request.Context())
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"status": "ok", "store_schema_version": version, "protocol_schema_version": agoprotocol.SchemaVersion})
}

func (server *Server) createThread(writer http.ResponseWriter, request *http.Request) {
	var input createThreadRequest
	if err := decodeRequest(writer, request, &input); err != nil {
		writeError(writer, err)
		return
	}
	canonical, err := canonicalMessage(input.InitialMessage)
	if err != nil {
		writeError(writer, err)
		return
	}
	input.InitialMessage = canonical
	result, err := server.store.CreateAtomicThread(request.Context(), command(input.commandEnvelope, agoprotocol.CommandThreadCreate, ""), agothreadstore.AtomicCreateInput{
		Spec: input.Spec, Project: input.Project, Agent: input.Agent, InitialMessage: input.InitialMessage,
	})
	if err != nil {
		writeError(writer, err)
		return
	}
	// The store commit above is authoritative. Launch is intentionally detached
	// from the client request context: disconnecting after acceptance cannot
	// cancel the committed turn.
	if server.coordinator != nil {
		if err := server.coordinator.LaunchCommitted(result); err != nil {
			writeError(writer, err)
			return
		}
	}
	writeJSON(writer, http.StatusAccepted, result)
}

func (server *Server) listThreads(writer http.ResponseWriter, request *http.Request) {
	if request.URL.RawQuery != "" {
		for key := range request.URL.Query() {
			switch key {
			case "project_id", "search", "archive", "limit", "cursor":
			default:
				writeError(writer, fmt.Errorf("unknown thread catalog query field %q", key))
				return
			}
		}
		limit := uint64(50)
		var err error
		if raw := request.URL.Query().Get("limit"); raw != "" {
			limit, err = parseUint(raw, "limit")
			if err != nil {
				writeError(writer, err)
				return
			}
		}
		archive := agothreadstore.ArchiveFilter(request.URL.Query().Get("archive"))
		if archive == "" {
			archive = agothreadstore.ArchiveActive
		}
		page, searchErr := server.store.SearchThreadCatalog(request.Context(), agothreadstore.ThreadCatalogQuery{
			ProjectID: request.URL.Query().Get("project_id"), Search: request.URL.Query().Get("search"), Archive: archive,
			Limit: int(limit), Cursor: request.URL.Query().Get("cursor"),
		})
		if searchErr != nil {
			writeError(writer, searchErr)
			return
		}
		writeJSON(writer, http.StatusOK, page)
		return
	}
	threads, err := server.store.ListThreads(request.Context())
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"threads": threads})
}

func (server *Server) setThreadArchived(archived bool) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		var input commandEnvelope
		if err := decodeRequest(writer, request, &input); err != nil {
			writeError(writer, err)
			return
		}
		commandType := agothreadstore.CommandThreadArchive
		if !archived {
			commandType = agothreadstore.CommandThreadUnarchive
		}
		result, err := func() (agothreadstore.CommandResult, error) {
			cmd := command(input, commandType, request.PathValue("threadID"))
			if archived {
				return server.store.ArchiveThread(request.Context(), cmd)
			}
			return server.store.UnarchiveThread(request.Context(), cmd)
		}()
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusAccepted, result)
	}
}

func (server *Server) thread(writer http.ResponseWriter, request *http.Request) {
	threadID := request.PathValue("threadID")
	record, err := server.store.Thread(request.Context(), threadID)
	if err != nil {
		writeError(writer, err)
		return
	}
	mailbox, err := server.store.Mailbox(request.Context(), threadID)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"thread": record, "mailbox": mailbox})
}

type PluginClientProjection struct {
	Available     bool                                   `json:"available"`
	Generation    int64                                  `json:"generation"`
	Registrations []agopluginprotocol.PluginRegistration `json:"registrations"`
}

type ExecutorClientProjection struct {
	Target       agoprotocol.ExecutorTarget `json:"target"`
	Activity     agoprotocol.Activity       `json:"activity"`
	ActiveTurnID string                     `json:"active_turn_id,omitempty"`
}

type ThreadClientProjection struct {
	agothreadstore.ClientProjection
	Plugins  PluginClientProjection   `json:"plugins"`
	Executor ExecutorClientProjection `json:"executor"`
}

func (server *Server) clientProjection(writer http.ResponseWriter, request *http.Request) {
	after, err := parseUint(request.URL.Query().Get("after"), "after")
	if err != nil {
		writeError(writer, err)
		return
	}
	limit := uint64(200)
	if raw := request.URL.Query().Get("limit"); raw != "" {
		limit, err = parseUint(raw, "limit")
		if err != nil {
			writeError(writer, err)
			return
		}
	}
	if limit == 0 || limit > 1000 {
		writeError(writer, fmt.Errorf("limit must be between 1 and 1000"))
		return
	}
	durable, err := server.store.ClientProjection(request.Context(), request.PathValue("threadID"), after, int(limit))
	if err != nil {
		writeError(writer, err)
		return
	}
	plugins := PluginClientProjection{Registrations: make([]agopluginprotocol.PluginRegistration, 0)}
	if server.plugins != nil {
		snapshot, snapshotErr := server.plugins.PluginRegistrations(request.Context(), durable.Thread.ThreadID)
		if snapshotErr != nil {
			writeError(writer, snapshotErr)
			return
		}
		plugins.Available = true
		plugins.Generation = snapshot.Generation
		plugins.Registrations = append(plugins.Registrations, snapshot.Registrations...)
	}
	writeJSON(writer, http.StatusOK, ThreadClientProjection{
		ClientProjection: durable,
		Plugins:          plugins,
		Executor: ExecutorClientProjection{
			Target: durable.Thread.Executor, Activity: durable.Mailbox.Activity, ActiveTurnID: durable.Mailbox.ActiveTurnID,
		},
	})
}

func (server *Server) events(writer http.ResponseWriter, request *http.Request) {
	after, err := parseUint(request.URL.Query().Get("after"), "after")
	if err != nil {
		writeError(writer, err)
		return
	}
	events, err := server.store.Replay(request.Context(), request.PathValue("threadID"), after, 0)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"events": events})
}

func (server *Server) submit(writer http.ResponseWriter, request *http.Request) {
	var input submitRequest
	if err := decodeRequest(writer, request, &input); err != nil {
		writeError(writer, err)
		return
	}
	threadID := request.PathValue("threadID")
	canonical, err := server.resolveSubmittedMessage(request.Context(), threadID, input.Content)
	if err != nil {
		writeError(writer, err)
		return
	}
	input.Content = canonical
	state, err := server.coordinator.Submit(request.Context(), command(input.commandEnvelope, agoprotocol.CommandMessageSubmit, threadID), agothreadstore.MessageInput{Content: input.Content, Class: input.Class})
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusAccepted, state)
}

const attachmentRefHeader = "X-Ago-Attachment-Ref"

func (server *Server) uploadAttachment(writer http.ResponseWriter, request *http.Request) {
	if server.attachments == nil {
		writeError(writer, fmt.Errorf("attachment persistence is unavailable"))
		return
	}
	values := request.Header.Values(attachmentRefHeader)
	if len(values) != 1 || len(values[0]) == 0 || len(values[0]) > 2048 {
		writeError(writer, fmt.Errorf("exactly one bounded %s header is required", attachmentRefHeader))
		return
	}
	decoder := json.NewDecoder(strings.NewReader(values[0]))
	decoder.DisallowUnknownFields()
	var ref agoprotocol.AttachmentRef
	if err := decoder.Decode(&ref); err != nil {
		writeError(writer, fmt.Errorf("invalid attachment reference: %w", err))
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(writer, fmt.Errorf("attachment reference must contain exactly one JSON value"))
		return
	}
	if request.ContentLength > int64(agoprotocol.MaxAttachmentBytes) {
		writeError(writer, fmt.Errorf("attachment body exceeds %d bytes", agoprotocol.MaxAttachmentBytes))
		return
	}
	thread, err := server.store.Thread(request.Context(), request.PathValue("threadID"))
	if err != nil {
		writeError(writer, err)
		return
	}
	owner := agoattachments.Owner{ProjectID: thread.Project.ProjectID, ThreadID: thread.ThreadID}
	request.Body = http.MaxBytesReader(writer, request.Body, int64(agoprotocol.MaxAttachmentBytes))
	if err := server.attachments.Upload(request.Context(), owner, ref, request.Body); err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, ref)
}

func (server *Server) resolveSubmittedMessage(ctx context.Context, threadID string, raw json.RawMessage) (json.RawMessage, error) {
	message, err := agoprotocol.DecodeMessageInput(raw)
	if err != nil {
		return nil, err
	}
	if len(message.Attachments) == 0 && len(message.FileMentions) == 0 {
		return agoprotocol.MarshalMessageInput(message)
	}
	thread, err := server.store.Thread(ctx, threadID)
	if err != nil {
		return nil, err
	}
	if len(message.Attachments) != 0 {
		if server.attachments == nil {
			return nil, fmt.Errorf("attachment persistence is unavailable")
		}
		owner := agoattachments.Owner{ProjectID: thread.Project.ProjectID, ThreadID: thread.ThreadID}
		for _, ref := range message.Attachments {
			opened, err := server.attachments.Open(ctx, owner, ref)
			if err != nil {
				return nil, fmt.Errorf("authorize attachment %q: %w", ref.AttachmentID, err)
			}
			if err := opened.Close(); err != nil {
				return nil, fmt.Errorf("close verified attachment %q: %w", ref.AttachmentID, err)
			}
		}
	}
	if len(message.FileMentions) != 0 {
		if err := server.resolveFileMentions(ctx, thread, message.FileMentions); err != nil {
			return nil, err
		}
	}
	return agoprotocol.MarshalMessageInput(message)
}

func (server *Server) resolveFileMentions(ctx context.Context, thread agothreadstore.ThreadRecord, mentions []agoprotocol.RepositoryFileMention) error {
	binding, err := server.store.LatestGitBinding(ctx, thread.ThreadID)
	if err != nil {
		return err
	}
	if thread.Executor.Type != agoprotocol.ExecutorLocal || binding.ThreadID != thread.ThreadID || binding.EnvironmentID != "thread:"+thread.ThreadID || !filepath.IsAbs(binding.WorktreeDir) || binding.WorktreeDir != thread.Workspace {
		return fmt.Errorf("latest git binding does not match authoritative thread worktree")
	}
	for _, mention := range mentions {
		if err := verifyMentionPath(binding.WorktreeDir, mention.Path); err != nil {
			return fmt.Errorf("resolve file mention %q: %w", mention.Path, err)
		}
	}
	return nil
}

func verifyMentionPath(worktree, path string) error {
	rootFD, err := unix.Open(worktree, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	currentFD := rootFD
	components := strings.Split(path, "/")
	for _, component := range components[:len(components)-1] {
		nextFD, openErr := unix.Openat(currentFD, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if currentFD != rootFD {
			_ = unix.Close(currentFD)
		}
		if openErr != nil {
			_ = unix.Close(rootFD)
			return openErr
		}
		currentFD = nextFD
	}
	leafFD, err := unix.Openat(currentFD, components[len(components)-1], unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if currentFD != rootFD {
		_ = unix.Close(currentFD)
	}
	_ = unix.Close(rootFD)
	if err != nil {
		return err
	}
	defer unix.Close(leafFD)
	var stat unix.Stat_t
	if err := unix.Fstat(leafFD, &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("mentioned path is not a regular file")
	}
	return nil
}

func (server *Server) editQueued(writer http.ResponseWriter, request *http.Request) {
	var input editQueuedRequest
	if err := decodeRequest(writer, request, &input); err != nil {
		writeError(writer, err)
		return
	}
	canonical, err := canonicalMessage(input.Content)
	if err != nil {
		writeError(writer, err)
		return
	}
	input.Content = canonical
	threadID := request.PathValue("threadID")
	state, err := server.store.EditQueued(request.Context(), command(input.commandEnvelope, agoprotocol.CommandMessageEditQueued, threadID), request.PathValue("queueItemID"), input.Content)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusAccepted, state)
}

func (server *Server) dequeue(writer http.ResponseWriter, request *http.Request) {
	var input commandEnvelope
	if err := decodeRequest(writer, request, &input); err != nil {
		writeError(writer, err)
		return
	}
	threadID := request.PathValue("threadID")
	state, err := server.store.Dequeue(request.Context(), command(input, agoprotocol.CommandMessageDequeue, threadID), request.PathValue("queueItemID"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusAccepted, state)
}

func (server *Server) steer(writer http.ResponseWriter, request *http.Request) {
	var input steerRequest
	if err := decodeRequest(writer, request, &input); err != nil {
		writeError(writer, err)
		return
	}
	threadID := request.PathValue("threadID")
	state, err := server.coordinator.Steer(request.Context(), command(input.commandEnvelope, agoprotocol.CommandMessageSteer, threadID), request.PathValue("queueItemID"), input.ExpectedTurnID)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusAccepted, state)
}

func (server *Server) interrupt(writer http.ResponseWriter, request *http.Request) {
	var input interruptRequest
	if err := decodeRequest(writer, request, &input); err != nil {
		writeError(writer, err)
		return
	}
	canonical, err := canonicalMessage(input.Content)
	if err != nil {
		writeError(writer, err)
		return
	}
	input.Content = canonical
	threadID := request.PathValue("threadID")
	state, err := server.coordinator.InterruptAndSubmit(request.Context(), command(input.commandEnvelope, agoprotocol.CommandTurnInterrupt, threadID), request.PathValue("turnID"), agothreadstore.MessageInput{Content: input.Content, Class: agoprotocol.QueueSteer})
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusAccepted, state)
}

func (server *Server) cancel(writer http.ResponseWriter, request *http.Request) {
	var input commandEnvelope
	if err := decodeRequest(writer, request, &input); err != nil {
		writeError(writer, err)
		return
	}
	threadID := request.PathValue("threadID")
	state, err := server.coordinator.Cancel(request.Context(), command(input, agoprotocol.CommandTurnCancel, threadID), request.PathValue("turnID"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusAccepted, state)
}

func command(envelope commandEnvelope, commandType agoprotocol.CommandType, threadID string) agoprotocol.Command {
	return agoprotocol.Command{
		SchemaVersion:    agoprotocol.SchemaVersion,
		CommandID:        envelope.CommandID,
		IdempotencyKey:   envelope.IdempotencyKey,
		ActorID:          envelope.ActorID,
		Type:             commandType,
		ThreadID:         threadID,
		ExpectedSequence: envelope.ExpectedSequence,
	}
}

func canonicalMessage(raw json.RawMessage) (json.RawMessage, error) {
	message, err := agoprotocol.DecodeMessageInput(raw)
	if err != nil {
		return nil, err
	}
	if len(message.Attachments) != 0 || len(message.FileMentions) != 0 {
		return nil, fmt.Errorf("attachment and file-mention resolution is unavailable")
	}
	return agoprotocol.MarshalMessageInput(message)
}

func decodeRequest(writer http.ResponseWriter, request *http.Request, target any) error {
	request.Body = http.MaxBytesReader(writer, request.Body, maxRequestBytes)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid request JSON: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("request must contain exactly one JSON value")
	}
	return nil
}

func parseUint(value, name string) (uint64, error) {
	if strings.TrimSpace(value) == "" {
		return 0, nil
	}
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be an unsigned integer", name)
	}
	return parsed, nil
}

func writeError(writer http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	var conflict agothreadstore.ConflictError
	if errors.As(err, &conflict) {
		status = http.StatusConflict
	}
	if errors.Is(err, agoattachments.ErrConflict) {
		status = http.StatusConflict
	} else if errors.Is(err, agoattachments.ErrNotFound) {
		status = http.StatusNotFound
	} else if errors.Is(err, agoattachments.ErrUnauthorized) {
		status = http.StatusForbidden
	}
	var gitConflict agothreadstore.GitOperationConflictError
	if errors.As(err, &gitConflict) || errors.Is(err, agogit.ErrIndexMutationConflict) {
		status = http.StatusConflict
	}
	var commentConflict agothreadstore.GitCommentConflictError
	if errors.As(err, &commentConflict) {
		status = http.StatusConflict
	}
	var unsupported *agogit.UnsupportedExecutorError
	if errors.As(err, &unsupported) {
		status = http.StatusNotImplemented
	}
	writeJSON(writer, status, map[string]any{"error": err.Error()})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
