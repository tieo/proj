// Package daemon watches tmux panes for Claude Code's usage-limit banner
// and resumes them by dismissing any "stop and wait" selector then typing
// "continue". Acts on banner presence alone; does not gate on the reset
// time printed in the banner, because that time is informational (the
// underlying limit may have already cleared).
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tieo/proj/internal/shellout"
	"github.com/tieo/proj/internal/tmux"
)

type Banner struct {
	// Reset is the parsed reset time from a usage-limit banner.
	Reset time.Time
	// Backoff is set for transient errors that should be retried after a short
	// pause (e.g. "Server is temporarily limiting requests · Rate limited"),
	// not deferred until a usage reset. When non-zero it overrides Reset.
	Backoff time.Duration
	Text    string
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
	Poll             time.Duration
	MaxWait          time.Duration // fallback retry interval when the banner has no parseable time
	DismissGap       time.Duration // pause between Escape and "continue"
	ResumeText       string
	CompactText      string // slash command to compact a stuck session
	ClearText        string // slash command to clear when compact itself fails
	Capture          int
	StatePath        string
	KeepAlive        bool // recreate vanished sessions that weren't cleanly closed
	ClaudeCommand    string
	ClaudeResumeFlag string
}

func DefaultConfig() Config {
	return Config{
		Poll:        60 * time.Second,
		MaxWait:     5 * time.Hour, // fallback retry interval when the banner has no parseable time at all
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
	return filepath.Join(base, "proj", "daemon.json")
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

// transientShortBackoff is the retry delay used when Claude Code reports the
// server-side "temporarily limiting" rate limit. Distinct from a usage-limit
// banner: the API is reachable but throttled, so a few-minute wait suffices.
// Combined with randomized jitter in nextAttemptAfter, this also spreads the
// thundering herd of every deferred client retrying at the same reset minute.
const transientShortBackoff = 60 * time.Second

// transientPattern matches Claude Code's gateway rate-limit error:
//
//	⎿  API Error: Server is temporarily limiting requests (not your usage limit) · Rate limited
//
// Distinct from the apiErrorRE (which expects a numeric status and JSON), this
// is a free-text reply that arrives when many clients hammer the API at the
// same instant (typically right at a usage-reset minute).
var transientPattern = regexp.MustCompile(`⎿` + sp + `+API Error:` + sp + `+Server is temporarily limiting requests`)

var bannerPatterns = []*regexp.Regexp{
	// Verified banner formats from real Claude Code (CLI TUI):
	//   ⎿  You're out of extra usage · resets 3am (Europe/Berlin)
	//   ⎿  You're out of extra usage · resets May 24, 2am (Europe/Berlin)
	//   ⎿  You've hit your session limit · resets 7:10pm (Europe/Berlin)
	// The "· resets <time> (tz)" tail is shared; only the lead phrase differs.
	// Timezone may wrap to the next line; the date prefix appears only when the
	// reset is more than ~24h out. New phrases are added only after a real capture.
	regexp.MustCompile(`(?i)(?:out of extra usage|session limit)(?:\s*[·.\-])?\s+resets\s+(?:([A-Za-z]+\s+\d{1,2}),\s+)?(\d{1,2}(?::\d{2})?\s*(?:am|pm))(?:\s*\(([A-Za-z_/+\-0-9]+)\))?`),
}

var timeRE = regexp.MustCompile(`^(\d{1,2})(?::(\d{2}))?(am|pm)$`)

// Known Claude TUI picker phrases the daemon will dismiss with Escape, limited
// to the rate-limit picker ("What do you want to do?" / "Stop and wait …"),
// which is safe to clear before sending "continue".
//
// The old-session "Resume from summary / Resume the full session" dialog is
// deliberately NOT here: Escape there cancels the resume, and choosing between
// summary and full is a real decision for the user, not one the daemon should
// answer. Acting on it interrupted the user mid-input and cancelled resumes.
var selectorRE = regexp.MustCompile(`(?i)(?:What do you want to do\?|Stop and wait for limit to reset)`)

// A "❯ <digit>." line; the highlighted option marker. Distinctive: the
// regular input prompt is "❯ " with no number after, so this only matches
// inside an actual picker overlay (or its verbatim quote).
var pickerOptionRE = regexp.MustCompile(`(?m)^\s*❯\s+\d+\.\s`)

// inputBoxRE matches the affordance line Claude renders directly beneath its
// live input box (the permission-mode cycler / shortcuts hint). A real picker
// replaces the input box, so none of these follow it. When one appears below a
// picker phrase, the phrase is therefore text sitting inside or above the
// user's input box, pasted TUI output or scrollback, not a live overlay, and
// must not be dismissed.
var inputBoxRE = regexp.MustCompile(`(?i)shift\+tab|\? for shortcuts|bypass permissions|accept edits|plan mode on`)

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
// in the input buffer ("❯ commit this") is intentionally matched; a session
// with unsent text AND a recent API error is still idle and should be recovered.
var inputPromptRE = regexp.MustCompile(`(?m)^❯`)

// modelRE matches the Claude model ID as rendered in the Claude Code TUI status
// bar (e.g. "claude-sonnet-4-6", "claude-opus-4-7", "claude-haiku-4-5-20251001").
var modelRE = regexp.MustCompile(`claude-(?:opus|sonnet|haiku)-[\d][\d.-]*`)

// APIError holds the data extracted from a Claude Code API error line.
type APIError struct {
	StatusCode int
	Message    string // "Could not process image"
	RequestID  string // "req_011Cb..."
	Text       string // raw line, truncated to 200 bytes
}

// ErrorTracked holds the tracking state for a pane stuck after an API error.
// Persisted to disk so daemon restarts don't lose the "compact was sent" flag.
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
// Structural guards make this robust without needing a recency window:
//  1. The ⎿ prefix on the error line distinguishes TUI-rendered tool output
//     from Claude's text, user input, or code blocks that mention API errors.
//  2. The input-prompt regex (❯ alone on a line) confirms Claude returned
//     control to the user; it rejects panes with active tool calls or pickers.
//     The last such prompt also marks the boundary between rendered output and
//     the user's input buffer, so a match below it (an error pasted/quoted into
//     the input box) is ignored.
//  3. The error's JSON payload must parse and carry a non-empty message; source
//     or fixtures on screen render as backslash-escaped literals that fail this,
//     so the daemon does not act on its own detection code being displayed.
//
// No byte-offset threshold is applied because the wide box-drawing characters
// used by Claude Code's TUI inflate content size unpredictably, and these
// structural guards alone are strong enough to prevent false positives.
func DetectAPIError(content string) *APIError {
	matches := apiErrorRE.FindAllStringSubmatchIndex(content, -1)
	if len(matches) == 0 {
		return nil
	}
	// Require the input prompt to be visible; the session must be idle, not
	// actively running tools (which would suppress the lone ❯ line). The live
	// prompt (the last column-0 ❯) is also the boundary between rendered output
	// above it and the user's input buffer below: an error quoted into the input
	// (pasted TUI text rendered as indented continuation lines) sits below the
	// prompt and must be ignored. This is purely positional, so an arbitrary
	// number of lines between the error and the prompt does not matter.
	prompts := inputPromptRE.FindAllStringIndex(content, -1)
	if len(prompts) == 0 {
		return nil
	}
	lastPrompt := prompts[len(prompts)-1][0]
	// Walk matches newest-first and return the most recent one that is both
	// above the live prompt AND carries a well-formed payload. A genuine Claude
	// Code error line renders clean JSON with a non-empty message. Source code
	// or test fixtures shown in the pane do not: this very codebase embeds
	// ⎿ "API Error:" literals, and when displayed they appear as Go string
	// literals with backslash-escaped quotes, so the JSON fails to parse (or has
	// no message). Skipping those keeps the daemon from clearing a session just
	// because its own detection code is on screen. A malformed match nearer the
	// prompt does not mask a real error further up; the loop keeps scanning.
	for i := len(matches) - 1; i >= 0; i-- {
		m := matches[i]
		if m[0] >= lastPrompt {
			continue // inside the input buffer, not a live error
		}
		jsonStr := content[m[4]:m[5]]
		var payload struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
			RequestID string `json:"request_id"`
		}
		if err := json.Unmarshal([]byte(jsonStr), &payload); err != nil || payload.Error.Message == "" {
			continue
		}
		statusCode, _ := strconv.Atoi(content[m[2]:m[3]])
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
	return nil
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
	// Transient gateway rate-limit takes precedence when present: it's only
	// rendered after a retry, so a hit here means the previous "continue" was
	// throttled and another short-delay retry is the right next step - not
	// deferring to the next usage reset. The regex already includes the ⎿
	// tool-output marker so no further line-prefix check is needed.
	if m := transientPattern.FindStringIndex(content); m != nil && m[0] >= threshold {
		text := strings.Join(strings.Fields(content[m[0]:m[1]]), " ")
		if len(text) > 160 {
			text = text[:160]
		}
		return &Banner{Backoff: transientShortBackoff, Text: text}
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
			reset, _ := parseReset(dateStr, timeStr, tzStr, now)
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
	// If the live input box (its mode/shortcut hint) sits below the phrase, the
	// phrase is text inside or above the user's input, pasted TUI output or
	// scrollback, not the live picker overlay, which replaces the input box.
	if inputBoxRE.MatchString(content[last[1]:]) {
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
	Model    string    // Claude model ID extracted from TUI status bar, empty if not visible
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
			Model:    modelRE.FindString(content),
		})
	}
	return out
}

// parseReset interprets banner date/time strings and returns the resolved
// datetime, or an error on unparseable input. When the banner gave only a
// clock-time and no date, the day is inferred via nearest-occurrence.
func parseReset(dateStr, timeStr, tzStr string, now time.Time) (time.Time, error) {
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
			return target, nil
		}
		// Date couldn't be parsed; fall through to clock-only inference.
	}

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

// launchSession recreates a tmux session for name at dir, running the Claude
// launch command as the pane's program (then dropping to a shell on exit) and
// appending the resume flag when there is prior history. An empty command
// just starts a plain shell.
func launchSession(cfg Config, name, dir string) {
	command := ""
	if cfg.ClaudeCommand != "" {
		cmdLine := strings.NewReplacer("{name}", shellout.Quote(name), "{dir}", shellout.Quote(dir)).Replace(cfg.ClaudeCommand)
		if cfg.ClaudeResumeFlag != "" && HasHistory(dir) {
			cmdLine += " " + cfg.ClaudeResumeFlag
		}
		command = cmdLine + `; exec "${SHELL:-bash}"`
	}
	if _, err := tmux.NewSession(name, dir, command); err != nil {
		slog.Error("recreate session failed", "session", name, "err", err)
	}
}

// HasHistory reports whether Claude Code has a prior session transcript for dir.
func HasHistory(dir string) bool {
	entries, err := os.ReadDir(claudeProjectDir(dir))
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
			return true
		}
	}
	return false
}

// claudeProjectDir derives the Claude Code project directory for a given
// working directory. Claude Code encodes the path by replacing every
// non-alphanumeric rune with '-' (so '/', '.', '_', ' ' all become '-').
func claudeProjectDir(workDir string) string {
	home, _ := os.UserHomeDir()
	encoded := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return '-'
	}, workDir)
	return filepath.Join(home, ".claude", "projects", encoded)
}

// claudeMemoryPath derives the Claude Code project memory directory for a
// given working directory, following Claude Code's path-encoding convention:
// replace every "/" with "-" (the leading "/" becomes a leading "-").
func claudeMemoryPath(workDir string) string {
	return filepath.Join(claudeProjectDir(workDir), "memory")
}

// ModelFromDir reads the model name from the most recent JSONL session for
// the given working directory. Returns "" if the project has no session files.
func ModelFromDir(workDir string) string {
	f := recentSessionFile(workDir)
	if f == "" {
		return ""
	}
	data, err := os.ReadFile(f)
	if err != nil {
		return ""
	}
	// Scan lines in reverse to find the most recent message with a "model" field.
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := lines[i]
		if !strings.Contains(line, `"model"`) {
			continue
		}
		var entry struct {
			Message struct {
				Model string `json:"model"`
			} `json:"message"`
		}
		// Skip angle-bracketed sentinels like "<synthetic>" that Claude Code
		// writes for harness-generated messages; they're not real model ids
		// and would shadow the actual model from the last assistant turn.
		if err := json.Unmarshal([]byte(line), &entry); err == nil && entry.Message.Model != "" && !strings.HasPrefix(entry.Message.Model, "<") {
			return entry.Message.Model
		}
	}
	return ""
}

// recentSessionFile returns the path of the most recently modified .jsonl
// session file in the Claude Code project directory for workDir. Returns ""
// if none is found. Call this before sending /clear; /clear causes Claude
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

	// Read the tail; 100 KB is enough to cover 30 records with room to spare.
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
const compactResumeMessage = "The conversation was just compacted automatically to recover from a stuck state. Continue the current task immediately without asking the user anything; just resume."

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
		imageAdvice = "Before resuming: save a memory about how to avoid this error; " +
			"always verify screenshot/image files are valid (check size > 10 KB " +
			"and run `file <path>` to confirm format) before passing them to the Read tool. " +
			"Never read a file that was just written by a tool without checking it first. "
	}
	return fmt.Sprintf(
		"The conversation was just cleared automatically. "+
			"The session was stuck on API Error %d (%s); this also caused /compact to fail. "+
			"%s"+
			"%s"+
			"Then immediately continue working without asking the user anything. "+
			"Do not summarize, do not ask what to work on; just resume.",
		apiErr.StatusCode, apiErr.Message, imageAdvice, contextSection)
}

func errorStatePath(statePath string) string {
	ext := filepath.Ext(statePath)
	return statePath[:len(statePath)-len(ext)] + "-errors" + ext
}

func LoadErrorState(path string) ErrorState {
	data, err := os.ReadFile(errorStatePath(path))
	if err != nil {
		return make(ErrorState)
	}
	var s ErrorState
	if err := json.Unmarshal(data, &s); err != nil {
		return make(ErrorState)
	}
	if s == nil {
		s = make(ErrorState)
	}
	return s
}

func SaveErrorState(path string, state ErrorState) error {
	p := errorStatePath(path)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// ManagedSession tracks a session daemon knows about.
// Sessions are automatically tracked as soon as the daemon observes them.
//
// Pinned sessions are always recreated by the daemon, even after a system
// restart; because the daemon persists their state to disk.
//
// Keep-alive sessions (governed by the global KeepAlive config flag, or the
// per-session KeepAlive field) are recreated whenever they vanish without a
// clean close signal; including across a system restart, which is the whole
// point of keep-alive: the sessions you had running come back.
//
// ExitedCleanly records whether the session got a clean goodbye signal; set
// by `proj close` or by the shell exit trap in proj.zsh / proj.bash /
// proj.fish, both of which call `proj daemon mark-closed <name>` before the
// session is destroyed. It distinguishes a deliberate exit from a crash or a
// reboot (which leave it false), so keep-alive can skip the former.
type ManagedSession struct {
	Name          string    `json:"name"`
	Dir           string    `json:"dir"`            // working directory, captured while alive
	Pinned        bool      `json:"pinned"`         // always recreate, survives system restart
	KeepAlive     bool      `json:"keep_alive"`     // recreate if not cleanly closed
	ExitedCleanly bool      `json:"exited_cleanly"` // got a clean goodbye (proj close / shell trap)
	SeenAt        time.Time `json:"seen_at"`        // last time daemon observed session alive
}

// ManagedState is the persisted map of all sessions proj daemon knows about.
type ManagedState map[string]ManagedSession // session name → entry

func managedStatePath(statePath string) string {
	ext := filepath.Ext(statePath)
	return statePath[:len(statePath)-len(ext)] + "-sessions" + ext
}

func LoadManagedState(path string) ManagedState {
	data, err := os.ReadFile(managedStatePath(path))
	if err != nil {
		return make(ManagedState)
	}
	var s ManagedState
	if err := json.Unmarshal(data, &s); err != nil {
		return make(ManagedState)
	}
	if s == nil {
		s = make(ManagedState)
	}
	return s
}

func SaveManagedState(path string, state ManagedState) error {
	p := managedStatePath(path)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
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
// dismissal is handled separately in Tick; it's a side-channel concern,
// independent of banner state.
type Action int

const (
	ActNone   Action = iota // no banner present
	ActResume               // banner visible, retry due → send "continue"
	ActWait                 // banner visible but scheduled retry is in the future
)

func Decide(content string, prev Tracked, now time.Time) Action {
	b := Detect(content, now)
	if b == nil {
		return ActNone
	}
	// Transient errors override any long deferral from a stale usage banner:
	// the previous defer was scheduled for a usage reset, but the live banner
	// says we're being throttled and just need to try again soon. Honour the
	// backoff window since the last attempt to avoid hammering on every poll.
	if b.Backoff > 0 {
		if !prev.LastActed.IsZero() && now.Sub(prev.LastActed) < b.Backoff {
			return ActWait
		}
		return ActResume
	}
	if !prev.NextAttempt.IsZero() && now.Before(prev.NextAttempt) {
		return ActWait
	}
	return ActResume
}

// nextAttemptAfter computes when the next retry should fire if this one
// fails. Trust the banner's time: explicit dates are used as-is; clock-only
// banners are advanced to the next future occurrence of that clock. If the
// banner had no parseable time at all, fall back to a fixed retry after
// MaxWait.
func nextAttemptAfter(b *Banner, now time.Time, cfg Config) time.Time {
	if b.Backoff > 0 {
		return now.Add(b.Backoff + jitter())
	}
	if b.Reset.IsZero() {
		return now.Add(cfg.MaxWait + jitter())
	}
	next := b.Reset
	for !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next.Add(jitter())
}

// jitter returns a random delay in [0, jitterMax). The reset minute is
// global to a timezone, so without spread every deferred client would hit
// the API at the same instant - exactly the "Server is temporarily limiting
// requests" burst the transient handler is there to clean up after. The
// window doesn't need to scale with the wait; the goal is just to break up
// the thundering herd at the boundary, which a few tens of seconds does
// regardless of whether the wait was a minute or a day.
const jitterMax = 30 * time.Second

func jitter() time.Duration {
	return time.Duration(rand.Int63n(int64(jitterMax)))
}

// mergeRenamedAliases folds the pin and keep-alive flags from stale managed
// entries onto the live session now running in the same directory, then drops
// the stale aliases. Managed state is keyed by tmux session name, which changes
// when a project's tags or the name format change; reconciling by directory
// keeps a project's pin attached to whatever its session is now called. A stale
// entry whose directory has no live session is left alone (it may be a genuinely
// vanished pinned session to recreate).
func mergeRenamedAliases(managed ManagedState, liveSessionMap map[string]tmux.Session, liveNameByDir map[string]string) {
	for name, ms := range managed {
		if _, alive := liveSessionMap[name]; alive {
			continue
		}
		if ms.Dir == "" {
			continue
		}
		liveName, ok := liveNameByDir[ms.Dir]
		if !ok || liveName == name {
			continue
		}
		live := managed[liveName]
		live.Pinned = live.Pinned || ms.Pinned
		live.KeepAlive = live.KeepAlive || ms.KeepAlive
		managed[liveName] = live
		delete(managed, name)
	}
}

func Tick(cfg Config, state State, errorState ErrorState, managed ManagedState, now time.Time) {
	// --- Session management: keep-alive and pinned recreation ---
	liveSessions := tmux.ListSessions()
	liveSessionMap := make(map[string]tmux.Session, len(liveSessions))
	liveNameByDir := make(map[string]string, len(liveSessions))
	for _, s := range liveSessions {
		liveSessionMap[s.Name] = s
		if s.Path != "" {
			liveNameByDir[s.Path] = s.Name
		}
	}

	// Upsert every live session into managed state.
	for _, s := range liveSessions {
		prev := managed[s.Name]
		prev.Name = s.Name
		// Only record Dir if not already set; preserves dirs set via `proj daemon pin --dir`.
		if prev.Dir == "" {
			prev.Dir = s.Path
		}
		prev.SeenAt = now
		// If the session is alive again after a clean exit, clear the flag.
		prev.ExitedCleanly = false
		managed[s.Name] = prev
	}

	// For sessions we know about that are no longer live:
	// Reconcile renamed sessions before deciding what to recreate: a live
	// session running in a stale entry's directory under a new name (tag change
	// or session-name format change) inherits that entry's pin/keep-alive, and
	// the stale alias is dropped. Keeps flags across renames and stops aliases
	// from piling up (or being spuriously recreated below).
	mergeRenamedAliases(managed, liveSessionMap, liveNameByDir)

	for name, ms := range managed {
		if _, alive := liveSessionMap[name]; alive {
			continue
		}
		if ms.ExitedCleanly {
			if ms.Pinned {
				// Pinned sessions are always recreated; a clean exit (which sets
				// ExitedCleanly via the trap) is not honored as a stop signal.
				// Clear the flag and fall through to recreate below.
				ms.ExitedCleanly = false
				managed[name] = ms
			} else {
				// Keep-alive or plain tracked: exited cleanly, stop tracking.
				delete(managed, name)
				continue
			}
		}
		if ms.Pinned {
			slog.Info("recreate pinned session", "session", name, "dir", ms.Dir)
			launchSession(cfg, name, ms.Dir)
		} else if ms.KeepAlive || cfg.KeepAlive {
			slog.Info("recreate keep-alive session", "session", name, "dir", ms.Dir)
			launchSession(cfg, name, ms.Dir)
		} else {
			// Nothing to recreate: the session is gone and is neither pinned nor
			// kept alive. Stop tracking it so dead entries don't accumulate.
			delete(managed, name)
		}
	}

	// --- Existing pane-level banner/error loop ---
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
		//    banner state; a stuck prompt is itself something to resolve.
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
				// /compact failed (either this run or a prior daemon run) -
				// the broken content is embedded in history. Fall back to
				// /clear, then send Claude an explanation so it can resume from memory.
				workDir := tmux.PaneCurrentPath(p.ID)
				// Extract context BEFORE /clear; /clear causes Claude Code to
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
						// separate Enter; this guarantees Enter only arrives after
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
				// First sighting; record, wait for next tick to confirm.
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
					// /compact was sent but the error is still visible; the
					// compact itself may have failed (e.g. broken image in
					// history). Warn every poll so the user knows to intervene.
					slog.Warn("compact sent but error persists; manual intervention needed",
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
		"poll", cfg.Poll, "max_wait", cfg.MaxWait,
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
	errorState := LoadErrorState(cfg.StatePath)
	if len(errorState) > 0 {
		slog.Info("loaded error state", "tracked", len(errorState))
	}
	tick := 0
	for {
		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("tick panicked", "panic", r)
				}
			}()
			// Reload managed state each tick so CLI changes (pin/unpin/mark-closed)
			// are picked up without requiring a daemon restart.
			managed := LoadManagedState(cfg.StatePath)
			Tick(cfg, state, errorState, managed, time.Now())
			if err := SaveState(cfg.StatePath, state); err != nil {
				slog.Error("save state failed", "err", err)
			}
			if err := SaveErrorState(cfg.StatePath, errorState); err != nil {
				slog.Error("save error state failed", "err", err)
			}
			if err := SaveManagedState(cfg.StatePath, managed); err != nil {
				slog.Error("save managed state failed", "err", err)
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
