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
	"io"
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
	Poll        time.Duration
	MaxWait     time.Duration // upper bound on how long we'll wait between retries
	Jitter      time.Duration // added to the scheduled retry time
	DismissGap  time.Duration // pause between Escape and "continue"
	ResumeText  string
	CompactText string // slash command to compact a stuck session
	ClearText   string // slash command to clear when compact itself fails
	Capture     int
	StatePath   string
}

func DefaultConfig() Config {
	return Config{
		Poll:        60 * time.Second,
		MaxWait:     5 * time.Hour, // ≥ any single Claude usage window
		Jitter:      time.Second,   // reset times are accurate to the minute; 1s grace is enough
		DismissGap:  300 * time.Millisecond,
		ResumeText:  "continue",
		CompactText: "/compact",
		ClearText:   "/clear",
		Capture:     10,
		StatePath:   defaultStatePath(),
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

// sp matches any mix of regular spaces and the non-breaking spaces (U+00A0)
// that Claude Code's TUI uses for padding between its ⎿/❯ markers and text.
// Go's \s covers only ASCII whitespace, so NBSP must be included explicitly.
const sp = `[\s\x{00a0}]`

// apiErrorRE matches Claude Code's API error output line.
// The ⎿ prefix is Claude Code's tool-output continuation marker; it only
// appears in rendered TUI output, not in Claude's text or user-typed input.
// This distinguishes a real API failure from prose or code mentioning one.
var apiErrorRE = regexp.MustCompile(`⎿` + sp + `+API Error:` + sp + `*(\d{3})` + sp + `+(\{[^\n]+\})`)

// compactFailedRE detects a failed /compact attempt: Claude Code renders the
// compaction error with this prefix. If visible, retrying /compact is futile
// (the broken content is in history and will keep failing until /clear or restart).
var compactFailedRE = regexp.MustCompile(`⎿` + sp + `+Error: Error during compaction:`)

// inputPromptRE matches the Claude Code input prompt: ❯ at the start of a
// line (after a newline or at the beginning of the string). In Claude Code's
// TUI, picker option lines (e.g. "❯ 1. Stop and wait") always have leading
// spaces, so they cannot start at column 0 and ^❯ never matches them. Text
// in the input buffer ("❯ commit this") is intentionally matched — a session
// with unsent text AND a recent API error is still idle and should be recovered.
var inputPromptRE = regexp.MustCompile(`(?m)^❯`)

// APIError holds the data extracted from a Claude Code API error line.
type APIError struct {
	StatusCode int
	Message    string // "Could not process image"
	RequestID  string // "req_011Cb..."
	Text       string // raw line, truncated to 200 bytes
}

// ErrorTracked holds the in-memory tracking state for a pane stuck after an
// API error. Not persisted — re-detection takes at most two poll cycles.
type ErrorTracked struct {
	Session   string
	Pane      string
	Text      string
	FirstSeen time.Time
	LastSeen  time.Time
	Acted     bool // true after /compact has been sent
}

// ErrorState maps pane IDs to their current error tracking entry.
type ErrorState map[string]ErrorTracked

// DetectAPIError returns a non-nil *APIError if `content` shows a Claude Code
// session that is stuck at the input prompt after a permanent API error.
//
// Two structural guards make this robust without needing a recency window:
//  1. The ⎿ prefix on the error line distinguishes TUI-rendered tool output
//     from Claude's text, user input, or code blocks that mention API errors.
//  2. The input-prompt regex (❯ alone on a line) confirms Claude returned
//     control to the user; it rejects panes with active tool calls or pickers.
//
// No byte-offset threshold is applied because the wide box-drawing characters
// used by Claude Code's TUI inflate content size unpredictably, and the two
// structural guards alone are strong enough to prevent false positives.
func DetectAPIError(content string) *APIError {
	matches := apiErrorRE.FindAllStringSubmatchIndex(content, -1)
	if len(matches) == 0 {
		return nil
	}
	m := matches[len(matches)-1] // most recent occurrence
	// Require the input prompt to be visible — the session must be idle, not
	// actively running tools (which would suppress the lone ❯ line).
	if !inputPromptRE.MatchString(content) {
		return nil
	}
	statusCode, _ := strconv.Atoi(content[m[2]:m[3]])
	jsonStr := content[m[4]:m[5]]
	var payload struct {
		Error     struct{ Message string `json:"message"` } `json:"error"`
		RequestID string                                    `json:"request_id"`
	}
	_ = json.Unmarshal([]byte(jsonStr), &payload)
	text := strings.TrimSpace(content[m[0]:m[1]])
	if len(text) > 200 {
		text = text[:200]
	}
	return &APIError{
		StatusCode: statusCode,
		Message:    payload.Error.Message,
		RequestID:  payload.RequestID,
		Text:       text,
	}
}

// DecideCompact reports whether the daemon should send /compact now.
// It requires the error to have been continuously visible for at least minAge
// (one full poll cycle) to filter transient errors Claude Code auto-recovers from.
func DecideCompact(apiErr *APIError, prev ErrorTracked, now time.Time, minAge time.Duration) bool {
	if apiErr == nil || prev.FirstSeen.IsZero() || prev.Acted {
		return false
	}
	return now.Sub(prev.FirstSeen) >= minAge
}

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
	Banner   *Banner   // non-nil if a usage-limit banner is visible
	Selector bool      // a dismissable interactive picker is visible
	APIError *APIError // non-nil if stuck after a permanent API error
}

// Label returns a short human-readable status word for the pane.
func (s PaneState) Label() string {
	switch {
	case s.APIError != nil:
		return "error"
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
			APIError: DetectAPIError(content),
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

// claudeProjectDir derives the Claude Code project directory for a given
// working directory, following Claude Code's path-encoding convention.
func claudeProjectDir(workDir string) string {
	home, _ := os.UserHomeDir()
	encoded := strings.ReplaceAll(workDir, "/", "-")
	return filepath.Join(home, ".claude", "projects", encoded)
}

// claudeMemoryPath derives the Claude Code project memory directory for a
// given working directory, following Claude Code's path-encoding convention:
// replace every "/" with "-" (the leading "/" becomes a leading "-").
func claudeMemoryPath(workDir string) string {
	return filepath.Join(claudeProjectDir(workDir), "memory")
}

// recentSessionFile returns the path of the most recently modified .jsonl
// session file in the Claude Code project directory for workDir. Returns ""
// if none is found. Call this before sending /clear — /clear causes Claude
// Code to start a new session file, so the pre-clear file is the most recent
// one at the time of the call.
func recentSessionFile(workDir string) string {
	dir := claudeProjectDir(workDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var best string
	var bestTime time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(bestTime) {
			bestTime = info.ModTime()
			best = filepath.Join(dir, e.Name())
		}
	}
	return best
}

// extractSessionContext reads the last 30 JSONL records from sessionFile and
// returns a compact human-readable summary of the meaningful entries:
// Claude's text responses, plain user messages, and slash command results.
// Queue operations, attachments, file snapshots, and meta entries are dropped.
func extractSessionContext(sessionFile string) string {
	if sessionFile == "" {
		return ""
	}
	f, err := os.Open(sessionFile)
	if err != nil {
		return ""
	}
	defer f.Close()

	// Read the tail — 100 KB is enough to cover 30 records with room to spare.
	const readBytes = 100 * 1024
	size, _ := f.Seek(0, io.SeekEnd)
	start := size - readBytes
	if start < 0 {
		start = 0
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return ""
	}
	buf := make([]byte, readBytes)
	n, _ := f.Read(buf)
	lines := strings.Split(string(buf[:n]), "\n")
	// First line may be partial (seeked mid-file); drop it.
	if start > 0 && len(lines) > 1 {
		lines = lines[1:]
	}
	// Keep only the last 30.
	if len(lines) > 30 {
		lines = lines[len(lines)-30:]
	}

	var out strings.Builder
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var obj map[string]interface{}
		if json.Unmarshal([]byte(line), &obj) != nil {
			continue
		}
		t, _ := obj["type"].(string)
		if isMeta, _ := obj["isMeta"].(bool); isMeta {
			continue
		}
		switch t {
		case "assistant":
			msg, _ := obj["message"].(map[string]interface{})
			if msg == nil {
				continue
			}
			for _, c := range toSlice(msg["content"]) {
				cm, _ := c.(map[string]interface{})
				if cm == nil || cm["type"] != "text" {
					continue
				}
				text := truncate(cm["text"], 600)
				if text != "" {
					fmt.Fprintf(&out, "[Claude] %s\n", text)
				}
			}
		case "user":
			msg, _ := obj["message"].(map[string]interface{})
			if msg == nil {
				continue
			}
			switch c := msg["content"].(type) {
			case string:
				if text := truncate(c, 800); text != "" && !strings.HasPrefix(text, "<") {
					fmt.Fprintf(&out, "[User] %s\n", text)
				}
			case []interface{}:
				for _, item := range c {
					cm, _ := item.(map[string]interface{})
					if cm == nil || cm["type"] != "text" {
						continue
					}
					text := truncate(cm["text"], 800)
					if text != "" && !strings.HasPrefix(text, "<") {
						fmt.Fprintf(&out, "[User] %s\n", text)
					}
				}
			}
		case "system":
			if sub, _ := obj["subtype"].(string); sub == "local_command" {
				text := truncate(obj["content"], 300)
				if text != "" {
					fmt.Fprintf(&out, "[Cmd] %s\n", text)
				}
			}
		}
	}
	return strings.TrimSpace(out.String())
}

func toSlice(v interface{}) []interface{} {
	s, _ := v.([]interface{})
	return s
}

func truncate(v interface{}, max int) string {
	s, _ := v.(string)
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

// compactResumeMessage is sent after a successful /compact so Claude continues
// without waiting for user input.
const compactResumeMessage = "The conversation was just compacted automatically to recover from a stuck state. Continue the current task immediately without asking the user anything — just resume."

// isImageError reports whether an API error message is about an image being
// corrupt, unreadable, or in an unsupported format.
func isImageError(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "image") || strings.Contains(msg, "could not process")
}

// clearRecoveryMessage returns the message sent to Claude after /clear so it
// can understand why the conversation was wiped and resume from session history.
// context is the pre-extracted recent session content (may be empty).
func clearRecoveryMessage(apiErr *APIError, context string) string {
	contextSection := ""
	if context != "" {
		contextSection = "\n\nRecent session context:\n" + context + "\n\n"
	}
	imageAdvice := ""
	if isImageError(apiErr.Message) {
		imageAdvice = "Before resuming: save a memory about how to avoid this error — " +
			"always verify screenshot/image files are valid (check size > 10 KB " +
			"and run `file <path>` to confirm format) before passing them to the Read tool. " +
			"Never read a file that was just written by a tool without checking it first. "
	}
	return fmt.Sprintf(
		"The conversation was just cleared automatically. "+
			"The session was stuck on API Error %d (%s) — this also caused /compact to fail. "+
			"%s"+
			"%s"+
			"Then immediately continue working without asking the user anything. "+
			"Do not summarize, do not ask what to work on — just resume.",
		apiErr.StatusCode, apiErr.Message, imageAdvice, contextSection)
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

func Tick(cfg Config, state State, errorState ErrorState, now time.Time) {
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
	for id, t := range errorState {
		if _, ok := live[id]; !ok {
			slog.Info("drop error tracked", "session", t.Session, "reason", "pane gone")
			delete(errorState, id)
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

		// 2) Handle a session stuck after a permanent API error.
		//    Two-tick confirmation (detect → wait one poll → act) filters out
		//    transient errors Claude Code auto-recovers from.
		apiErr := DetectAPIError(content)
		if apiErr != nil {
			compactFailed := compactFailedRE.MatchString(content)
			prev := errorState[p.ID]
			switch {
			case compactFailed:
				// /compact failed (either this run or a prior daemon run) —
				// the broken content is embedded in history. Fall back to
				// /clear, then send Claude an explanation so it can resume from memory.
				workDir := tmux.PaneCurrentPath(p.ID)
				// Extract context BEFORE /clear — /clear causes Claude Code to
				// start a new session file, so the pre-clear file has the history.
				sessFile := recentSessionFile(workDir)
				sessContext := extractSessionContext(sessFile)
				slog.Info("compact failed, clearing conversation",
					"session", p.Session, "pane", p.ID,
					"error", apiErr.Message, "session_file", sessFile)
				clearOK := false
				if err := tmux.SendKey(p.ID, "Escape"); err != nil {
					slog.Error("send Escape failed", "session", p.Session, "err", err)
				} else {
					time.Sleep(cfg.DismissGap)
					if err := tmux.SendKeys(p.ID, cfg.ClearText); err != nil {
						slog.Error("send /clear failed", "session", p.Session, "err", err)
					} else {
						clearOK = true
						slog.Info("cleared", "session", p.Session, "pane", p.ID)
						// Wait for Claude Code to finish processing /clear and
						// redraw to the idle prompt. Then send the recovery message
						// in literal mode (no key-name expansion) followed by a
						// separate Enter — this guarantees Enter only arrives after
						// all the message text has landed in the input buffer.
						time.Sleep(2 * time.Second)
						msg := clearRecoveryMessage(apiErr, sessContext)
						if err := tmux.SendLiteral(p.ID, msg); err != nil {
							slog.Error("send recovery message failed", "session", p.Session, "err", err)
						} else {
							time.Sleep(100 * time.Millisecond)
							if err := tmux.SendKey(p.ID, "Enter"); err != nil {
								slog.Error("send Enter failed", "session", p.Session, "err", err)
							}
						}
					}
				}
				if prev.FirstSeen.IsZero() {
					prev.FirstSeen = now
				}
				prev.Session = p.Session
				prev.Pane = p.ID
				prev.Text = apiErr.Text
				prev.LastSeen = now
				prev.Acted = clearOK
				errorState[p.ID] = prev
			case prev.FirstSeen.IsZero():
				// First sighting — record, wait for next tick to confirm.
				errorState[p.ID] = ErrorTracked{
					Session: p.Session, Pane: p.ID, Text: apiErr.Text,
					FirstSeen: now, LastSeen: now,
				}
				slog.Info("api error detected",
					"session", p.Session, "pane", p.ID,
					"status", apiErr.StatusCode, "msg", apiErr.Message)
			case DecideCompact(apiErr, prev, now, cfg.Poll):
				// Error persisted for a full poll cycle → send /compact.
				slog.Info("compact",
					"session", p.Session, "pane", p.ID,
					"error", apiErr.Message,
					"stuck_for", now.Sub(prev.FirstSeen).Round(time.Second))
				if err := tmux.SendKey(p.ID, "Escape"); err != nil {
					slog.Error("send Escape failed", "session", p.Session, "err", err)
				} else {
					time.Sleep(cfg.DismissGap)
					if err := tmux.SendKeys(p.ID, cfg.CompactText); err != nil {
						slog.Error("send compact failed", "session", p.Session, "err", err)
					} else {
						prev.Acted = true
					}
				}
				prev.LastSeen = now
				errorState[p.ID] = prev
			default:
				if prev.Acted {
					// /compact was sent but the error is still visible — the
					// compact itself may have failed (e.g. broken image in
					// history). Warn every poll so the user knows to intervene.
					slog.Warn("compact sent but error persists — manual intervention needed",
						"session", p.Session, "pane", p.ID,
						"stuck_for", now.Sub(prev.FirstSeen).Round(time.Second))
				}
				prev.LastSeen = now
				errorState[p.ID] = prev
			}
			continue // skip banner check while error is present
		}
		if prev, ok := errorState[p.ID]; ok {
			if prev.Acted {
				slog.Info("compact succeeded", "session", p.Session, "pane", p.ID)
				if err := tmux.SendLiteral(p.ID, compactResumeMessage); err != nil {
					slog.Error("send compact resume failed", "session", p.Session, "err", err)
				} else {
					time.Sleep(100 * time.Millisecond)
					if err := tmux.SendKey(p.ID, "Enter"); err != nil {
						slog.Error("send Enter failed", "session", p.Session, "err", err)
					}
				}
			}
			delete(errorState, p.ID)
		}

		// 3) Handle usage-limit banner (now visible if it was hidden behind the picker).
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
	errorState := make(ErrorState)
	tick := 0
	for {
		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("tick panicked", "panic", r)
				}
			}()
			Tick(cfg, state, errorState, time.Now())
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
