package agopluginprotocol

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

const DefaultMaxFrameBytes = 1 << 20

type Decoder struct {
	reader *bufio.Reader
	max    int
}

func NewDecoder(reader io.Reader, maxFrameBytes int) *Decoder {
	if maxFrameBytes <= 0 {
		maxFrameBytes = DefaultMaxFrameBytes
	}
	return &Decoder{reader: bufio.NewReaderSize(reader, maxFrameBytes+1), max: maxFrameBytes}
}

func (d *Decoder) Decode() (Envelope, error) {
	line, err := d.reader.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) || len(line) > d.max+1 || (len(line) == d.max+1 && line[len(line)-1] != '\n') {
		return Envelope{}, rpcError(CodeOverloaded, "JSONL frame exceeds limit")
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return Envelope{}, err
	}
	if errors.Is(err, io.EOF) && len(line) == 0 {
		return Envelope{}, io.EOF
	}
	line = bytes.TrimSuffix(line, []byte{'\n'})
	if len(line) > d.max {
		return Envelope{}, rpcError(CodeOverloaded, "JSONL frame exceeds limit")
	}
	var envelope Envelope
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return Envelope{}, rpcError(CodeInvalidRequest, "invalid JSON envelope")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return Envelope{}, rpcError(CodeInvalidRequest, "multiple values in JSONL frame")
	}
	if envelope.Type != MessageRequest && envelope.Type != MessageResponse && envelope.Type != MessageNotification {
		return Envelope{}, rpcError(CodeInvalidRequest, "unknown message type")
	}
	if (envelope.Type == MessageRequest && (envelope.ID == "" || envelope.Method == "")) || (envelope.Type == MessageNotification && envelope.Method == "") || (envelope.Type == MessageResponse && envelope.ID == "") {
		return Envelope{}, rpcError(CodeInvalidRequest, "missing envelope fields")
	}
	return envelope, nil
}
