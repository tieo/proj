package compact

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTranscript(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const (
	request = `{"type":"assistant","timestamp":"2026-09-28T10:00:00Z","message":{"model":"claude-opus-5-5","usage":{"input_tokens":2,"cache_read_input_tokens":300000,"cache_creation_input_tokens":1000}}}`
	limit   = `{"type":"assistant","timestamp":"2026-09-28T10:05:00Z","message":{"model":"<synthetic>","usage":{"input_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`
	summary = `{"type":"system","subtype":"compact_boundary","timestamp":"2026-09-28T10:56:00Z"}`
)

func TestLastRequestReadsContextOfTheLastAnsweredRequest(t *testing.T) {
	r, err := LastRequest(writeTranscript(t, `{"type":"user"}`, request, limit, `{"type":"attachment"}`))
	if err != nil {
		t.Fatal(err)
	}
	if r.Context != 301002 || r.Compacted || !r.At.Equal(time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("got %+v", r)
	}
}

func TestLastRequestSeesACompactionAfterIt(t *testing.T) {
	r, _ := LastRequest(writeTranscript(t, request, summary))
	if !r.Compacted {
		t.Fatal("compaction after the request not seen")
	}
}

func TestDueOnlyInsideTheWindowAndAboveTheThreshold(t *testing.T) {
	at := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	r := Request{At: at, Context: 300000}
	cases := []struct {
		idle  time.Duration
		above int
		want  bool
	}{
		{54 * time.Minute, 250000, false},
		{55 * time.Minute, 250000, true},
		{59 * time.Minute, 250000, true},
		{60 * time.Minute, 250000, false}, // cache already gone, nothing left to save
		{56 * time.Minute, 400000, false},
	}
	for _, c := range cases {
		if got := Due(r, at.Add(c.idle), 55*time.Minute, c.above); got != c.want {
			t.Errorf("idle %s above %d: got %v", c.idle, c.above, got)
		}
	}
	r.Compacted = true
	if Due(r, at.Add(56*time.Minute), 55*time.Minute, 250000) {
		t.Error("compacted session compacted again")
	}
}
