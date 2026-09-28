// Package compact decides when an idle session should be compacted.
//
// Every request re-reads a session's whole context, most of it from the prompt
// cache at a tenth of the input price, which is cheap per request and ruinous
// in total once a session has grown to several hundred thousand tokens. A
// session left alone long enough loses that cache: Claude Code writes it with a
// one-hour lifetime, refreshed on every hit, so the first request after an
// hour away writes the whole context again at twice the input price. Compacting
// shortly before the hour is up reads the context once more while it is still
// cached and leaves a small summary to continue from, whichever way the session
// is picked up again.
package compact

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"time"
)

// CacheLifetime is how long a cached context survives without a request.
// Measured on this machine's transcripts rather than taken from the docs alone:
// requests after 55 to 60 idle minutes still read 95-100% of their context
// from cache, after 62 minutes none do.
const CacheLifetime = time.Hour

// Request is the last API request a transcript records.
type Request struct {
	At time.Time
	// Context is the tokens the request sent: uncached input plus what was
	// read from and written to the cache.
	Context int
	// Compacted reports that a compaction was recorded after the request, so
	// the context it measured is already gone.
	Compacted bool
}

// LastRequest reads the transcript backwards in a fixed window, as
// doner.LastReply does and for the same reason: transcripts of long sessions
// run to hundreds of megabytes and the answer is in the last few records.
func LastRequest(path string) (Request, error) {
	f, err := os.Open(path)
	if err != nil {
		return Request{}, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return Request{}, err
	}
	const window = 4 << 20
	start := fi.Size() - window
	if start < 0 {
		start = 0
	}
	buf := make([]byte, fi.Size()-start)
	if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
		return Request{}, err
	}
	lines := strings.Split(string(buf), "\n")
	if start > 0 && len(lines) > 0 {
		lines = lines[1:]
	}
	compacted := false
	for i := len(lines) - 1; i >= 0; i-- {
		var rec struct {
			Type      string `json:"type"`
			Subtype   string `json:"subtype"`
			Timestamp string `json:"timestamp"`
			Message   struct {
				Model string `json:"model"`
				Usage *struct {
					Input       int `json:"input_tokens"`
					CacheRead   int `json:"cache_read_input_tokens"`
					CacheCreate int `json:"cache_creation_input_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(lines[i]), &rec) != nil {
			continue
		}
		if rec.Type == "system" && rec.Subtype == "compact_boundary" {
			compacted = true
			continue
		}
		// A synthetic message is Claude Code reporting an error such as a usage
		// limit, not a request the API answered.
		if rec.Type != "assistant" || rec.Message.Usage == nil || rec.Message.Model == "<synthetic>" {
			continue
		}
		at, _ := time.Parse(time.RFC3339, rec.Timestamp)
		u := rec.Message.Usage
		return Request{At: at, Context: u.Input + u.CacheRead + u.CacheCreate, Compacted: compacted}, nil
	}
	return Request{}, nil
}

// Due reports whether a session whose last request was r should be compacted
// now: idle for at least after, its cache not yet expired, and its context
// above the threshold.
func Due(r Request, now time.Time, after time.Duration, above int) bool {
	if r.At.IsZero() || r.Compacted || r.Context <= above {
		return false
	}
	idle := now.Sub(r.At)
	return idle >= after && idle < CacheLifetime
}
