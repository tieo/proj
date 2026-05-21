// Package unreset watches tmux panes for Claude Code's usage-limit banner
// and resumes them by dismissing any "stop and wait" selector then typing
// "continue". Acts on banner presence alone — does not gate on the reset
// time printed in the banner, because that time is informational (the
// underlying limit may have already cleared).
package unreset

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tieo/proj/internal/tmux"
)

type Banner struct {
	// Reset is the parsed clock time from the banner. Best-effort: the
	// daemon uses it only for status display, not for gating actions.
	Reset time.Time
	Text  string
}

type Tracked struct {
	Session     string    `json:"session"`
	Pane        string    `json:"pane"`
	Banner      string    `json:"banner"`
	Reset       time.Time `json:"reset"`
	FirstSeen   time.Time `json:"first_seen"`
	LastSeen    time.Time `json:"last_seen"`
	LastActed   time.Time `json:"last_acted"`
	NextAttempt time.Time `json:"next_attempt"`
	Attempts    int       `json:"attempts"`
}

type State map[string]Tracked

type Config struct {
	Poll       time.Duration
	MaxWait    time.Duration // upper bound on how long we'll wait between retries
	Jitter     time.Duration // added to the scheduled retry time
	DismissGap time.Duration // pause between Escape and "continue"
	ResumeText string
	Capture    int
	StatePath  string
}

func DefaultConfig() Config {
	return Config{
		Poll:       60 * time.Second,
		MaxWait:    5 * time.Hour, // ≥ any single Claude usage window
		Jitter:     30 * time.Second,
		DismissGap: 300 * time.Millisecond,
		ResumeText: "continue",
		Capture:    300,
		StatePath:  defaultStatePath(),
	}
}

func defaultStatePath() string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "proj", "unreset.json")
}

// recentWindow is a loose first filter on where in the pane capture the
// match must appear. The toolPrefix check below is the actual structural
// guard against false positives.
const recentWindow = 2000

// toolPrefix is the continuation marker Claude Code prefixes tool-output
// lines with. Requiring it on the matched line is what distinguishes a
// real banner (rendered by Claude's TUI) from the same phrase appearing
// in prose, in a code block, or in user-typed input.
const toolPrefix = '⎿'

var bannerPatterns = []*regexp.Regexp{
	// The only banner format verified from real Claude Code (CLI TUI) captures:
	//   ⎿  You're out of extra usage · resets 3am (Europe/Berlin)
	// Timezone may wrap to the next line.
	// New patterns are added here only after a real capture proves them out.
	regexp.MustCompile(`(?i)out of extra usage(?:\s*[·.\-])?\s+resets\s+(\d{1,2}(?::\d{2})?\s*(?:am|pm))(?:\s*\(([A-Za-z_/+\-0-9]+)\))?`),
}

var timeRE = regexp.MustCompile(`^(\d{1,2})(?::(\d{2}))?(am|pm)$`)

// Selector shown by Claude's /rate-limit-options or mid-call when a tool
// hits the limit. Dismissed by sending Escape.
var selectorRE = regexp.MustCompile(`(?i)(?:What do you want to do\?|Stop and wait for limit to reset)`)

// Detect returns the parsed banner if `content` shows a blocked Claude
// session. Returns nil if no banner, the session is still proceeding via
// extra-usage credits, the only match is buried in old scrollback, or
// the matched line lacks Claude's tool-output continuation marker.
func Detect(content string, now time.Time) *Banner {
	threshold := len(content) - recentWindow
	if threshold < 0 {
		threshold = 0
	}
	for _, re := range bannerPatterns {
		matches := re.FindAllStringSubmatchIndex(content, -1)
		// Iterate matches from most recent backwards; first valid one wins.
		for i := len(matches) - 1; i >= 0; i-- {
			m := matches[i]
			if m[0] < threshold {
				break // older matches are even further back
			}
			if !hasToolPrefix(content, m[0]) {
				continue
			}
			end := m[1]
			tailEnd := end + 120
			if tailEnd > len(content) {
				tailEnd = len(content)
			}
			tail := strings.ToLower(content[end:tailEnd])
			if strings.Contains(tail, "continuing with") || strings.Contains(tail, "now using extra") {
				continue
			}
			timeStr := content[m[2]:m[3]]
			tzStr := ""
			if len(m) >= 6 && m[4] >= 0 {
				tzStr = content[m[4]:m[5]]
			}
			reset, _ := parseReset(timeStr, tzStr, now) // best-effort; zero ok
			text := strings.Join(strings.Fields(content[m[0]:m[1]]), " ")
			if len(text) > 160 {
				text = text[:160]
			}
			return &Banner{Reset: reset, Text: text}
		}
	}
	return nil
}

// hasToolPrefix reports whether the line containing matchStart begins
// (after optional leading whitespace) with Claude's tool-output marker.
func hasToolPrefix(content string, matchStart int) bool {
	lineStart := strings.LastIndexByte(content[:matchStart], '\n') + 1
	for _, r := range content[lineStart:matchStart] {
		if r == toolPrefix {
			return true
		}
	}
	return false
}

func HasSelector(content string) bool {
	return selectorRE.MatchString(content)
}

// parseReset interprets a clock-time string like "3am" as the nearest
// occurrence to `now` (within ±12h). Returns zero time + error on
// unparseable input. Used only for status display.
func parseReset(timeStr, tzStr string, now time.Time) (time.Time, error) {
	s := strings.ToLower(strings.ReplaceAll(timeStr, " ", ""))
	m := timeRE.FindStringSubmatch(s)
	if m == nil {
		return time.Time{}, fmt.Errorf("unparseable time %q", timeStr)
	}
	hour, _ := strconv.Atoi(m[1])
	hour = hour % 12
	if m[3] == "pm" {
		hour += 12
	}
	minute := 0
	if m[2] != "" {
		minute, _ = strconv.Atoi(m[2])
	}
	var loc *time.Location
	if tzStr != "" {
		if l, err := time.LoadLocation(tzStr); err == nil {
			loc = l
		}
	}
	if loc == nil {
		loc = now.Location()
	}
	n := now.In(loc)
	target := time.Date(n.Year(), n.Month(), n.Day(), hour, minute, 0, 0, loc)
	diff := target.Sub(n)
	switch {
	case diff > 12*time.Hour:
		target = target.AddDate(0, 0, -1)
	case diff < -12*time.Hour:
		target = target.AddDate(0, 0, 1)
	}
	return target, nil
}

func LoadState(path string) State {
	data, err := os.ReadFile(path)
	if err != nil {
		return State{}
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return State{}
	}
	if s == nil {
		s = State{}
	}
	return s
}

func SaveState(path string, state State) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Action describes what the daemon decided to do for one pane this tick.
// Pure function — Decide has no side effects so it's directly testable.
type Action int

const (
	ActNone    Action = iota // no banner present
	ActResume                // banner visible, no selector → send "continue"
	ActDismiss               // banner + selector visible → send Escape then "continue"
	ActWait                  // banner visible but scheduled retry is in the future
)

func Decide(content string, prev Tracked, now time.Time) Action {
	if Detect(content, now) == nil {
		return ActNone
	}
	if !prev.NextAttempt.IsZero() && now.Before(prev.NextAttempt) {
		return ActWait
	}
	if HasSelector(content) {
		return ActDismiss
	}
	return ActResume
}

// nextAttemptAfter computes when the next retry should fire if this one
// fails. Uses the banner's parsed clock time as the lower bound (advancing
// to the next future occurrence) and the configured MaxWait as the upper
// bound. If the banner had no parseable time, falls back to MaxWait.
func nextAttemptAfter(b *Banner, now time.Time, cfg Config) time.Time {
	cap := now.Add(cfg.MaxWait)
	if b.Reset.IsZero() {
		return cap
	}
	next := b.Reset
	for !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	next = next.Add(cfg.Jitter)
	if next.After(cap) {
		next = cap
	}
	return next
}

func Tick(cfg Config, state State, now time.Time) {
	panes := tmux.ListPanes()
	live := make(map[string]string, len(panes))
	for _, p := range panes {
		live[p.ID] = p.Session
	}
	for id, t := range state {
		if _, ok := live[id]; !ok {
			slog.Info("drop tracked", "session", t.Session, "reason", "pane gone")
			delete(state, id)
		}
	}

	for _, p := range panes {
		content := tmux.CapturePane(p.ID, cfg.Capture)
		b := Detect(content, now)
		if b == nil {
			if t, ok := state[p.ID]; ok {
				reason := "banner cleared"
				if t.Attempts > 0 {
					reason = "resume succeeded"
				}
				slog.Info("drop tracked", "session", t.Session, "reason", reason, "attempts", t.Attempts)
				delete(state, p.ID)
			}
			continue
		}
		prev := state[p.ID]
		switch Decide(content, prev, now) {
		case ActWait:
			prev.LastSeen = now
			prev.Banner = b.Text
			prev.Reset = b.Reset
			state[p.ID] = prev
		case ActDismiss:
			slog.Info("dismiss + resume",
				"session", p.Session, "pane", p.ID,
				"attempt", prev.Attempts+1, "banner", b.Text)
			if err := tmux.SendKey(p.ID, "Escape"); err != nil {
				slog.Error("send Escape failed", "session", p.Session, "err", err)
				continue
			}
			time.Sleep(cfg.DismissGap)
			if err := tmux.SendKeys(p.ID, cfg.ResumeText); err != nil {
				slog.Error("send-keys failed", "session", p.Session, "err", err)
				continue
			}
			state[p.ID] = recordAction(prev, p, b, now, cfg)
		case ActResume:
			slog.Info("resume",
				"session", p.Session, "pane", p.ID,
				"attempt", prev.Attempts+1, "banner", b.Text)
			if err := tmux.SendKeys(p.ID, cfg.ResumeText); err != nil {
				slog.Error("send-keys failed", "session", p.Session, "err", err)
				continue
			}
			state[p.ID] = recordAction(prev, p, b, now, cfg)
		}
	}
}

func recordAction(prev Tracked, p tmux.Pane, b *Banner, now time.Time, cfg Config) Tracked {
	first := prev.FirstSeen
	if first.IsZero() {
		first = now
	}
	next := nextAttemptAfter(b, now, cfg)
	return Tracked{
		Session:     p.Session,
		Pane:        p.ID,
		Banner:      b.Text,
		Reset:       b.Reset,
		FirstSeen:   first,
		LastSeen:    now,
		LastActed:   now,
		NextAttempt: next,
		Attempts:    prev.Attempts + 1,
	}
}

func Run(ctx context.Context, cfg Config) error {
	slog.Info("started",
		"poll", cfg.Poll, "max_wait", cfg.MaxWait, "jitter", cfg.Jitter,
		"resume_text", cfg.ResumeText, "state", cfg.StatePath)
	state := LoadState(cfg.StatePath)
	if len(state) > 0 {
		slog.Info("loaded state", "tracked", len(state))
	}
	ticker := time.NewTicker(cfg.Poll)
	defer ticker.Stop()
	for {
		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("tick panicked", "panic", r)
				}
			}()
			Tick(cfg, state, time.Now())
			if err := SaveState(cfg.StatePath, state); err != nil {
				slog.Error("save state failed", "err", err)
			}
		}()
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
