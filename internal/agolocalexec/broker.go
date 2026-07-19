package agolocalexec

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	BrokerWireVersion     = 2
	maxBrokerResultBytes  = 1 << 20
	brokerStderrHeadBytes = 16 << 10
	brokerStderrTailBytes = 16 << 10
)

type LaunchEnvelope struct {
	Version int        `json:"version"`
	Digest  string     `json:"digest"`
	Plan    LaunchPlan `json:"plan"`
}

// BrokerSession is the daemon end of the FD3/FD4/FD5 supervisor contract.
type BrokerSession struct {
	cmd            *exec.Cmd
	digest         string
	plan           LaunchPlan
	live, controls *os.File
	events         <-chan []byte
	eventErr       <-chan error
	providerErr    <-chan error
	stdout         *cappedBuffer
	stderr         *OutputCollector
	writeMu        sync.Mutex
	closeOnce      sync.Once
	waitOnce       sync.Once
	done           chan struct{}
	waitErr        error
}

func StartBroker(ctx context.Context, supervisor string, plan LaunchPlan) (*BrokerSession, error) {
	return StartBrokerWithProvider(ctx, supervisor, plan, nil)
}

func StartBrokerWithProvider(ctx context.Context, supervisor string, plan LaunchPlan, provider ProviderCallback) (*BrokerSession, error) {
	if !filepath.IsAbs(supervisor) || filepath.Clean(supervisor) != supervisor {
		return nil, fmt.Errorf("supervisor must be canonical and absolute")
	}
	envelope, err := NewLaunchEnvelope(plan)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		return nil, err
	}
	payload = append(payload, '\n')
	liveR, liveW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	controlR, controlW, err := os.Pipe()
	if err != nil {
		liveR.Close()
		liveW.Close()
		return nil, err
	}
	eventR, eventW, err := os.Pipe()
	if err != nil {
		liveR.Close()
		liveW.Close()
		controlR.Close()
		controlW.Close()
		return nil, err
	}
	providerReqR, providerReqW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	providerRespR, providerRespW, err := os.Pipe()
	if err != nil {
		providerReqR.Close()
		providerReqW.Close()
		return nil, err
	}
	cmd := exec.Command(supervisor)
	cmd.Stdin = bytes.NewReader(payload)
	cmd.ExtraFiles = []*os.File{liveR, controlR, eventW, providerReqW, providerRespR}
	out := &cappedBuffer{limit: maxBrokerResultBytes}
	errOut, _ := NewOutputCollector(brokerStderrHeadBytes, brokerStderrTailBytes)
	cmd.Stdout, cmd.Stderr = out, errOut
	if err := cmd.Start(); err != nil {
		liveR.Close()
		liveW.Close()
		controlR.Close()
		controlW.Close()
		eventR.Close()
		eventW.Close()
		return nil, fmt.Errorf("start supervisor: %w", err)
	}
	liveR.Close()
	controlR.Close()
	eventW.Close()
	providerReqW.Close()
	providerRespR.Close()
	providerErrors := make(chan error, 1)
	if provider != nil {
		go func() {
			providerErrors <- serveProviderBroker(ctx, providerReqR, providerRespW, provider)
			_ = providerReqR.Close()
			_ = providerRespW.Close()
		}()
	} else {
		providerReqR.Close()
		providerRespW.Close()
		providerErrors <- nil
	}
	lines := make(chan []byte)
	scanErr := make(chan error, 1)
	go func() {
		defer close(lines)
		defer eventR.Close()
		scanErr <- scanFrames(eventR, plan.Protocol, lines)
		close(scanErr)
	}()
	s := &BrokerSession{cmd: cmd, digest: envelope.Digest, plan: plan, live: liveW, controls: controlW, events: lines, eventErr: scanErr, providerErr: providerErrors, stdout: out, stderr: errOut, done: make(chan struct{})}
	go func() {
		select {
		case <-ctx.Done():
		case <-s.done:
			return
		}
		if plan.Protocol.ID != "" {
			_ = s.Send(context.Background(), []byte(`{"type":"abort"}`))
			time.Sleep(plan.Protocol.AbortGrace)
		}
		_ = s.live.Close()
	}()
	return s, nil
}

func scanFrames(r io.Reader, budget ProtocolBudget, out chan<- []byte) error {
	if budget.ID == "" {
		_, err := io.Copy(io.Discard, r)
		return err
	}
	reader := newBoundedFrameReader(r, budget.MaxFrameBytes)
	count := 0
	var total int64
	for {
		line, err := reader.Read()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("event %w", err)
		}
		count++
		total += int64(len(line) + 1)
		if count > budget.MaxEvents || total > budget.MaxEventBytes {
			return fmt.Errorf("event stream exceeds budget")
		}
		out <- append([]byte(nil), line...)
	}
}

func (s *BrokerSession) Events() <-chan []byte { return s.events }
func (s *BrokerSession) Send(ctx context.Context, line []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if len(line) > s.plan.Protocol.MaxFrameBytes {
		return fmt.Errorf("control frame exceeds budget")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if _, err := s.controls.Write(append(append([]byte(nil), line...), '\n')); err != nil {
		return err
	}
	return nil
}
func (s *BrokerSession) CloseInput() error {
	var err error
	s.closeOnce.Do(func() { err = s.controls.Close() })
	return err
}
func (s *BrokerSession) Wait() error {
	s.waitOnce.Do(func() { s.waitErr = s.wait() })
	return s.waitErr
}
func (s *BrokerSession) wait() error {
	defer close(s.done)
	scanErr := <-s.eventErr
	err := s.cmd.Wait()
	s.live.Close()
	s.CloseInput()
	providerErr := <-s.providerErr
	if scanErr != nil {
		return scanErr
	}
	if providerErr != nil {
		return fmt.Errorf("provider broker failed: %w", providerErr)
	}
	if err != nil {
		return fmt.Errorf("supervisor failed: %w (stderr: %+v)", err, s.stderr.Result())
	}
	if s.stdout.exceeded {
		return fmt.Errorf("supervisor result exceeds budget")
	}
	dec := json.NewDecoder(bytes.NewReader(s.stdout.Bytes()))
	dec.DisallowUnknownFields()
	var result BrokerResult
	if err := dec.Decode(&result); err != nil {
		return err
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return fmt.Errorf("supervisor returned more than one result")
	}
	if result.Version != BrokerWireVersion || result.Digest != s.digest {
		return fmt.Errorf("unverified supervisor result")
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("sandboxed sidecar exited %d (stderr=%q)", result.ExitCode, joinedOutput(result.Stderr))
	}
	return nil
}

type BrokerResult struct {
	Version  int             `json:"version"`
	Digest   string          `json:"digest"`
	ExitCode int             `json:"exit_code"`
	Stdout   CollectedOutput `json:"stdout"`
	Stderr   CollectedOutput `json:"stderr"`
}

type cappedBuffer struct {
	bytes.Buffer
	limit    int
	exceeded bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := b.limit - b.Len()
	if remaining < len(p) {
		b.exceeded = true
		if remaining > 0 {
			_, _ = b.Buffer.Write(p[:remaining])
		}
		return n, nil
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}

func NewLaunchEnvelope(plan LaunchPlan) (LaunchEnvelope, error) {
	digest, err := plan.Digest()
	if err != nil {
		return LaunchEnvelope{}, fmt.Errorf("unsupported launch plan: %w", err)
	}
	return LaunchEnvelope{Version: BrokerWireVersion, Digest: digest, Plan: plan.Clone()}, nil
}

// BindSeatbeltProfile binds a launch plan to the exact generated profile
// template. Parameter values remain separately bound by the launch digest.
func BindSeatbeltProfile(plan LaunchPlan) (LaunchPlan, error) {
	plan.ProfileHash = strings.Repeat("0", sha256.Size*2)
	profile, err := RenderSeatbeltProfile(plan)
	if err != nil {
		return LaunchPlan{}, err
	}
	plan.ProfileHash = seatbeltProfileHash(profile)
	return plan, nil
}

// RenderSeatbeltProfile renders a stable, deny-default parameterized profile.
// Synthetic directories are writable regardless of caller root duplication.
func RenderSeatbeltProfile(plan LaunchPlan) (string, error) {
	if _, err := plan.Digest(); err != nil {
		return "", err
	}
	reads := normalizedRoots(append(append([]string{}, plan.ReadRoots...), plan.SyntheticHome, plan.SyntheticTemp))
	writes := normalizedRoots(append(append([]string{}, plan.WriteRoots...), plan.SyntheticHome, plan.SyntheticTemp))
	var b strings.Builder
	b.WriteString("(version 1)\n(deny default)\n(deny network*)\n(allow process-fork)\n(allow process-exec)\n(allow signal)\n(allow mach-lookup)\n(allow ipc-posix-shm)\n(allow file-read-data (literal \"/\"))\n(allow file-read-metadata)\n(allow sysctl-read)\n")
	for index := range reads {
		fmt.Fprintf(&b, "(allow file-read* (subpath (param \"AGO_READ_%03d\")))\n", index)
	}
	for index := range writes {
		fmt.Fprintf(&b, "(allow file-write* (subpath (param \"AGO_WRITE_%03d\")))\n", index)
	}
	return b.String(), nil
}

func seatbeltProfileHash(profile string) string {
	digest := sha256.Sum256([]byte(profile))
	return fmt.Sprintf("%x", digest[:])
}

func normalizedRoots(roots []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(roots))
	for _, r := range roots {
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	sort.Strings(out)
	return out
}

func SandboxExecArgv(plan LaunchPlan, profilePath string) ([]string, error) {
	if _, err := plan.Digest(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(profilePath) || filepath.Clean(profilePath) != profilePath {
		return nil, fmt.Errorf("profile path must be canonical and absolute")
	}
	reads := normalizedRoots(append(append([]string{}, plan.ReadRoots...), plan.SyntheticHome, plan.SyntheticTemp))
	writes := normalizedRoots(append(append([]string{}, plan.WriteRoots...), plan.SyntheticHome, plan.SyntheticTemp))
	argv := []string{"/usr/bin/sandbox-exec"}
	for index, root := range reads {
		argv = append(argv, "-D", fmt.Sprintf("AGO_READ_%03d=%s", index, root))
	}
	for index, root := range writes {
		argv = append(argv, "-D", fmt.Sprintf("AGO_WRITE_%03d=%s", index, root))
	}
	argv = append(argv, "-f", profilePath, plan.Executable)
	return append(argv, plan.Arguments...), nil
}

// LaunchEnvironment deliberately starts from an empty environment.
func LaunchEnvironment(plan LaunchPlan) ([]string, error) {
	if _, err := plan.Digest(); err != nil {
		return nil, err
	}
	values := make(map[string]string, len(plan.Environment)+2)
	for k, v := range plan.Environment {
		values[k] = v
	}
	values["HOME"] = plan.SyntheticHome
	values["TMPDIR"] = plan.SyntheticTemp
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys))
	for _, k := range keys {
		env = append(env, k+"="+values[k])
	}
	return env, nil
}

// ExecuteBroker directly starts the injected supervisor and communicates only
// through its standard pipes. It never starts the requested tool itself.
func ExecuteBroker(ctx context.Context, supervisor string, plan LaunchPlan) (BrokerResult, error) {
	if !filepath.IsAbs(supervisor) || filepath.Clean(supervisor) != supervisor {
		return BrokerResult{}, fmt.Errorf("supervisor must be canonical and absolute")
	}
	envelope, err := NewLaunchEnvelope(plan)
	if err != nil {
		return BrokerResult{}, err
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		return BrokerResult{}, fmt.Errorf("encode launch envelope: %w", err)
	}
	payload = append(payload, '\n')
	cmd := exec.Command(supervisor)
	cmd.Stdin = bytes.NewReader(payload)
	liveR, liveW, err := os.Pipe()
	if err != nil {
		return BrokerResult{}, fmt.Errorf("create supervisor liveness pipe: %w", err)
	}
	defer liveW.Close()
	cmd.ExtraFiles = []*os.File{liveR}
	stdout := cappedBuffer{limit: maxBrokerResultBytes}
	cmd.Stdout = &stdout
	errOutput, _ := NewOutputCollector(brokerStderrHeadBytes, brokerStderrTailBytes)
	cmd.Stderr = errOutput
	if err := cmd.Start(); err != nil {
		_ = liveR.Close()
		return BrokerResult{}, fmt.Errorf("start supervisor: %w", err)
	}
	_ = liveR.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err = <-done:
	case <-ctx.Done():
		_ = liveW.Close()
		err = <-done
		if err != nil {
			return BrokerResult{}, fmt.Errorf("%w (supervisor cleanup: %v; stderr: %+v)", ctx.Err(), err, errOutput.Result())
		}
		return BrokerResult{}, ctx.Err()
	}
	if err != nil {
		return BrokerResult{}, fmt.Errorf("supervisor failed: %w (stderr: %+v)", err, errOutput.Result())
	}
	if stdout.exceeded {
		return BrokerResult{}, fmt.Errorf("supervisor result exceeds %d bytes", maxBrokerResultBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	dec.DisallowUnknownFields()
	var result BrokerResult
	if err := dec.Decode(&result); err != nil {
		return BrokerResult{}, fmt.Errorf("decode supervisor result: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return BrokerResult{}, fmt.Errorf("supervisor returned more than one result")
	}
	if result.Version != BrokerWireVersion {
		return BrokerResult{}, fmt.Errorf("unsupported supervisor result version %d", result.Version)
	}
	if result.Digest != envelope.Digest {
		return BrokerResult{}, fmt.Errorf("supervisor result digest mismatch")
	}
	if err := validateResultOutput(result.Stdout, plan.Output); err != nil {
		return BrokerResult{}, fmt.Errorf("stdout result: %w", err)
	}
	if err := validateResultOutput(result.Stderr, plan.Output); err != nil {
		return BrokerResult{}, fmt.Errorf("stderr result: %w", err)
	}
	return result, nil
}

func validateResultOutput(o CollectedOutput, b OutputBudget) error {
	if len(o.Head) > b.HeadBytes || len(o.Tail) > b.TailBytes {
		return fmt.Errorf("output exceeds negotiated budget")
	}
	if o.TotalBytes < 0 || o.DroppedBytes < 0 || o.DroppedBytes != o.TotalBytes-int64(len(o.Head)+len(o.Tail)) {
		return fmt.Errorf("inconsistent output byte counts")
	}
	return nil
}
