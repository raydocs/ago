package threadgraph

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
)

const defaultScanBudget = 128 * 1024

// ScanResult describes one incremental transcript graph scan.
type ScanResult struct {
	Events     []Event
	Next       int64
	Scanned    int64
	PartialEnd bool
}

// ScanFile reads complete JSONL records after offset without consuming a partial last line.
func ScanFile(path string, offset int64, context Context, maxPayloadBytes int) (ScanResult, error) {
	file, err := os.Open(path)
	if err != nil {
		return ScanResult{}, err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return ScanResult{}, err
	}
	if offset < 0 || offset > info.Size() {
		offset = 0
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return ScanResult{}, err
	}
	if maxPayloadBytes <= 0 {
		maxPayloadBytes = defaultScanBudget
	}

	result := ScanResult{Next: offset}
	parser := NewParser(context)
	reader := bufio.NewReader(file)
	seen := map[string]struct{}{}
	encodedBytes := 0

	for {
		lineStart := result.Next
		line, readErr := reader.ReadBytes('\n')
		if errors.Is(readErr, io.EOF) && len(line) > 0 {
			result.PartialEnd = true
			break
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return ScanResult{}, readErr
		}
		if len(line) == 0 {
			break
		}

		events := parser.ParseLine(line[:len(line)-1])
		newEvents := make([]Event, 0, len(events))
		lineBytes := 0
		for _, event := range events {
			if _, ok := seen[event.EventID]; ok {
				continue
			}
			encoded, err := json.Marshal(event)
			if err != nil {
				return ScanResult{}, err
			}
			lineBytes += len(encoded)
			newEvents = append(newEvents, event)
		}
		if len(result.Events) > 0 && encodedBytes+lineBytes > maxPayloadBytes {
			result.Next = lineStart
			break
		}
		for _, event := range newEvents {
			seen[event.EventID] = struct{}{}
			result.Events = append(result.Events, event)
		}
		encodedBytes += lineBytes
		result.Next += int64(len(line))
		result.Scanned = result.Next - offset
		if errors.Is(readErr, io.EOF) {
			break
		}
	}
	return result, nil
}
