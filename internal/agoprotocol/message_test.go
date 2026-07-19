package agoprotocol

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestMessageInputJSONRoundTripUsesStableTypedShape(t *testing.T) {
	content := []byte("attachment bytes")
	digest := sha256.Sum256(content)
	input := MessageInput{
		Text: "Review these inputs",
		Attachments: []AttachmentRef{{
			AttachmentID: "att-01J2YQZ",
			SHA256:       hex.EncodeToString(digest[:]),
			SizeBytes:    uint64(len(content)),
			MediaType:    "text/plain",
			Filename:     "notes.txt",
		}},
		FileMentions: []RepositoryFileMention{{Path: "internal/agoprotocol/protocol.go"}},
	}

	encoded, err := MarshalMessageInput(input)
	if err != nil {
		t.Fatalf("marshal valid message input: %v", err)
	}
	want := `{"text":"Review these inputs","attachments":[{"attachment_id":"att-01J2YQZ","sha256":"` + hex.EncodeToString(digest[:]) + `","size_bytes":16,"media_type":"text/plain","filename":"notes.txt"}],"file_mentions":[{"path":"internal/agoprotocol/protocol.go"}]}`
	if string(encoded) != want {
		t.Fatalf("encoded message = %s, want %s", encoded, want)
	}

	decoded, err := DecodeMessageInput(encoded)
	if err != nil {
		t.Fatalf("decode valid message input: %v", err)
	}
	if decoded.Text != input.Text || len(decoded.Attachments) != 1 || decoded.Attachments[0] != input.Attachments[0] || len(decoded.FileMentions) != 1 || decoded.FileMentions[0] != input.FileMentions[0] {
		t.Fatalf("decoded message = %#v, want %#v", decoded, input)
	}
}

func TestDecodeMessageInputIsStrict(t *testing.T) {
	tests := map[string]string{
		"unknown field":  `{"text":"hello","workspace":"/tmp"}`,
		"trailing value": `{"text":"hello"}{"text":"again"}`,
		"empty":          `{}`,
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeMessageInput([]byte(raw)); err == nil {
				t.Fatalf("invalid message input %s was accepted", raw)
			}
		})
	}
}

func TestMessageInputValidationEnforcesBoundsAndUniqueReferences(t *testing.T) {
	digest := strings.Repeat("a", 64)
	attachment := AttachmentRef{AttachmentID: "att-1", SHA256: digest, SizeBytes: 1, MediaType: "text/plain", Filename: "a.txt"}
	valid := MessageInput{Text: "hello", Attachments: []AttachmentRef{attachment}, FileMentions: []RepositoryFileMention{{Path: "a.txt"}}}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid message rejected: %v", err)
	}

	tests := map[string]func(*MessageInput){
		"text bytes": func(input *MessageInput) { input.Text = strings.Repeat("x", MaxMessageTextBytes+1) },
		"attachment count": func(input *MessageInput) {
			input.Attachments = make([]AttachmentRef, MaxMessageAttachments+1)
		},
		"aggregate attachment bytes": func(input *MessageInput) {
			input.Attachments = make([]AttachmentRef, 5)
			for index := range input.Attachments {
				input.Attachments[index] = attachment
				input.Attachments[index].AttachmentID += string(rune('a' + index))
				input.Attachments[index].SizeBytes = MaxAttachmentBytes
			}
		},
		"file mention count": func(input *MessageInput) {
			input.FileMentions = make([]RepositoryFileMention, MaxMessageFileMentions+1)
		},
		"duplicate attachment": func(input *MessageInput) { input.Attachments = []AttachmentRef{attachment, attachment} },
		"duplicate file mention": func(input *MessageInput) {
			input.FileMentions = []RepositoryFileMention{{Path: "a.txt"}, {Path: "a.txt"}}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			candidate := valid
			mutate(&candidate)
			if err := candidate.Validate(); err == nil {
				t.Fatal("invalid message input was accepted")
			}
		})
	}
}

func TestValidateRepositoryRelativePathRequiresCanonicalPortablePath(t *testing.T) {
	valid := []string{"README.md", "internal/agoprotocol/message.go", ".github/workflows/test.yml"}
	for _, value := range valid {
		if err := ValidateRepositoryRelativePath(value); err != nil {
			t.Fatalf("valid path %q rejected: %v", value, err)
		}
	}

	invalid := []string{"", ".", "/tmp/a", "../a", "a/../../b", "a/../b", "a//b", "a\\b", "a\x00b", "a/", ".git/config", "a/.git/config", strings.Repeat("x", MaxRepositoryRelativePathBytes+1)}
	for _, value := range invalid {
		if err := ValidateRepositoryRelativePath(value); err == nil {
			t.Fatalf("non-canonical repository path %q was accepted", value)
		}
	}
}

func TestAttachmentRefValidatesIdentityAndMetadata(t *testing.T) {
	valid := AttachmentRef{AttachmentID: "att:01J2YQZ-1", SHA256: strings.Repeat("a", 64), SizeBytes: 42, MediaType: "image/png", Filename: "screen shot.png"}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid attachment rejected: %v", err)
	}

	tests := map[string]func(*AttachmentRef){
		"missing id":        func(ref *AttachmentRef) { ref.AttachmentID = "" },
		"invalid id":        func(ref *AttachmentRef) { ref.AttachmentID = "att id" },
		"long id":           func(ref *AttachmentRef) { ref.AttachmentID = strings.Repeat("a", MaxAttachmentIDBytes+1) },
		"noncanonical hash": func(ref *AttachmentRef) { ref.SHA256 = strings.Repeat("A", 64) },
		"short hash":        func(ref *AttachmentRef) { ref.SHA256 = "abcd" },
		"too large":         func(ref *AttachmentRef) { ref.SizeBytes = MaxAttachmentBytes + 1 },
		"invalid media type": func(ref *AttachmentRef) {
			ref.MediaType = "text/plain; charset=utf-8"
		},
		"noncanonical media type": func(ref *AttachmentRef) { ref.MediaType = "Image/PNG" },
		"path filename":           func(ref *AttachmentRef) { ref.Filename = "dir/file.png" },
		"long filename":           func(ref *AttachmentRef) { ref.Filename = strings.Repeat("x", MaxAttachmentFilenameBytes+1) },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			candidate := valid
			mutate(&candidate)
			if err := candidate.Validate(); err == nil {
				t.Fatal("invalid attachment was accepted")
			}
		})
	}
}

func TestValidateAttachmentBytesBindsImmutableIdentity(t *testing.T) {
	content := []byte("immutable")
	digest := sha256.Sum256(content)
	ref := AttachmentRef{AttachmentID: "att-1", SHA256: hex.EncodeToString(digest[:]), SizeBytes: uint64(len(content)), MediaType: "application/octet-stream", Filename: "data.bin"}
	if err := ValidateAttachmentBytes(ref, content); err != nil {
		t.Fatalf("matching attachment bytes rejected: %v", err)
	}

	changed := append([]byte(nil), content...)
	changed[0] = 'I'
	if err := ValidateAttachmentBytes(ref, changed); err == nil {
		t.Fatal("attachment bytes with a changed digest were accepted")
	}
	ref.SizeBytes++
	if err := ValidateAttachmentBytes(ref, content); err == nil {
		t.Fatal("attachment bytes with a changed size were accepted")
	}
}
