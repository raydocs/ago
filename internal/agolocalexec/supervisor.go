package agolocalexec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

const (
	maxLaunchEnvelopeBytes = 1 << 20
	supervisorKillGrace    = 2 * time.Second
)

// DecodeLaunchEnvelope reads one and only one bounded wire message.
func DecodeLaunchEnvelope(r io.Reader) (LaunchEnvelope, error) {
	limited := &io.LimitedReader{R: r, N: maxLaunchEnvelopeBytes + 1}
	data, err := io.ReadAll(limited)
	if err != nil {
		return LaunchEnvelope{}, fmt.Errorf("read launch envelope: %w", err)
	}
	if len(data) > maxLaunchEnvelopeBytes {
		return LaunchEnvelope{}, fmt.Errorf("launch envelope exceeds %d bytes", maxLaunchEnvelopeBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var envelope LaunchEnvelope
	if err := dec.Decode(&envelope); err != nil {
		return LaunchEnvelope{}, fmt.Errorf("decode launch envelope: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return LaunchEnvelope{}, fmt.Errorf("launch envelope must contain exactly one JSON value")
	}
	if envelope.Version != BrokerWireVersion {
		return LaunchEnvelope{}, fmt.Errorf("unsupported launch envelope version %d", envelope.Version)
	}
	digest, err := envelope.Plan.Digest()
	if err != nil {
		return LaunchEnvelope{}, fmt.Errorf("invalid launch plan: %w", err)
	}
	if digest != envelope.Digest {
		return LaunchEnvelope{}, fmt.Errorf("launch envelope digest mismatch")
	}
	// Base64 expansion plus JSON framing must remain inside the broker's cap.
	if envelope.Plan.Output.HeadBytes+envelope.Plan.Output.TailBytes > maxBrokerResultBytes/4 {
		return LaunchEnvelope{}, fmt.Errorf("output budget is too large")
	}
	return envelope, nil
}

// RunSupervisor owns the sandboxed command from start through group cleanup.
func RunSupervisor(stdin io.Reader, stdout io.Writer, liveness io.Reader) error {
	return RunSupervisorProviderSession(stdin, stdout, liveness, nil, nil, nil, nil)
}

func RunSupervisorSession(stdin io.Reader, stdout io.Writer, liveness, controls io.Reader, events io.Writer) error {
	requests := os.NewFile(6, "provider-requests")
	responses := os.NewFile(7, "provider-responses")
	return RunSupervisorProviderSession(stdin, stdout, liveness, controls, events, requests, responses)
}

func RunSupervisorProviderSession(stdin io.Reader, stdout io.Writer, liveness, controls io.Reader, events io.Writer, providerRequests io.Writer, providerResponses io.Reader) error {
	envelope, err := DecodeLaunchEnvelope(stdin)
	if err != nil {
		return err
	}
	p := envelope.Plan
	for _, dir := range []string{p.SyntheticHome, p.SyntheticTemp} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return fmt.Errorf("create synthetic directory: %w", err)
		}
		if err := os.Chmod(dir, 0700); err != nil {
			return fmt.Errorf("secure synthetic directory: %w", err)
		}
	}
	profile, err := RenderSeatbeltProfile(p)
	if err != nil {
		return fmt.Errorf("render Seatbelt profile: %w", err)
	}
	if seatbeltProfileHash(profile) != p.ProfileHash {
		return fmt.Errorf("Seatbelt profile hash mismatch")
	}
	profilePath := filepath.Join(p.SyntheticTemp, ".ago-seatbelt.sb")
	profileFile, err := os.OpenFile(profilePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("create Seatbelt profile: %w", err)
	}
	if _, err = io.WriteString(profileFile, profile); err == nil {
		err = profileFile.Close()
	} else {
		_ = profileFile.Close()
	}
	if err != nil {
		return fmt.Errorf("write Seatbelt profile: %w", err)
	}
	defer os.Remove(profilePath)

	argv, err := SandboxExecArgv(p, profilePath)
	if err != nil {
		return err
	}
	env, err := LaunchEnvironment(p)
	if err != nil {
		return err
	}
	command := exec.Command(argv[0], argv[1:]...)
	command.Dir, command.Env = p.WorkingDir, env
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	outCollector, _ := NewOutputCollector(p.Output.HeadBytes, p.Output.TailBytes)
	errCollector, _ := NewOutputCollector(p.Output.HeadBytes, p.Output.TailBytes)
	command.Stderr = errCollector
	var childIn io.WriteCloser
	var protocolDone chan error
	if p.Protocol.ID == "" {
		command.Stdin, command.Stdout = bytes.NewReader(p.Stdin), outCollector
	} else {
		if controls == nil || events == nil {
			return fmt.Errorf("session protocol descriptors are required")
		}
		childIn, err = command.StdinPipe()
		if err != nil {
			return err
		}
		childOut, pipeErr := command.StdoutPipe()
		if pipeErr != nil {
			childIn.Close()
			return pipeErr
		}
		go func() { _, _ = childIn.Write(p.Stdin); _, _ = io.Copy(childIn, controls); _ = childIn.Close() }()
		protocolDone = make(chan error, 1)
		go func() { protocolDone <- proxyEventFrames(childOut, events, p.Protocol) }()
		if providerRequests == nil || providerResponses == nil {
			return fmt.Errorf("provider descriptors are required")
		}
		requestFile, ok1 := providerRequests.(*os.File)
		responseFile, ok2 := providerResponses.(*os.File)
		if !ok1 || !ok2 {
			return fmt.Errorf("provider descriptors must be files")
		}
		command.ExtraFiles = []*os.File{requestFile, responseFile}
	}
	if err := command.Start(); err != nil {
		return fmt.Errorf("start sandbox: %w", err)
	}
	// The sandboxed child exclusively owns these inherited endpoints. Keeping
	// supervisor copies open prevents the trusted broker from observing EOF and
	// can deadlock session shutdown.
	if p.Protocol.ID != "" {
		if file, ok := providerRequests.(*os.File); ok {
			_ = file.Close()
		}
		if file, ok := providerResponses.(*os.File); ok {
			_ = file.Close()
		}
	}

	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	dead := time.NewTimer(p.Deadline)
	defer dead.Stop()
	lost := make(chan struct{}, 1)
	go func() { var one [1]byte; _, _ = liveness.Read(one[:]); lost <- struct{}{} }()
	var waitErr error
	select {
	case waitErr = <-done:
		if protocolDone != nil {
			if protocolErr := <-protocolDone; protocolErr != nil {
				waitErr = protocolErr
			}
		}
	case <-dead.C:
		waitErr = terminateProcessGroup(command.Process.Pid, done)
	case <-lost:
		waitErr = terminateProcessGroup(command.Process.Pid, done)
	case protocolErr := <-protocolDone:
		if protocolErr != nil {
			_ = terminateProcessGroup(command.Process.Pid, done)
			return protocolErr
		}
		waitErr = <-done
	}
	result := BrokerResult{Version: BrokerWireVersion, Digest: envelope.Digest, ExitCode: processExitCode(waitErr), Stdout: outCollector.Result(), Stderr: errCollector.Result()}
	if err := json.NewEncoder(stdout).Encode(result); err != nil {
		return fmt.Errorf("encode broker result: %w", err)
	}
	return nil
}

func proxyEventFrames(r io.Reader, w io.Writer, budget ProtocolBudget) error {
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
		var raw map[string]json.RawMessage
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.DisallowUnknownFields()
		if dec.Decode(&raw) != nil {
			return fmt.Errorf("malformed event frame")
		}
		count++
		total += int64(len(line) + 1)
		if count > budget.MaxEvents || total > budget.MaxEventBytes {
			return fmt.Errorf("event stream exceeds budget")
		}
		if _, err := w.Write(append(line, '\n')); err != nil {
			return err
		}
	}
}

func terminateProcessGroup(pid int, done <-chan error) error {
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	timer := time.NewTimer(supervisorKillGrace)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		return <-done
	}
}

func processExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if status, ok := exit.Sys().(syscall.WaitStatus); ok {
			if status.Signaled() {
				return 128 + int(status.Signal())
			}
			return status.ExitStatus()
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return 124
	}
	return 125
}
