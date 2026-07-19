package agoprotocol

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	pathpkg "path"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	MaxMessageInputBytes                  = 512 << 10
	MaxMessageTextBytes                   = 256 << 10
	MaxMessageAttachments                 = 16
	MaxMessageFileMentions                = 64
	MaxMessageAttachmentBytes      uint64 = 16 << 20
	MaxAttachmentBytes             uint64 = 4 << 20
	MaxAttachmentIDBytes                  = 128
	MaxAttachmentMediaTypeBytes           = 127
	MaxAttachmentFilenameBytes            = 255
	MaxRepositoryRelativePathBytes        = 4096
)

var attachmentIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)

// MessageInput is the canonical user-authored message envelope. Attachments are
// immutable references whose bytes are persisted and verified outside this
// package; file mentions are relative to the thread's repository binding.
type MessageInput struct {
	Text         string                  `json:"text,omitempty"`
	Attachments  []AttachmentRef         `json:"attachments,omitempty"`
	FileMentions []RepositoryFileMention `json:"file_mentions,omitempty"`
}

type AttachmentRef struct {
	AttachmentID string `json:"attachment_id"`
	SHA256       string `json:"sha256"`
	SizeBytes    uint64 `json:"size_bytes"`
	MediaType    string `json:"media_type"`
	Filename     string `json:"filename"`
}

type RepositoryFileMention struct {
	Path string `json:"path"`
}

func (input MessageInput) Validate() error {
	if !utf8.ValidString(input.Text) || strings.IndexByte(input.Text, 0) >= 0 {
		return fmt.Errorf("message text must be valid UTF-8 without NUL bytes")
	}
	if len(input.Text) > MaxMessageTextBytes {
		return fmt.Errorf("message text must not exceed %d bytes", MaxMessageTextBytes)
	}
	if input.Text == "" && len(input.Attachments) == 0 && len(input.FileMentions) == 0 {
		return fmt.Errorf("message must contain text, an attachment, or a file mention")
	}
	if len(input.Attachments) > MaxMessageAttachments {
		return fmt.Errorf("message must not contain more than %d attachments", MaxMessageAttachments)
	}
	if len(input.FileMentions) > MaxMessageFileMentions {
		return fmt.Errorf("message must not contain more than %d file mentions", MaxMessageFileMentions)
	}

	attachmentIDs := make(map[string]struct{}, len(input.Attachments))
	var attachmentBytes uint64
	for index, attachment := range input.Attachments {
		if err := attachment.Validate(); err != nil {
			return fmt.Errorf("attachment %d: %w", index, err)
		}
		if _, duplicate := attachmentIDs[attachment.AttachmentID]; duplicate {
			return fmt.Errorf("attachment_id %q is duplicated", attachment.AttachmentID)
		}
		attachmentIDs[attachment.AttachmentID] = struct{}{}
		attachmentBytes += attachment.SizeBytes
	}
	if attachmentBytes > MaxMessageAttachmentBytes {
		return fmt.Errorf("message attachments must not exceed %d bytes", MaxMessageAttachmentBytes)
	}

	mentionedPaths := make(map[string]struct{}, len(input.FileMentions))
	for index, mention := range input.FileMentions {
		if err := mention.Validate(); err != nil {
			return fmt.Errorf("file mention %d: %w", index, err)
		}
		if _, duplicate := mentionedPaths[mention.Path]; duplicate {
			return fmt.Errorf("file mention path %q is duplicated", mention.Path)
		}
		mentionedPaths[mention.Path] = struct{}{}
	}
	return nil
}

func (ref AttachmentRef) Validate() error {
	if len(ref.AttachmentID) == 0 || len(ref.AttachmentID) > MaxAttachmentIDBytes || !attachmentIDPattern.MatchString(ref.AttachmentID) {
		return fmt.Errorf("attachment_id must be a 1-%d byte ASCII identifier", MaxAttachmentIDBytes)
	}
	if len(ref.SHA256) != sha256.Size*2 || strings.ToLower(ref.SHA256) != ref.SHA256 {
		return fmt.Errorf("sha256 must be a lowercase hexadecimal SHA-256 digest")
	}
	if _, err := hex.DecodeString(ref.SHA256); err != nil {
		return fmt.Errorf("sha256 must be a lowercase hexadecimal SHA-256 digest")
	}
	if ref.SizeBytes > MaxAttachmentBytes {
		return fmt.Errorf("attachment must not exceed %d bytes", MaxAttachmentBytes)
	}
	if err := validateMediaType(ref.MediaType); err != nil {
		return err
	}
	if err := validateAttachmentFilename(ref.Filename); err != nil {
		return err
	}
	return nil
}

func (mention RepositoryFileMention) Validate() error {
	return ValidateRepositoryRelativePath(mention.Path)
}

// ValidateRepositoryRelativePath accepts only an already-canonical slash path.
// It never cleans or rewrites user input and excludes Git administrative data.
func ValidateRepositoryRelativePath(value string) error {
	if value == "" || len(value) > MaxRepositoryRelativePathBytes || !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 || strings.Contains(value, `\`) || pathpkg.IsAbs(value) || pathpkg.Clean(value) != value || value == "." || strings.HasSuffix(value, "/") {
		return fmt.Errorf("path %q must be a canonical repository-relative path", value)
	}
	for _, component := range strings.Split(value, "/") {
		if component == "" || component == "." || component == ".." || strings.EqualFold(component, ".git") {
			return fmt.Errorf("path %q must be a canonical repository-relative path outside .git", value)
		}
	}
	return nil
}

// ValidateAttachmentBytes verifies that persisted bytes match an immutable
// attachment reference without performing I/O or changing either input.
func ValidateAttachmentBytes(ref AttachmentRef, content []byte) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	if uint64(len(content)) != ref.SizeBytes {
		return fmt.Errorf("attachment size is %d bytes, expected %d", len(content), ref.SizeBytes)
	}
	digest := sha256.Sum256(content)
	if hex.EncodeToString(digest[:]) != ref.SHA256 {
		return fmt.Errorf("attachment bytes do not match sha256 identity")
	}
	return nil
}

func DecodeMessageInput(raw []byte) (MessageInput, error) {
	if len(raw) == 0 || len(raw) > MaxMessageInputBytes {
		return MessageInput{}, fmt.Errorf("message input must contain at most %d bytes", MaxMessageInputBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var input MessageInput
	if err := decoder.Decode(&input); err != nil {
		return MessageInput{}, fmt.Errorf("decode message input: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return MessageInput{}, err
	}
	if err := input.Validate(); err != nil {
		return MessageInput{}, err
	}
	return input, nil
}

func MarshalMessageInput(input MessageInput) (json.RawMessage, error) {
	if err := input.Validate(); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("encode message input: %w", err)
	}
	if len(encoded) > MaxMessageInputBytes {
		return nil, fmt.Errorf("message input must not exceed %d encoded bytes", MaxMessageInputBytes)
	}
	return encoded, nil
}

func validateMediaType(value string) error {
	if value == "" || len(value) > MaxAttachmentMediaTypeBytes || value != strings.ToLower(value) {
		return fmt.Errorf("media_type must be a canonical lowercase MIME type")
	}
	mediaType, parameters, err := mime.ParseMediaType(value)
	if err != nil || mediaType != value || len(parameters) != 0 || !strings.Contains(mediaType, "/") {
		return fmt.Errorf("media_type must be a canonical lowercase MIME type without parameters")
	}
	return nil
}

func validateAttachmentFilename(value string) error {
	if value == "" || len(value) > MaxAttachmentFilenameBytes || !utf8.ValidString(value) || value == "." || value == ".." || strings.ContainsAny(value, "/\\") {
		return fmt.Errorf("filename must be a 1-%d byte UTF-8 base name", MaxAttachmentFilenameBytes)
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return fmt.Errorf("filename must not contain control characters")
		}
	}
	return nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("message input must contain exactly one JSON value")
		}
		return fmt.Errorf("decode trailing message input: %w", err)
	}
	return nil
}
