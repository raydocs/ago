package threadusage

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
)

// ScanResult is the incremental parse output for one transcript file.
type ScanResult struct {
	Records    []Record
	Next       FileCursor
	Scanned    int64
	PartialEnd bool
}

// ScanFile reads only new complete JSONL lines from offset and returns normalized records.
// Incomplete trailing lines are not consumed (offset stays before them).
// If the file shrank or the stored size/mtime is inconsistent, a full rescan is performed.
func ScanFile(path string, prev FileCursor) (ScanResult, error) {
	return scanFile(path, prev, 0)
}

// ScanFileLimited reads at most maxRecords usage rows while advancing across
// complete non-usage lines. It is intended for bounded transcript backfills.
func ScanFileLimited(path string, prev FileCursor, maxRecords int) (ScanResult, error) {
	if maxRecords <= 0 {
		return ScanFile(path, prev)
	}
	return scanFile(path, prev, maxRecords)
}

func scanFile(path string, prev FileCursor, maxRecords int) (ScanResult, error) {
	info, err := os.Stat(path)
	if err != nil {
		return ScanResult{}, err
	}
	size := info.Size()
	mtimeNs := info.ModTime().UnixNano()

	start := prev.Offset
	// Truncate / rewrite / unexpected shrink: rescan from the beginning.
	if size < prev.Offset || (prev.Size > 0 && size < prev.Size) {
		start = 0
	}

	file, err := os.Open(path)
	if err != nil {
		return ScanResult{}, err
	}
	defer file.Close()

	if start > 0 {
		if _, err := file.Seek(start, io.SeekStart); err != nil {
			return ScanResult{}, err
		}
	}

	reader := bufio.NewReaderSize(file, 64*1024)
	var (
		records    []Record
		consumed   int64 = start
		partialEnd bool
	)

	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			// Incomplete final line: do not advance past it.
			if err == io.EOF && !bytes.HasSuffix(line, []byte("\n")) {
				partialEnd = true
				break
			}
			if rec, ok := ParseLine(line); ok {
				records = append(records, rec)
			}
			consumed += int64(len(line))
			if maxRecords > 0 && len(records) >= maxRecords {
				break
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return ScanResult{}, err
		}
	}

	return ScanResult{
		Records: Dedup(records),
		Next: FileCursor{
			Offset:  consumed,
			Size:    size,
			MtimeNs: mtimeNs,
		},
		Scanned:    consumed - start,
		PartialEnd: partialEnd,
	}, nil
}

// ScanTranscript is a convenience that loads cursor state, scans, and returns records + next cursor.
// It does not persist the cursor; callers should StoreCursor after a successful upload/spool.
func ScanTranscript(statePath, transcriptPath string) (ScanResult, error) {
	if transcriptPath == "" {
		return ScanResult{}, fmt.Errorf("transcript path is empty")
	}
	prev, err := LoadCursor(statePath, transcriptPath)
	if err != nil {
		return ScanResult{}, err
	}
	return ScanFile(transcriptPath, prev)
}
