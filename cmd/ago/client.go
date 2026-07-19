package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"claudexflow/internal/agoprotocol"
	"golang.org/x/sys/unix"
)

type clientFlags struct {
	socket, thread, queue, turn, content, title, project, workspace, mode, executor string
	pluginCommand, input, dialog, resolver, response                                string
	snapshotDigest, units                                                           string
	receipt                                                                         string
	fileID, hunkID                                                                  string
	search, archiveFilter, cursor                                                   string
	idempotency, until, expectedTurn                                                string
	expected                                                                        int64
	expectedRevision                                                                uint64
	snapshotRevision                                                                uint64
	after                                                                           int64
	limit                                                                           int
	poll                                                                            time.Duration
	expectedSet                                                                     bool
	attachments, fileMentions                                                       stringList
}

type stringList []string

func (values *stringList) String() string { return strings.Join(*values, ",") }
func (values *stringList) Set(value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("value must not be empty")
	}
	*values = append(*values, value)
	return nil
}

func runClient(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("missing command")
	}
	cmd := args[0]
	f := flag.NewFlagSet("ago "+cmd, flag.ContinueOnError)
	f.SetOutput(stderr)
	home, _ := os.UserHomeDir()
	c := clientFlags{mode: "medium", executor: "local", expected: -1, limit: 200, poll: 250 * time.Millisecond}
	f.StringVar(&c.socket, "socket", filepath.Join(home, ".local", "state", "ago", "ago.sock"), "daemon Unix socket")
	f.StringVar(&c.thread, "thread", "", "thread ID")
	f.StringVar(&c.queue, "queue", "", "queue item ID")
	f.StringVar(&c.turn, "turn", "", "turn ID")
	f.StringVar(&c.content, "content", "", "content text")
	f.Var(&c.attachments, "attachment", "local attachment file (repeatable; submit only)")
	f.Var(&c.fileMentions, "file-mention", "repository-relative file mention (repeatable; submit only)")
	f.StringVar(&c.title, "title", "", "thread title")
	f.StringVar(&c.project, "project", "", "project identity")
	f.StringVar(&c.workspace, "workspace", "", "thread workspace")
	f.StringVar(&c.mode, "mode", "medium", "thread mode")
	f.StringVar(&c.executor, "executor", "local", "thread executor")
	f.StringVar(&c.idempotency, "idempotency-key", "", "retry idempotency key")
	f.Int64Var(&c.expected, "expected-sequence", -1, "sequence fence")
	f.Int64Var(&c.after, "after", 0, "event cursor")
	f.StringVar(&c.until, "until", "", "stop after this event type")
	f.StringVar(&c.expectedTurn, "expected-turn", "", "expected active turn ID")
	f.StringVar(&c.pluginCommand, "command", "", "canonical pluginId:commandId")
	f.StringVar(&c.input, "input", "{}", "plugin command input as JSON")
	f.StringVar(&c.dialog, "dialog", "", "plugin dialog ID")
	f.StringVar(&c.resolver, "resolver", "", "dialog resolver ID")
	f.StringVar(&c.response, "response", "", "typed plugin dialog result as JSON")
	f.Uint64Var(&c.expectedRevision, "expected-revision", 0, "dialog revision fence")
	f.Uint64Var(&c.snapshotRevision, "snapshot-revision", 0, "Git snapshot revision fence")
	f.StringVar(&c.snapshotDigest, "snapshot-digest", "", "Git snapshot digest fence")
	f.StringVar(&c.units, "units", "", "comma-separated opaque Git unit IDs")
	f.StringVar(&c.receipt, "receipt", "", "durable Ago tool-write receipt ID")
	f.StringVar(&c.fileID, "file-id", "", "opaque Git file ID")
	f.StringVar(&c.hunkID, "hunk-id", "", "optional opaque Git hunk ID")
	f.StringVar(&c.search, "search", "", "thread title search")
	f.StringVar(&c.archiveFilter, "archive-filter", "active", "active, archived, or all threads")
	f.StringVar(&c.cursor, "cursor", "", "opaque pagination cursor")
	f.IntVar(&c.limit, "limit", 200, "projection event page size")
	f.DurationVar(&c.poll, "poll", 250*time.Millisecond, "watch polling interval")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(f.Args(), " "))
	}
	if c.socket == "" {
		return errors.New("--socket must not be empty")
	}
	if c.after < 0 {
		return errors.New("--after must not be negative")
	}
	if c.limit < 1 || c.limit > 1000 {
		return errors.New("--limit must be between 1 and 1000")
	}
	f.Visit(func(option *flag.Flag) {
		if option.Name == "expected-sequence" {
			c.expectedSet = true
		}
	})
	if c.expectedSet && c.expected < 0 {
		return errors.New("--expected-sequence must not be negative")
	}
	client := unixHTTPClient(c.socket)
	switch cmd {
	case "list":
		if c.project == "" {
			return requestJSON(ctx, client, http.MethodGet, "/v1/threads", nil, stdout, stderr)
		}
		query := url.Values{"project_id": {c.project}, "archive": {c.archiveFilter}, "limit": {strconv.Itoa(c.limit)}}
		if c.search != "" {
			query.Set("search", c.search)
		}
		if c.cursor != "" {
			query.Set("cursor", c.cursor)
		}
		return requestJSON(ctx, client, http.MethodGet, "/v1/threads?"+query.Encode(), nil, stdout, stderr)
	case "open":
		if err := require("thread", c.thread); err != nil {
			return err
		}
		return requestJSON(ctx, client, http.MethodGet, projectionPath(c.thread, c.after, c.limit), nil, stdout, stderr)
	case "conformance":
		if err := require("thread", c.thread); err != nil {
			return err
		}
		return writeProjectionConformance(ctx, client, c.thread, c.limit, stdout, stderr)
	case "replay":
		if err := require("thread", c.thread); err != nil {
			return err
		}
		return requestJSON(ctx, client, http.MethodGet, eventsPath(c.thread, c.after), nil, stdout, stderr)
	case "plugins":
		if err := require("thread", c.thread); err != nil {
			return err
		}
		return requestJSON(ctx, client, http.MethodGet, threadPath(c.thread)+"/plugins", nil, stdout, stderr)
	case "dialogs":
		if err := require("thread", c.thread); err != nil {
			return err
		}
		return requestJSON(ctx, client, http.MethodGet, threadPath(c.thread)+"/dialogs", nil, stdout, stderr)
	case "resolve-dialog":
		if err := require("thread", c.thread, "dialog", c.dialog, "resolver", c.resolver, "response", c.response); err != nil {
			return err
		}
		if c.expectedRevision == 0 {
			return errors.New("--expected-revision must be positive")
		}
		var response any
		decoder := json.NewDecoder(strings.NewReader(c.response))
		decoder.UseNumber()
		if err := decoder.Decode(&response); err != nil {
			return fmt.Errorf("invalid --response JSON: %w", err)
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			return errors.New("--response must contain exactly one JSON value")
		}
		body := map[string]any{"resolver_id": c.resolver, "expected_revision": c.expectedRevision, "response": response}
		if c.expectedSet {
			body["expected_sequence"] = c.expected
		}
		return requestJSON(ctx, client, http.MethodPost, threadPath(c.thread)+"/dialogs/"+escaped(c.dialog)+"/resolve", body, stdout, stderr)
	case "plugin-command":
		if err := require("thread", c.thread, "turn", c.turn, "command", c.pluginCommand); err != nil {
			return err
		}
		var input any
		decoder := json.NewDecoder(strings.NewReader(c.input))
		decoder.UseNumber()
		if err := decoder.Decode(&input); err != nil {
			return fmt.Errorf("invalid --input JSON: %w", err)
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			return errors.New("--input must contain exactly one JSON value")
		}
		return requestJSON(ctx, client, http.MethodPost, threadPath(c.thread)+"/plugin-commands/"+escaped(c.pluginCommand), map[string]any{"turn_id": c.turn, "input": input}, stdout, stderr)
	case "watch":
		if err := require("thread", c.thread); err != nil {
			return err
		}
		if c.poll <= 0 {
			return errors.New("--poll must be positive")
		}
		return watch(ctx, client, c, stdout, stderr)
	case "create":
		if len(c.attachments) != 0 || len(c.fileMentions) != 0 {
			return errors.New("--attachment and --file-mention are unavailable for initial thread creation")
		}
		if err := require("title", c.title, "project", c.project, "workspace", c.workspace, "content", c.content, "mode", c.mode, "executor", c.executor); err != nil {
			return err
		}
		return mutate(ctx, client, http.MethodPost, "/v1/threads", c, map[string]any{
			"spec":            map[string]any{"title": c.title, "workspace": c.workspace, "mode": c.mode, "executor": map[string]string{"type": c.executor}},
			"project":         map[string]any{"project_id": c.project},
			"agent":           map[string]any{"definition_id": "ago.default", "version": "1", "display_name": "Ago", "system_instructions_ref": "ago://agents/default/v1", "system_instructions_digest": "sha256:cbcc77cdbf7c9dd2a9dff6898f98cd821c41c4b05043e14385979ad2be98d539", "default_mode": c.mode},
			"initial_message": map[string]string{"text": c.content},
		}, stdout, stderr)
	case "submit":
		if err := require("thread", c.thread); err != nil {
			return err
		}
		if c.content == "" && len(c.attachments) == 0 && len(c.fileMentions) == 0 {
			return errors.New("submit requires --content, --attachment, or --file-mention")
		}
		return submitMessage(ctx, client, c, stdout, stderr)
	case "stage", "unstage":
		if err := require("thread", c.thread, "snapshot-digest", c.snapshotDigest, "units", c.units); err != nil {
			return err
		}
		if !c.expectedSet || c.snapshotRevision == 0 {
			return errors.New("--expected-sequence and positive --snapshot-revision are required")
		}
		selected := strings.Split(c.units, ",")
		for _, id := range selected {
			if strings.TrimSpace(id) == "" || id != strings.TrimSpace(id) {
				return errors.New("--units must contain non-empty comma-separated opaque IDs")
			}
		}
		return gitMutate(ctx, client, threadPath(c.thread)+"/diff/"+cmd, c, selected, stdout, stderr)
	case "revert":
		if err := require("thread", c.thread, "snapshot-digest", c.snapshotDigest, "receipt", c.receipt); err != nil {
			return err
		}
		if !c.expectedSet || c.snapshotRevision == 0 {
			return errors.New("--expected-sequence and positive --snapshot-revision are required")
		}
		return gitRevert(ctx, client, threadPath(c.thread)+"/diff/revert", c, stdout, stderr)
	case "comment":
		if err := require("thread", c.thread, "snapshot-digest", c.snapshotDigest, "file-id", c.fileID, "content", c.content); err != nil {
			return err
		}
		if !c.expectedSet || c.expected <= 0 || c.snapshotRevision == 0 {
			return errors.New("positive --expected-sequence and --snapshot-revision are required")
		}
		body := map[string]any{
			"comment_id": newID(), "expected_sequence": c.expected, "snapshot_revision": c.snapshotRevision,
			"snapshot_digest": c.snapshotDigest, "file_id": c.fileID, "actor_id": "ago-cli", "body": c.content,
		}
		if c.hunkID != "" {
			body["hunk_id"] = c.hunkID
		}
		return requestJSON(ctx, client, http.MethodPost, threadPath(c.thread)+"/diff/comments", body, stdout, stderr)
	case "archive", "unarchive":
		if err := require("thread", c.thread); err != nil {
			return err
		}
		if !c.expectedSet || c.expected <= 0 {
			return errors.New("positive --expected-sequence is required")
		}
		return mutate(ctx, client, http.MethodPost, threadPath(c.thread)+"/"+cmd, c, nil, stdout, stderr)
	case "edit":
		if err := require("thread", c.thread, "queue", c.queue, "content", c.content); err != nil {
			return err
		}
		return mutate(ctx, client, http.MethodPatch, queuePath(c), c, map[string]any{"content": map[string]string{"text": c.content}}, stdout, stderr)
	case "dequeue":
		if err := require("thread", c.thread, "queue", c.queue); err != nil {
			return err
		}
		return mutate(ctx, client, http.MethodDelete, queuePath(c), c, nil, stdout, stderr)
	case "steer":
		if err := require("thread", c.thread, "queue", c.queue, "expected-turn", c.expectedTurn); err != nil {
			return err
		}
		return mutate(ctx, client, http.MethodPost, queuePath(c)+"/steer", c, map[string]any{"expected_turn_id": c.expectedTurn}, stdout, stderr)
	case "interrupt":
		if err := require("thread", c.thread, "turn", c.turn, "content", c.content); err != nil {
			return err
		}
		return mutate(ctx, client, http.MethodPost, turnPath(c)+"/interrupt", c, map[string]any{"content": map[string]string{"text": c.content}}, stdout, stderr)
	case "cancel":
		if err := require("thread", c.thread, "turn", c.turn); err != nil {
			return err
		}
		return mutate(ctx, client, http.MethodPost, turnPath(c)+"/cancel", c, nil, stdout, stderr)
	default:
		return fmt.Errorf("unknown command %q", cmd)
	}
}

type preparedAttachment struct {
	ref     agoprotocol.AttachmentRef
	content []byte
}

func submitMessage(ctx context.Context, client *http.Client, c clientFlags, out, errOut io.Writer) error {
	if len(c.attachments) > agoprotocol.MaxMessageAttachments {
		return fmt.Errorf("message must not contain more than %d attachments", agoprotocol.MaxMessageAttachments)
	}
	if c.idempotency == "" {
		c.idempotency = newID()
	}
	message := agoprotocol.MessageInput{Text: c.content}
	for _, path := range c.fileMentions {
		message.FileMentions = append(message.FileMentions, agoprotocol.RepositoryFileMention{Path: path})
	}
	prepared := make([]preparedAttachment, 0, len(c.attachments))
	var totalBytes uint64
	for index, path := range c.attachments {
		content, filename, mediaType, err := readAttachment(path)
		if err != nil {
			return fmt.Errorf("attachment %q: %w", path, err)
		}
		totalBytes += uint64(len(content))
		if totalBytes > agoprotocol.MaxMessageAttachmentBytes {
			return fmt.Errorf("message attachments must not exceed %d bytes", agoprotocol.MaxMessageAttachmentBytes)
		}
		digest := sha256.Sum256(content)
		identity := sha256.Sum256([]byte(c.idempotency + "\x00" + strconv.Itoa(index)))
		ref := agoprotocol.AttachmentRef{
			AttachmentID: "att-" + hex.EncodeToString(identity[:]), SHA256: hex.EncodeToString(digest[:]),
			SizeBytes: uint64(len(content)), MediaType: mediaType, Filename: filename,
		}
		if err := ref.Validate(); err != nil {
			return fmt.Errorf("attachment %q metadata: %w", path, err)
		}
		message.Attachments = append(message.Attachments, ref)
		prepared = append(prepared, preparedAttachment{ref: ref, content: content})
	}
	canonical, err := agoprotocol.MarshalMessageInput(message)
	if err != nil {
		return err
	}
	for _, attachment := range prepared {
		if err := uploadAttachment(ctx, client, c.thread, attachment, errOut); err != nil {
			return err
		}
	}
	return mutate(ctx, client, http.MethodPost, threadPath(c.thread)+"/messages", c, map[string]any{"content": canonical, "class": "normal"}, out, errOut)
}

func readAttachment(path string) ([]byte, string, string, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, "", "", err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, "", "", fmt.Errorf("open attachment file")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, "", "", err
	}
	if !info.Mode().IsRegular() {
		return nil, "", "", fmt.Errorf("attachment must be a regular file")
	}
	content, err := io.ReadAll(io.LimitReader(file, int64(agoprotocol.MaxAttachmentBytes)+1))
	if err != nil {
		return nil, "", "", err
	}
	if uint64(len(content)) > agoprotocol.MaxAttachmentBytes {
		return nil, "", "", fmt.Errorf("attachment exceeds %d bytes", agoprotocol.MaxAttachmentBytes)
	}
	mediaType, _, err := mime.ParseMediaType(http.DetectContentType(content))
	if err != nil {
		return nil, "", "", fmt.Errorf("detect attachment media type: %w", err)
	}
	return content, filepath.Base(path), strings.ToLower(mediaType), nil
}

func uploadAttachment(ctx context.Context, client *http.Client, threadID string, attachment preparedAttachment, errOut io.Writer) error {
	encodedRef, err := json.Marshal(attachment.ref)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://ago"+threadPath(threadID)+"/attachments", bytes.NewReader(attachment.content))
	if err != nil {
		return err
	}
	request.Header.Set("X-Ago-Attachment-Ref", string(encodedRef))
	request.Header.Set("Content-Type", "application/octet-stream")
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if len(body) != 0 {
			_, _ = errOut.Write(append(bytes.TrimSpace(body), '\n'))
		}
		return fmt.Errorf("attachment upload HTTP %d %s", response.StatusCode, response.Status)
	}
	var returned agoprotocol.AttachmentRef
	if err := json.Unmarshal(body, &returned); err != nil || returned != attachment.ref {
		return fmt.Errorf("daemon returned mismatched attachment reference")
	}
	return nil
}

func gitRevert(ctx context.Context, client *http.Client, path string, c clientFlags, out, errOut io.Writer) error {
	if c.idempotency == "" {
		c.idempotency = "git:" + newID()
	} else if !strings.HasPrefix(c.idempotency, "git:") {
		c.idempotency = "git:" + c.idempotency
	}
	body := map[string]any{
		"command_id": c.idempotency, "idempotency_key": c.idempotency, "actor_id": "ago-cli",
		"expected_sequence": c.expected, "expected_snapshot_revision": c.snapshotRevision,
		"expected_snapshot_digest": c.snapshotDigest, "receipt_id": c.receipt,
	}
	return requestJSON(ctx, client, http.MethodPost, path, body, out, errOut)
}

func gitMutate(ctx context.Context, client *http.Client, path string, c clientFlags, units []string, out, errOut io.Writer) error {
	if c.idempotency == "" {
		c.idempotency = "git:" + newID()
	} else if !strings.HasPrefix(c.idempotency, "git:") {
		c.idempotency = "git:" + c.idempotency
	}
	body := map[string]any{
		"command_id": c.idempotency, "idempotency_key": c.idempotency, "actor_id": "ago-cli",
		"expected_sequence": c.expected, "expected_snapshot_revision": c.snapshotRevision,
		"expected_snapshot_digest": c.snapshotDigest, "selected_unit_ids": units,
	}
	return requestJSON(ctx, client, http.MethodPost, path, body, out, errOut)
}

func mutate(ctx context.Context, client *http.Client, method, path string, c clientFlags, fields map[string]any, out, errOut io.Writer) error {
	if fields == nil {
		fields = map[string]any{}
	}
	fields["actor_id"] = "ago-cli"
	if c.idempotency == "" {
		c.idempotency = newID()
	}
	fields["command_id"] = c.idempotency
	fields["idempotency_key"] = c.idempotency
	if c.expectedSet {
		fields["expected_sequence"] = c.expected
	}
	return requestJSON(ctx, client, method, path, fields, out, errOut)
}

func requestJSON(ctx context.Context, client *http.Client, method, path string, body any, out, errOut io.Writer) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://ago"+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if len(b) > 0 {
			_, _ = errOut.Write(append(bytes.TrimSpace(b), '\n'))
		}
		return fmt.Errorf("HTTP %d %s", resp.StatusCode, resp.Status)
	}
	if !json.Valid(b) {
		return errors.New("daemon returned invalid JSON")
	}
	_, err = out.Write(append(bytes.TrimSpace(b), '\n'))
	return err
}

func watch(ctx context.Context, client *http.Client, c clientFlags, out, errOut io.Writer) error {
	cursor := c.after
	for {
		var payload struct {
			Events            []json.RawMessage `json:"events"`
			NextAfterSequence uint64            `json:"next_after_sequence"`
			HasMore           bool              `json:"has_more"`
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://ago"+projectionPath(c.thread, cursor, c.limit), nil)
		resp, err := client.Do(req)
		if err == nil {
			b, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if readErr == nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
				err = json.Unmarshal(b, &payload)
			} else if readErr != nil {
				err = readErr
			} else {
				_, _ = errOut.Write(append(bytes.TrimSpace(b), '\n'))
				return fmt.Errorf("HTTP %d %s", resp.StatusCode, resp.Status)
			}
		}
		if err == nil {
			for _, raw := range payload.Events {
				var e struct {
					Sequence int64  `json:"sequence"`
					Type     string `json:"type"`
				}
				if json.Unmarshal(raw, &e) != nil {
					return errors.New("invalid event JSON")
				}
				if e.Sequence <= cursor {
					continue
				}
				if _, err := out.Write(append(bytes.TrimSpace(raw), '\n')); err != nil {
					return err
				}
				cursor = e.Sequence
				if c.until != "" && e.Type == c.until {
					return nil
				}
			}
			if payload.NextAfterSequence != uint64(cursor) {
				return errors.New("projection cursor does not match delivered events")
			}
			if payload.HasMore {
				if len(payload.Events) == 0 {
					return errors.New("projection has_more page contains no events")
				}
				continue
			}
		}
		timer := time.NewTimer(c.poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

const maxProjectionPageBytes = 16 << 20

type clientProjectionPage struct {
	threadID        string
	requested, next uint64
	snapshot        uint64
	hasMore         bool
	events          []any
	mailbox         map[string]any
	queue, dialogs  []any
	diff            map[string]any
	diffComments    []any
}

func writeProjectionConformance(ctx context.Context, client *http.Client, threadID string, limit int, out, errOut io.Writer) error {
	var last clientProjectionPage
	var events []any
	var cursor uint64
	for {
		page, err := fetchClientProjection(ctx, client, threadID, cursor, limit, errOut)
		if err != nil {
			return err
		}
		last = page
		events = append(events, page.events...)
		cursor = page.next
		if !page.hasMore {
			break
		}
	}
	mailbox := make(map[string]any, len(last.mailbox))
	for key, value := range last.mailbox {
		if key != "queue" {
			mailbox[key] = value
		}
	}
	whole := map[string]any{"snapshot_sequence": last.snapshot, "mailbox": mailbox, "queue": last.queue, "dialogs": last.dialogs, "diff": last.diff, "events": events}
	mailboxSummary := make(map[string]any, len(mailbox)+1)
	for key, value := range mailbox {
		mailboxSummary[key] = value
	}
	mailboxSummary["digest"] = conformanceDigest(mailbox)
	var firstSequence, lastSequence any
	if len(events) != 0 {
		firstSequence = events[0].(map[string]any)["sequence"]
		lastSequence = events[len(events)-1].(map[string]any)["sequence"]
	}
	summary := map[string]any{
		"digest":            conformanceDigest(whole),
		"snapshot_sequence": last.snapshot,
		"mailbox":           mailboxSummary,
		"queue":             map[string]any{"count": len(last.queue), "digest": conformanceDigest(last.queue)},
		"dialogs":           map[string]any{"count": len(last.dialogs), "digest": conformanceDigest(last.dialogs)},
		"diff":              map[string]any{"has_snapshot": last.diff["snapshot"] != nil, "comment_count": len(last.diffComments), "digest": conformanceDigest(last.diff)},
		"events":            map[string]any{"count": len(events), "first_sequence": firstSequence, "last_sequence": lastSequence, "digest": conformanceDigest(events)},
	}
	encoded, err := canonicalJSON(summary)
	if err != nil {
		return fmt.Errorf("encode conformance summary: %w", err)
	}
	_, err = fmt.Fprintln(out, string(encoded))
	return err
}

func fetchClientProjection(ctx context.Context, client *http.Client, threadID string, after uint64, limit int, errOut io.Writer) (clientProjectionPage, error) {
	var page clientProjectionPage
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://ago"+projectionPath(threadID, int64(after), limit), nil)
	if err != nil {
		return page, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return page, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxProjectionPageBytes+1))
	if err != nil {
		return page, err
	}
	if len(data) > maxProjectionPageBytes {
		return page, errors.New("projection page exceeds size limit")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if len(data) != 0 {
			_, _ = errOut.Write(append(bytes.TrimSpace(data), '\n'))
		}
		return page, fmt.Errorf("HTTP %d %s", resp.StatusCode, resp.Status)
	}
	page, err = parseClientProjection(data)
	if err != nil {
		return page, fmt.Errorf("malformed projection: %w", err)
	}
	if page.threadID != threadID || page.requested != after {
		return page, errors.New("malformed projection: projection request contradiction")
	}
	return page, nil
}

func parseClientProjection(data []byte) (clientProjectionPage, error) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&value); err != nil {
		return clientProjectionPage{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return clientProjectionPage{}, errors.New("projection must contain exactly one JSON value")
	}
	raw, err := projectionObject(value, "projection")
	if err != nil {
		return clientProjectionPage{}, err
	}
	if err := exactProjectionKeys(raw, []string{"schema_version", "thread", "mailbox", "events", "dialogs", "diff", "requested_after_sequence", "next_after_sequence", "snapshot_sequence", "has_more", "plugins", "executor"}, "projection"); err != nil {
		return clientProjectionPage{}, err
	}
	schema, err := projectionUint(raw["schema_version"], "schema_version")
	if err != nil || schema != 1 {
		return clientProjectionPage{}, errors.New("unsupported projection schema")
	}
	thread, err := projectionObject(raw["thread"], "thread")
	if err != nil {
		return clientProjectionPage{}, err
	}
	threadID, err := projectionString(thread["thread_id"], "thread_id", false)
	if err != nil {
		return clientProjectionPage{}, err
	}
	threadSequence, err := projectionUint(thread["last_sequence"], "thread.last_sequence")
	if err != nil {
		return clientProjectionPage{}, err
	}
	threadTarget, err := projectionTarget(thread["executor"])
	if err != nil {
		return clientProjectionPage{}, err
	}
	if _, err = projectionString(thread["mode"], "thread.mode", false); err != nil {
		return clientProjectionPage{}, err
	}
	project, err := projectionObject(thread["project"], "project")
	if err != nil {
		return clientProjectionPage{}, err
	}
	if _, err = projectionString(project["project_id"], "project_id", false); err != nil {
		return clientProjectionPage{}, err
	}
	agent, err := projectionObject(thread["agent"], "agent")
	if err != nil {
		return clientProjectionPage{}, err
	}
	for _, key := range []string{"definition_id", "version", "display_name", "default_mode"} {
		if _, err = projectionString(agent[key], "agent."+key, false); err != nil {
			return clientProjectionPage{}, err
		}
	}
	mailbox, err := projectionObject(raw["mailbox"], "mailbox")
	if err != nil {
		return clientProjectionPage{}, err
	}
	mailboxThread, err := projectionString(mailbox["thread_id"], "mailbox.thread_id", false)
	if err != nil {
		return clientProjectionPage{}, err
	}
	mailboxSequence, err := projectionUint(mailbox["last_sequence"], "mailbox.last_sequence")
	if err != nil {
		return clientProjectionPage{}, err
	}
	mailboxActivity, err := projectionString(mailbox["activity"], "mailbox.activity", false)
	if err != nil {
		return clientProjectionPage{}, err
	}
	if _, ok := mailbox["cancel_requested"].(bool); !ok {
		return clientProjectionPage{}, errors.New("mailbox.cancel_requested must be a boolean")
	}
	queue, err := projectionArray(mailbox["queue"], "mailbox.queue")
	if err != nil {
		return clientProjectionPage{}, err
	}
	for index, value := range queue {
		item, objectErr := projectionObject(value, fmt.Sprintf("queue item %d", index))
		if objectErr != nil {
			return clientProjectionPage{}, objectErr
		}
		normalized := make(map[string]any, 5)
		for _, key := range []string{"queue_item_id", "class", "state"} {
			normalized[key], err = projectionString(item[key], "queue item."+key, false)
			if err != nil {
				return clientProjectionPage{}, err
			}
		}
		if _, err = projectionUint(item["position"], "queue item.position"); err != nil {
			return clientProjectionPage{}, err
		}
		normalized["position"] = item["position"]
		content, exists := item["content"]
		if !exists {
			return clientProjectionPage{}, errors.New("queue item.content missing")
		}
		normalized["content"] = content
		queue[index] = normalized
	}
	events, err := projectionArray(raw["events"], "events")
	if err != nil {
		return clientProjectionPage{}, err
	}
	dialogs, err := projectionArray(raw["dialogs"], "dialogs")
	if err != nil {
		return clientProjectionPage{}, err
	}
	for index, value := range dialogs {
		dialog, objectErr := normalizeProjectionDialog(value, index)
		if objectErr != nil {
			return clientProjectionPage{}, objectErr
		}
		dialogs[index] = dialog
	}
	diff, err := projectionObject(raw["diff"], "diff")
	if err != nil {
		return clientProjectionPage{}, err
	}
	if err = exactProjectionKeys(diff, []string{"snapshot", "comments"}, "diff"); err != nil {
		return clientProjectionPage{}, err
	}
	if snapshot := diff["snapshot"]; snapshot != nil {
		if _, ok := snapshot.(map[string]any); !ok {
			return clientProjectionPage{}, errors.New("diff.snapshot must be an object or null")
		}
	}
	comments, err := projectionArray(diff["comments"], "diff.comments")
	if err != nil {
		return clientProjectionPage{}, err
	}
	plugins, err := projectionObject(raw["plugins"], "plugins")
	if err != nil {
		return clientProjectionPage{}, err
	}
	if err = exactProjectionKeys(plugins, []string{"available", "generation", "registrations"}, "plugins"); err != nil {
		return clientProjectionPage{}, err
	}
	if _, ok := plugins["available"].(bool); !ok {
		return clientProjectionPage{}, errors.New("plugins.available must be a boolean")
	}
	if _, err = projectionUint(plugins["generation"], "plugins.generation"); err != nil {
		return clientProjectionPage{}, err
	}
	if _, err = projectionArray(plugins["registrations"], "plugins.registrations"); err != nil {
		return clientProjectionPage{}, err
	}
	executor, err := projectionObject(raw["executor"], "executor")
	if err != nil {
		return clientProjectionPage{}, err
	}
	if err = exactProjectionKeys(executor, []string{"target", "activity", "active_turn_id"}, "executor"); err != nil {
		return clientProjectionPage{}, err
	}
	executorTarget, err := projectionTarget(executor["target"])
	if err != nil {
		return clientProjectionPage{}, err
	}
	executorActivity, err := projectionString(executor["activity"], "executor.activity", false)
	if err != nil {
		return clientProjectionPage{}, err
	}
	requested, err := projectionUint(raw["requested_after_sequence"], "requested_after_sequence")
	if err != nil {
		return clientProjectionPage{}, err
	}
	next, err := projectionUint(raw["next_after_sequence"], "next_after_sequence")
	if err != nil {
		return clientProjectionPage{}, err
	}
	snapshot, err := projectionUint(raw["snapshot_sequence"], "snapshot_sequence")
	if err != nil {
		return clientProjectionPage{}, err
	}
	hasMore, ok := raw["has_more"].(bool)
	if !ok {
		return clientProjectionPage{}, errors.New("has_more must be a boolean")
	}
	previous := requested
	for index, eventValue := range events {
		event, eventThread, sequence, objectErr := normalizeProjectionEvent(eventValue, fmt.Sprintf("event %d", index))
		if objectErr != nil {
			return clientProjectionPage{}, objectErr
		}
		if eventThread != threadID || sequence <= previous || sequence > next {
			return clientProjectionPage{}, errors.New("projection event contradiction")
		}
		events[index] = event
		previous = sequence
	}
	mailboxActive := ""
	mailboxActiveValue, mailboxActiveExists := mailbox["active_turn_id"]
	if mailboxActiveExists {
		mailboxActive, err = projectionString(mailboxActiveValue, "mailbox.active_turn_id", true)
		if err != nil {
			return clientProjectionPage{}, err
		}
	}
	executorActive := ""
	executorActiveValue, executorActiveExists := executor["active_turn_id"]
	if executorActiveExists {
		executorActive, err = projectionString(executorActiveValue, "executor.active_turn_id", true)
		if err != nil {
			return clientProjectionPage{}, err
		}
	}
	normalizedMailbox := map[string]any{
		"thread_id":        mailboxThread,
		"last_sequence":    mailbox["last_sequence"],
		"activity":         mailboxActivity,
		"cancel_requested": mailbox["cancel_requested"],
		"queue":            queue,
	}
	if mailboxActiveExists {
		normalizedMailbox["active_turn_id"] = mailboxActive
	}
	if mailboxEventsValue, exists := mailbox["events"]; exists {
		mailboxEvents, arrayErr := projectionArray(mailboxEventsValue, "mailbox.events")
		if arrayErr != nil {
			return clientProjectionPage{}, arrayErr
		}
		for index, value := range mailboxEvents {
			event, _, _, eventErr := normalizeProjectionEvent(value, fmt.Sprintf("mailbox event %d", index))
			if eventErr != nil {
				return clientProjectionPage{}, eventErr
			}
			mailboxEvents[index] = event
		}
		normalizedMailbox["events"] = mailboxEvents
	}
	if next < requested || next > snapshot || threadSequence != snapshot || mailboxSequence != snapshot || mailboxThread != threadID ||
		threadTarget != executorTarget || mailboxActivity != executorActivity || mailboxActiveExists != executorActiveExists || mailboxActive != executorActive ||
		(len(events) == 0 && next != requested) || (len(events) != 0 && previous != next) || (hasMore && next >= snapshot) {
		return clientProjectionPage{}, errors.New("projection contradiction")
	}
	return clientProjectionPage{threadID: threadID, requested: requested, next: next, snapshot: snapshot, hasMore: hasMore, events: events, mailbox: normalizedMailbox, queue: queue, dialogs: dialogs, diff: diff, diffComments: comments}, nil
}

func normalizeProjectionEvent(value any, name string) (map[string]any, string, uint64, error) {
	event, err := projectionObject(value, name)
	if err != nil {
		return nil, "", 0, err
	}
	schema, err := projectionUint(event["schema_version"], name+".schema_version")
	if err != nil || schema != 1 {
		return nil, "", 0, errors.New("unsupported event schema")
	}
	threadID, err := projectionString(event["thread_id"], name+".thread_id", false)
	if err != nil {
		return nil, "", 0, err
	}
	sequence, err := projectionUint(event["sequence"], name+".sequence")
	if err != nil {
		return nil, "", 0, err
	}
	normalized := map[string]any{"schema_version": float64(1), "thread_id": threadID, "sequence": event["sequence"]}
	for _, key := range []string{"event_id", "type", "visibility"} {
		normalized[key], err = projectionString(event[key], name+"."+key, false)
		if err != nil {
			return nil, "", 0, err
		}
	}
	for _, key := range []string{"provenance", "payload"} {
		if optional, exists := event[key]; exists {
			normalized[key] = optional
		}
	}
	return normalized, threadID, sequence, nil
}

func normalizeProjectionDialog(value any, index int) (map[string]any, error) {
	name := fmt.Sprintf("dialog %d", index)
	dialog, err := projectionObject(value, name)
	if err != nil {
		return nil, err
	}
	normalized := make(map[string]any, 15)
	for _, key := range []string{"dialog_id", "thread_id", "turn_id", "plugin_id", "invocation_id", "deadline"} {
		normalized[key], err = projectionString(dialog[key], name+"."+key, false)
		if err != nil {
			return nil, err
		}
	}
	for _, key := range []string{"generation", "revision", "requested_sequence"} {
		if _, err = projectionUint(dialog[key], name+"."+key); err != nil {
			return nil, err
		}
		normalized[key] = dialog[key]
	}
	requestType, err := projectionString(dialog["request_type"], name+".request_type", false)
	if err != nil || (requestType != "confirm" && requestType != "input" && requestType != "select") {
		return nil, errors.New("malformed dialog request_type")
	}
	state, err := projectionString(dialog["state"], name+".state", false)
	if err != nil || (state != "pending" && state != "resolved") {
		return nil, errors.New("malformed dialog state")
	}
	normalized["request_type"], normalized["state"] = requestType, state
	request, exists := dialog["request"]
	if !exists {
		return nil, errors.New("dialog.request missing")
	}
	normalized["request"] = request
	if value, exists := dialog["resolved_sequence"]; exists {
		if _, err = projectionUint(value, name+".resolved_sequence"); err != nil {
			return nil, err
		}
		normalized["resolved_sequence"] = value
	}
	for _, key := range []string{"resolver_id"} {
		if value, exists := dialog[key]; exists {
			normalized[key], err = projectionString(value, name+"."+key, true)
			if err != nil {
				return nil, err
			}
		}
	}
	if response, exists := dialog["response"]; exists {
		normalized["response"] = response
	}
	return normalized, nil
}

func projectionObject(value any, name string) (map[string]any, error) {
	object, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an object", name)
	}
	return object, nil
}

func projectionArray(value any, name string) ([]any, error) {
	array, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an array", name)
	}
	return array, nil
}

func projectionString(value any, name string, allowEmpty bool) (string, error) {
	text, ok := value.(string)
	if !ok || (!allowEmpty && text == "") {
		return "", fmt.Errorf("%s must be a non-empty string", name)
	}
	return text, nil
}

func projectionUint(value any, name string) (uint64, error) {
	number, ok := value.(float64)
	if !ok || number < 0 || number != float64(uint64(number)) || number > float64(1<<53-1) {
		return 0, fmt.Errorf("%s must be an unsigned safe integer", name)
	}
	return uint64(number), nil
}

func exactProjectionKeys(object map[string]any, allowed []string, name string) error {
	for key := range object {
		found := false
		for _, candidate := range allowed {
			if key == candidate {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("unknown %s field %q", name, key)
		}
	}
	return nil
}

func projectionTarget(value any) (string, error) {
	target, err := projectionObject(value, "executor target")
	if err != nil {
		return "", err
	}
	if err = exactProjectionKeys(target, []string{"type", "runner_id"}, "executor target"); err != nil {
		return "", err
	}
	targetType, err := projectionString(target["type"], "executor target.type", false)
	if err != nil {
		return "", err
	}
	runner := ""
	if value, exists := target["runner_id"]; exists {
		runner, err = projectionString(value, "executor target.runner_id", true)
		if err != nil {
			return "", err
		}
	}
	return targetType + "\x00" + runner, nil
}

func canonicalJSON(value any) ([]byte, error) {
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(output.Bytes(), []byte{'\n'}), nil
}

func conformanceDigest(value any) string {
	encoded, err := canonicalJSON(value)
	if err != nil {
		panic(err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func unixHTTPClient(socket string) *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}
}
func require(pairs ...string) error {
	for i := 0; i < len(pairs); i += 2 {
		if strings.TrimSpace(pairs[i+1]) == "" {
			return fmt.Errorf("--%s is required", pairs[i])
		}
	}
	return nil
}
func escaped(v string) string     { return url.PathEscape(v) }
func threadPath(id string) string { return "/v1/threads/" + escaped(id) }
func eventsPath(id string, after int64) string {
	return threadPath(id) + "/events?after=" + strconv.FormatInt(after, 10)
}
func projectionPath(id string, after int64, limit int) string {
	return threadPath(id) + "/projection?after=" + strconv.FormatInt(after, 10) + "&limit=" + strconv.Itoa(limit)
}
func queuePath(c clientFlags) string { return threadPath(c.thread) + "/queue/" + escaped(c.queue) }
func turnPath(c clientFlags) string  { return threadPath(c.thread) + "/turns/" + escaped(c.turn) }
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
