package agolocalexec

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

type NetworkMode string

const (
	NetworkDisabled NetworkMode = "disabled"
	NetworkAllowed  NetworkMode = "allowed"
)

type OutputBudget struct {
	HeadBytes int `json:"head_bytes"`
	TailBytes int `json:"tail_bytes"`
}

type ProtocolBudget struct {
	ID            string        `json:"id,omitempty"`
	MaxFrameBytes int           `json:"max_frame_bytes,omitempty"`
	MaxEvents     int           `json:"max_events,omitempty"`
	MaxEventBytes int64         `json:"max_event_bytes,omitempty"`
	AbortGrace    time.Duration `json:"abort_grace_nanoseconds,omitempty"`
}

type LaunchPlan struct {
	Origin        string            `json:"origin"`
	Executable    string            `json:"executable"`
	Arguments     []string          `json:"arguments"`
	Stdin         []byte            `json:"stdin"`
	WorkingDir    string            `json:"working_dir"`
	Environment   map[string]string `json:"environment"`
	ReadRoots     []string          `json:"read_roots"`
	WriteRoots    []string          `json:"write_roots"`
	SyntheticHome string            `json:"synthetic_home"`
	SyntheticTemp string            `json:"synthetic_temp"`
	ProfileID     string            `json:"profile_id"`
	ProfileHash   string            `json:"profile_hash"`
	Network       NetworkMode       `json:"network"`
	TTY           bool              `json:"tty"`
	Deadline      time.Duration     `json:"deadline_nanoseconds"`
	Output        OutputBudget      `json:"output"`
	Protocol      ProtocolBudget    `json:"protocol"`
	ApprovalNonce string            `json:"approval_nonce"`
}

func (plan LaunchPlan) Clone() LaunchPlan {
	clone := plan
	clone.Arguments = append([]string(nil), plan.Arguments...)
	clone.Stdin = append([]byte(nil), plan.Stdin...)
	clone.ReadRoots = append([]string(nil), plan.ReadRoots...)
	clone.WriteRoots = append([]string(nil), plan.WriteRoots...)
	clone.Environment = make(map[string]string, len(plan.Environment))
	for key, value := range plan.Environment {
		clone.Environment[key] = value
	}
	return clone
}

func (plan LaunchPlan) Digest() (string, error) {
	if err := plan.validate(); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		return "", fmt.Errorf("encode launch plan: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func (plan LaunchPlan) validate() error {
	if strings.TrimSpace(plan.Origin) == "" {
		return fmt.Errorf("launch origin is required")
	}
	if err := canonicalAbsolute("executable", plan.Executable); err != nil {
		return err
	}
	if err := canonicalAbsolute("working directory", plan.WorkingDir); err != nil {
		return err
	}
	for _, root := range append(append([]string(nil), plan.ReadRoots...), plan.WriteRoots...) {
		if err := canonicalAbsolute("filesystem root", root); err != nil {
			return err
		}
	}
	if len(plan.ReadRoots) == 0 {
		return fmt.Errorf("at least one read root is required")
	}
	if err := canonicalAbsolute("synthetic home", plan.SyntheticHome); err != nil {
		return err
	}
	if err := canonicalAbsolute("synthetic temp", plan.SyntheticTemp); err != nil {
		return err
	}
	for key, value := range plan.Environment {
		if strings.TrimSpace(key) == "" || strings.ContainsAny(key, "=\x00") || strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("invalid environment entry %q", key)
		}
	}
	for _, argument := range plan.Arguments {
		if strings.ContainsRune(argument, '\x00') {
			return fmt.Errorf("argument contains NUL")
		}
	}
	if plan.ProfileID != "ago.model.v1" {
		return fmt.Errorf("unsupported sandbox profile %q", plan.ProfileID)
	}
	if strings.TrimSpace(plan.ProfileHash) == "" {
		return fmt.Errorf("sandbox profile hash is required")
	}
	if plan.Network != NetworkDisabled {
		return fmt.Errorf("automatic launch network mode %q is unsupported", plan.Network)
	}
	if plan.TTY {
		return fmt.Errorf("automatic launch TTY is unsupported")
	}
	if plan.Deadline <= 0 {
		return fmt.Errorf("positive deadline is required")
	}
	if plan.Output.HeadBytes <= 0 || plan.Output.TailBytes <= 0 {
		return fmt.Errorf("positive head and tail output budgets are required")
	}
	if plan.Protocol.ID == "" {
		if plan.Protocol != (ProtocolBudget{}) {
			return fmt.Errorf("protocol budget requires an id")
		}
	} else if plan.Protocol.ID != "pi-jsonl-v1" || plan.Protocol.MaxFrameBytes <= 0 || plan.Protocol.MaxFrameBytes > 1<<20 || plan.Protocol.MaxEvents <= 0 || plan.Protocol.MaxEventBytes <= 0 || plan.Protocol.AbortGrace <= 0 {
		return fmt.Errorf("unsupported or invalid sidecar protocol budget")
	}
	if strings.TrimSpace(plan.ApprovalNonce) == "" {
		return fmt.Errorf("one-use approval nonce is required")
	}
	return nil
}

func canonicalAbsolute(field, path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("%s must be canonical and absolute", field)
	}
	return nil
}
