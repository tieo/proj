package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/tieo/proj/internal/compact"
	"github.com/tieo/proj/internal/config"
	"github.com/tieo/proj/internal/doner"
	"github.com/tieo/proj/internal/paseo"
)

// Idle compaction sends /compact to a session that has sat idle long enough
// that its prompt cache is about to expire, if its context is large. See
// package compact for why that is cheaper than leaving it.
//
// It covers every Paseo agent, doner-tagged or not: the cost it avoids is paid
// by any long session left alone for an hour.
//
// Each compaction is logged with the context it replaced, so --report can show
// afterwards what the compaction left and what the next request actually read
// from cache. Whether this pays off is a question the log answers, not the
// model it was derived from.

var (
	idleCompactDryRun bool
	idleCompactReport bool
	idleCompactCmd    = &cobra.Command{
		Use:   "idle-compact",
		Short: "compact large idle sessions before their prompt cache expires",
		Long: `Send /compact to every idle Paseo agent whose last request is older than the
configured idle time but younger than the one-hour cache lifetime, and whose
context was above the configured size.

Run from a timer every minute. --report shows each compaction so far and what
the session's next request read from cache.`,
		Args: cobra.NoArgs,
		RunE: runIdleCompact,
	}
)

func init() {
	idleCompactCmd.Flags().BoolVar(&idleCompactDryRun, "dry-run", false, "report what would be compacted without sending anything")
	idleCompactCmd.Flags().BoolVar(&idleCompactReport, "report", false, "show past compactions and their effect")
	rootCmd.AddCommand(idleCompactCmd)
}

// compactionLogPath holds one JSON line per compaction sent.
func compactionLogPath() string {
	return filepath.Join(filepath.Dir(sweepStampPath()), "idle-compact.jsonl")
}

type compactionEntry struct {
	At         time.Time `json:"at"`
	Agent      string    `json:"agent"`
	Project    string    `json:"project"`
	Transcript string    `json:"transcript"`
	// LastRequest is when the session's last request ran; a session is
	// compacted once per idle stretch, keyed on it.
	LastRequest time.Time `json:"lastRequest"`
	Context     int       `json:"context"`
}

func loadCompactions() []compactionEntry {
	var out []compactionEntry
	f, err := os.Open(compactionLogPath())
	if err != nil {
		return nil
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		var e compactionEntry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	return out
}

func appendCompaction(e compactionEntry) error {
	p := compactionLogPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = f.Write(append(data, '\n'))
	return err
}

func runIdleCompact(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if idleCompactReport {
		return reportCompactions()
	}
	ic := cfg.Daemon.IdleCompact
	if !ic.Enabled || !paseo.Available() {
		return nil
	}
	after, above := ic.AfterDuration(), ic.AboveTokens()
	root := claudeRoot(cfg.Claude.Home)
	home := paseo.Home()
	done := map[string]bool{}
	for _, e := range loadCompactions() {
		done[e.Agent+"@"+e.LastRequest.Format(time.RFC3339)] = true
	}
	now := time.Now()
	for _, a := range paseo.Agents() {
		if !a.Idle() {
			continue
		}
		dir := a.Dir()
		sid := paseo.SessionID(home, a.ID, dir)
		if sid == "" {
			continue
		}
		path := doner.TranscriptPath(root, dir, sid)
		r, err := compact.LastRequest(path)
		if err != nil || !compact.Due(r, now, after, above) {
			continue
		}
		if done[a.ID+"@"+r.At.Format(time.RFC3339)] {
			continue
		}
		// A session stopped by a usage limit cannot compact either; the
		// request would be refused like the one that stopped it.
		if reply, _, err := doner.LastReply(path); err == nil && doner.IsAPIError(reply) {
			continue
		}
		fmt.Printf("%s: idle %s with %dk context\n", lastPathSegment(dir), now.Sub(r.At).Round(time.Minute), r.Context/1000)
		if idleCompactDryRun {
			continue
		}
		if err := paseo.Send(a.ID, "/compact"); err != nil {
			fmt.Fprintf(os.Stderr, "  %s: %v\n", lastPathSegment(dir), err)
			continue
		}
		if err := appendCompaction(compactionEntry{At: now, Agent: a.ID, Project: lastPathSegment(dir), Transcript: path, LastRequest: r.At, Context: r.Context}); err != nil {
			fmt.Fprintf(os.Stderr, "  log: %v\n", err)
		}
	}
	return nil
}

// outcome is what a transcript shows after a logged compaction: the summary
// it left, and the first request that followed.
type outcome struct {
	Compacted  bool
	Pre, Post  int
	NextAt     time.Time
	NextCtx    int
	NextRead   int
	NextCreate int
}

func compactionOutcome(e compactionEntry) outcome {
	var o outcome
	f, err := os.Open(e.Transcript)
	if err != nil {
		return o
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		line := sc.Bytes()
		var rec struct {
			Type      string `json:"type"`
			Subtype   string `json:"subtype"`
			Timestamp string `json:"timestamp"`
			Meta      struct {
				Pre  int `json:"preTokens"`
				Post int `json:"postTokens"`
			} `json:"compactMetadata"`
			Message struct {
				Model string `json:"model"`
				Usage *struct {
					Input       int `json:"input_tokens"`
					CacheRead   int `json:"cache_read_input_tokens"`
					CacheCreate int `json:"cache_creation_input_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		at, _ := time.Parse(time.RFC3339, rec.Timestamp)
		if at.Before(e.At.Add(-time.Minute)) {
			continue
		}
		if !o.Compacted && rec.Type == "system" && rec.Subtype == "compact_boundary" {
			o.Compacted, o.Pre, o.Post = true, rec.Meta.Pre, rec.Meta.Post
			continue
		}
		if o.Compacted && rec.Type == "assistant" && rec.Message.Usage != nil && rec.Message.Model != "<synthetic>" {
			u := rec.Message.Usage
			o.NextAt, o.NextRead, o.NextCreate = at, u.CacheRead, u.CacheCreate
			o.NextCtx = u.Input + u.CacheRead + u.CacheCreate
			return o
		}
	}
	return o
}

func reportCompactions() error {
	entries := loadCompactions()
	if len(entries) == 0 {
		fmt.Println("no idle compactions yet")
		return nil
	}
	var before, after int
	for _, e := range entries {
		o := compactionOutcome(e)
		line := fmt.Sprintf("%s  %-18s %4dk", e.At.Local().Format("Jan 02 15:04"), e.Project, e.Context/1000)
		switch {
		case !o.Compacted:
			line += "  no compaction recorded"
		case o.NextAt.IsZero():
			line += fmt.Sprintf("  -> %dk summary, not resumed yet", o.Post/1000)
		default:
			line += fmt.Sprintf("  -> resumed %s later at %dk (%dk from cache, %dk written)",
				o.NextAt.Sub(e.At).Round(time.Minute), o.NextCtx/1000, o.NextRead/1000, o.NextCreate/1000)
			before += e.Context
			after += o.NextCtx
		}
		fmt.Println(line)
	}
	if before > 0 {
		fmt.Printf("resumed sessions came back at %s of the context they were compacted from\n", percent(after, before))
	}
	return nil
}

func percent(a, b int) string {
	return strings.TrimSpace(fmt.Sprintf("%3.0f%%", float64(a)/float64(b)*100))
}
