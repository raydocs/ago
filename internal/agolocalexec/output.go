package agolocalexec

import (
	"fmt"
	"sync"
)

// CollectedOutput is a bounded representation of a byte stream.
type CollectedOutput struct {
	Head         []byte `json:"head"`
	Tail         []byte `json:"tail"`
	DroppedBytes int64  `json:"dropped_bytes"`
	TotalBytes   int64  `json:"total_bytes"`
}

// OutputCollector retains the beginning and end of a stream without retaining
// its potentially unbounded middle.
type OutputCollector struct {
	mu                   sync.Mutex
	headLimit, tailLimit int
	head, tail           []byte
	total                int64
}

func NewOutputCollector(headBytes, tailBytes int) (*OutputCollector, error) {
	if headBytes <= 0 || tailBytes <= 0 {
		return nil, fmt.Errorf("positive head and tail budgets are required")
	}
	return &OutputCollector{headLimit: headBytes, tailLimit: tailBytes}, nil
}

func (c *OutputCollector) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(p)
	c.total += int64(n)
	if missing := c.headLimit - len(c.head); missing > 0 {
		if missing > len(p) {
			missing = len(p)
		}
		c.head = append(c.head, p[:missing]...)
		p = p[missing:]
	}
	if len(p) >= c.tailLimit {
		c.tail = append(c.tail[:0], p[len(p)-c.tailLimit:]...)
	} else if len(p) > 0 {
		if excess := len(c.tail) + len(p) - c.tailLimit; excess > 0 {
			c.tail = append(c.tail[:0], c.tail[excess:]...)
		}
		c.tail = append(c.tail, p...)
	}
	return n, nil
}

func (c *OutputCollector) Result() CollectedOutput {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := CollectedOutput{Head: append([]byte(nil), c.head...), Tail: append([]byte(nil), c.tail...), TotalBytes: c.total}
	kept := int64(len(r.Head) + len(r.Tail))
	if kept > r.TotalBytes {
		kept = r.TotalBytes
	}
	r.DroppedBytes = r.TotalBytes - kept
	return r
}
