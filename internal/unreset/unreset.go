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
	// Reset is the parsed reset time from the banner.
	Reset time.Time
	// ResetExplicit is true when the banner included an explicit date
	// ("resets May 24, 2am") — meaning Reset is the authoritative time.
	// False when only a clock time was given ("resets 2am") and Reset was
	// inferred via nearest-occurrence; in that case the daemon caps
	// scheduling at MaxWait in case the inference is wrong.
	ResetExplicit bool
	Text          string
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
		Jitter:     time.Second,   // reset times are accurate to the minute; 1s grace is enough
		DismissGap: 300 * time.Millisecond,
		ResumeText: "continue",
		Capture:    0,
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
	//   ⎿  You're out of extra usage · resets May 24, 2am (Europe/Berlin)
	// Timezone may wrap to the next line. The date prefix appears only when
	// the reset is more than ~24h out.
	// New patterns are added here only after a real capture proves them out.
	regexp.MustCompile(`(?i)out of extra usage(?:\s*[·.\-])?\s+resets\s+(?:([A-Za-z]+\s+\d{1,2}),\s+)?(\d{1,2}(?::\d{2})?\s*(?:am|pm))(?:\s*\(([A-Za-z_/+\-0-9]+)\))?`),
}

var timeRE = regexp.MustCompile(`^(\d{1,2})(?::(\d{2}))?(am|pm)$`)

// Known Claude TUI picker phrases. Recognised pickers we'll dismiss with
// Escape:
//   - "What do you want to do?" / "Stop and wait …" — /rate-limit-options
//   - "Resume from summary" / "Resume the full session" — old-session resume
var selectorRE = regexp.MustCompile(`(?i)(?:What do you want to do\?|Stop and wait for limit to reset|Resume from summary|Resume the full session)`)

// A "❯ <digit>." line — the highlighted option marker. Distinctive: the
// regular input prompt is "❯ " with no number after, so this only matches
// inside an actual picker overlay (or its verbatim quote).
var pickerOptionRE = regexp.MustCompile(`(?m)^\s*❯\s+\d+\.\s`)

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
			dateStr := ""
			if m[2] >= 0 {
				dateStr = content[m[2]:m[3]]
			}
			timeStr := content[m[4]:m[5]]
			tzStr := ""
			if len(m) >= 8 && m[6] >= 0 {
				tzStr = content[m[6]:m[7]]
			}
			reset, explicit, _ := parseReset(dateStr, timeStr, tzStr, now)
			text := strings.Join(strings.Fields(content[m[0]:m[1]]), " ")
			if len(text) > 160 {
				text = text[:160]
			}
			return &Banner{Reset: reset, ResetExplicit: explicit, Text: text}
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

// HasSelector reports whether a real Claude picker overlay is visible.
// Requires (1) a recognised picker phrase, (2) appearing in the recent
// portion of the pane capture (i.e. visible right now, not buried in
// scrollback), and (3) accompanied by a "❯ <digit>." option line. All
// three together filter out chat quotations of picker text.
func HasSelector(content string) bool {
	matches := selectorRE.FindAllStringIndex(content, -1)
	if len(matches) == 0 {
		return false
	}
	// Use the most recent occurrence.
	last := matches[len(matches)-1]
	if last[0] < len(content)-recentWindow {
		return false
	}
	// And there must be a ❯ <digit>. option line within ~500 chars.
	near := last[1] + 500
	if near > len(content) {
		near = len(content)
	}
	// Scan from start-of-line preceding the phrase too (option list may
	// appear just before the phrase header in some layouts).
	scanStart := last[0] - 200
	if scanStart < 0 {
		scanStart = 0
	}
	return pickerOptionRE.MatchString(content[scanStart:near])
}

// PaneState summarises what the daemon currently sees for one pane.
type PaneState struct {
	Pane     tmux.Pane
	Banner   *Banner // non-nil if a usage-limit banner is visible
	Selector bool    // a dismissable interactive picker is visible
}

// Label returns a short human-readable status word for the pane.
func (s PaneState) Label() string {
	switch {
	case s.Banner != nil && s.Selector:
		return "banner + selector"
	case s.Banner != nil:
		return "banner"
	case s.Selector:
		return "selector"
	default:
		return "idle"
	}
}

// ScanPanes captures every pane and classifies each. Used by status output.
func ScanPanes(captureLines int) []PaneState {
	panes := tmux.ListPanes()
	now := time.Now()
	out := make([]PaneState, 0, len(panes))
	for _, p := range panes {
		content := tmux.CapturePane(p.ID, captureLines)
		out = append(out, PaneState{
			Pane:     p,
			Banner:   Detect(content, now),
			Selector: HasSelector(content),
		})
	}
	return out
}

// parseReset interprets banner date/time strings. Returns:
//   - the resolved datetime,
//   - explicit=true when an authoritative date string was given (e.g.
//     "May 24") that pinned the day, false when only a clock-time was
//     given and the day had to be inferred,
//   - error on unparseable input.
func parseReset(dateStr, timeStr, tzStr string, now time.Time) (time.Time, bool, error) {
	s := strings.ToLower(strings.ReplaceAll(timeStr, " ", ""))
	m := timeRE.FindStringSubmatch(s)
	if m == nil {
		return time.Time{}, false, fmt.Errorf("unparseable time %q", timeStr)
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

	if dateStr != "" {
		// Banner gave an explicit date like "May 24". Combine with the
		// clock time; assume the current year, advancing to next year if
		// the resulting datetime is far enough in the past that the
		// banner must really be referring to next year (>30 days behind).
		if d, err := time.ParseInLocation("Jan 2", dateStr, loc); err == nil {
			target := time.Date(n.Year(), d.Month(), d.Day(), hour, minute, 0, 0, loc)
			if target.Before(n.AddDate(0, 0, -30)) {
				target = target.AddDate(1, 0, 0)
			}
			return target, true, nil
		}
		// Date couldn't be parsed — fall through to clock-only inference.
	}

	target := time.Date(n.Year(), n.Month(), n.Day(), hour, minute, 0, 0, loc)
	diff := target.Sub(n)
	switch {
	case diff > 12*time.Hour:
		target = target.AddDate(0, 0, -1)
	case diff < -12*time.Hour:
		target = target.AddDate(0, 0, 1)
	}
	return target, false, nil
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

// Action describes the per-banner decision in one tick. Selector
// dismissal is handled separately in Tick — it's a side-channel concern,
// independent of banner state.
type Action int

const (
	ActNone   Action = iota // no banner present
	ActResume               // banner visible, retry due → send "continue"
	ActWait                 // banner visible but scheduled retry is in the future
)

func Decide(content string, prev Tracked, now time.Time) Action {
	if Detect(content, now) == nil {
		return ActNone
	}
	if !prev.NextAttempt.IsZero() && now.Before(prev.NextAttempt) {
		return ActWait
	}
	return ActResume
}

// nextAttemptAfter computes when the next retry should fire if this one
// fails. If the banner gave an explicit future date, trust it as-is. If
// only a clock time was given (date inferred), advance to the next future
// occurrence and cap at MaxWait so a bad inference doesn't strand us.
func nextAttemptAfter(b *Banner, now time.Time, cfg Config) time.Time {
	cap := now.Add(cfg.MaxWait)
	if b.Reset.IsZero() {
		return cap
	}
	if b.ResetExplicit && b.Reset.After(now) {
		return b.Reset.Add(cfg.Jitter)
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

		// 1) Dismiss any known Claude interactive picker. Independent of
		//    banner state — a stuck prompt is itself something to resolve.
		//    Esc is idempotent; if already gone next tick, nothing happens.
		if HasSelector(content) {
			slog.Info("dismiss selector", "session", p.Session, "pane", p.ID)
			if err := tmux.SendKey(p.ID, "Escape"); err != nil {
				slog.Error("send Escape failed", "session", p.Session, "err", err)
			} else {
				time.Sleep(cfg.DismissGap)
				content = tmux.CapturePane(p.ID, cfg.Capture) // re-read post-dismiss
			}
		}

		// 2) Handle banner (now visible if it was hidden behind the picker).
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
		case ActResume:
			slog.Info("resume",
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
			t := recordAction(prev, p, b, now, cfg)
			state[p.ID] = t
			slog.Info("deferred", "session", p.Session,
				"next", t.NextAttempt.Format("Mon 15:04 MST"))
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
	heartbeatEvery := int(30 * time.Minute / cfg.Poll)
	if heartbeatEvery < 1 {
		heartbeatEvery = 1
	}
	tick := 0
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
		tick++
		if tick%heartbeatEvery == 0 {
			slog.Info("heartbeat", "tick", tick, "tracked", len(state))
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
