package agolocalexec

import "testing"

func TestOutputCollectorPreservesHeadTailAndDroppedCount(t *testing.T) {
	c, err := NewOutputCollector(4, 3)
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"012", "345", "6789"} {
		if _, err := c.Write([]byte(part)); err != nil {
			t.Fatal(err)
		}
	}
	got := c.Result()
	if string(got.Head) != "0123" || string(got.Tail) != "789" || got.DroppedBytes != 3 || got.TotalBytes != 10 {
		t.Fatalf("unexpected result: %+v", got)
	}
}

func TestOutputCollectorRejectsInvalidBudgets(t *testing.T) {
	if _, err := NewOutputCollector(0, 1); err == nil {
		t.Fatal("accepted zero head budget")
	}
}
