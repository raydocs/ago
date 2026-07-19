package agolocalexec

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
)

const maxProviderFrameBytes = 1 << 20

type ProviderRequest struct {
	Type     string          `json:"type"`
	ID       string          `json:"id"`
	Provider string          `json:"provider"`
	Model    string          `json:"model"`
	Context  json.RawMessage `json:"context"`
	Options  json.RawMessage `json:"options"`
}

type ProviderResponse struct {
	Type    string          `json:"type"`
	ID      string          `json:"id"`
	Delta   string          `json:"delta,omitempty"`
	Message json.RawMessage `json:"message,omitempty"`
	Error   string          `json:"error,omitempty"`
}

// ProviderCallback runs in the trusted Ago process and may use its credentials/network.
// emit may be called repeatedly with delta frames and exactly once with a result.
type ProviderCallback func(context.Context, ProviderRequest, func(ProviderResponse) error) error

type boundedFrameReader struct {
	reader *bufio.Reader
	max    int
}

func newBoundedFrameReader(reader io.Reader, max int) *boundedFrameReader {
	return &boundedFrameReader{reader: bufio.NewReaderSize(reader, max+1), max: max}
}

func (reader *boundedFrameReader) Read() ([]byte, error) {
	frame, err := reader.reader.ReadSlice('\n')
	if err == bufio.ErrBufferFull {
		return nil, fmt.Errorf("frame exceeds budget")
	}
	if err == io.EOF {
		if len(frame) == 0 {
			return nil, io.EOF
		}
		return nil, fmt.Errorf("unterminated frame")
	}
	if err != nil {
		return nil, err
	}
	if len(frame)-1 > reader.max {
		return nil, fmt.Errorf("frame exceeds budget")
	}
	return frame[:len(frame)-1], nil
}

func serveProviderBroker(ctx context.Context, r io.Reader, w io.Writer, callback ProviderCallback) error {
	if callback == nil {
		return fmt.Errorf("provider callback is required")
	}
	frames := newBoundedFrameReader(r, maxProviderFrameBytes)
	var writeMu sync.Mutex
	var cancelMu sync.Mutex
	cancels := make(map[string]context.CancelFunc)
	seen := make(map[string]bool)
	var workers sync.WaitGroup
	workerErr := make(chan error, 1)
	report := func(err error) {
		select {
		case workerErr <- err:
		default:
		}
	}
	defer func() {
		cancelMu.Lock()
		for _, cancel := range cancels {
			cancel()
		}
		cancelMu.Unlock()
		workers.Wait()
	}()
	for {
		select {
		case err := <-workerErr:
			return err
		default:
		}
		line, err := frames.Read()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("provider %w", err)
		}
		var kind struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(line, &kind) != nil {
			return fmt.Errorf("malformed provider request")
		}
		if kind.Type == "cancel" {
			dec := json.NewDecoder(bytes.NewReader(line))
			dec.DisallowUnknownFields()
			var cancel struct {
				Type string `json:"type"`
				ID   string `json:"id"`
			}
			if dec.Decode(&cancel) != nil || dec.Decode(new(any)) != io.EOF || strings.TrimSpace(cancel.ID) == "" {
				return fmt.Errorf("malformed provider cancel")
			}
			cancelMu.Lock()
			fn := cancels[cancel.ID]
			cancelMu.Unlock()
			if fn == nil {
				return fmt.Errorf("cancel references unknown provider request %q", cancel.ID)
			}
			fn()
			continue
		}
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.DisallowUnknownFields()
		var req ProviderRequest
		if dec.Decode(&req) != nil || dec.Decode(new(any)) != io.EOF || req.Type != "inference_request" || strings.TrimSpace(req.ID) == "" || strings.TrimSpace(req.Provider) == "" || strings.TrimSpace(req.Model) == "" || !json.Valid(req.Context) || !json.Valid(req.Options) {
			return fmt.Errorf("malformed provider request")
		}
		requestCtx, cancel := context.WithCancel(ctx)
		cancelMu.Lock()
		if seen[req.ID] {
			cancelMu.Unlock()
			cancel()
			return fmt.Errorf("duplicate provider request id %q", req.ID)
		}
		seen[req.ID] = true
		cancels[req.ID] = cancel
		cancelMu.Unlock()
		workers.Add(1)
		go func(req ProviderRequest, requestCtx context.Context, cancel context.CancelFunc) {
			defer workers.Done()
			defer func() { cancel(); cancelMu.Lock(); delete(cancels, req.ID); cancelMu.Unlock() }()
			terminal := false
			emit := func(response ProviderResponse) error {
				response.ID = req.ID
				if response.Type != "delta" && response.Type != "result" && response.Type != "error" {
					return fmt.Errorf("invalid provider response type")
				}
				if terminal {
					return fmt.Errorf("provider response after terminal frame")
				}
				switch response.Type {
				case "delta":
					if response.Message != nil || response.Error != "" {
						return fmt.Errorf("invalid delta response fields")
					}
				case "result":
					if response.Message == nil || !json.Valid(response.Message) || response.Delta != "" || response.Error != "" {
						return fmt.Errorf("invalid result response fields")
					}
					terminal = true
				case "error":
					if strings.TrimSpace(response.Error) == "" || response.Delta != "" || response.Message != nil {
						return fmt.Errorf("invalid error response fields")
					}
					terminal = true
				}
				encoded, e := json.Marshal(response)
				if e != nil || len(encoded) > maxProviderFrameBytes {
					return fmt.Errorf("provider response exceeds budget")
				}
				writeMu.Lock()
				defer writeMu.Unlock()
				_, e = w.Write(append(encoded, '\n'))
				return e
			}
			if e := callback(requestCtx, req, emit); e != nil && !terminal {
				if emitErr := emit(ProviderResponse{Type: "error", Error: e.Error()}); emitErr != nil {
					report(emitErr)
				}
			}
			if !terminal {
				if e := emit(ProviderResponse{Type: "error", Error: "provider callback returned without terminal response"}); e != nil {
					report(e)
				}
			}
		}(req, requestCtx, cancel)
	}
}
