// Package daemon watches tmux panes for Claude Code's usage-limit banner
// and resumes them by dismissing any "stop and wait" selector then typing
// "continue". Acts on banner presence alone; does not gate on the reset
// time printed in the banner, because that time is informational (the
// underlying limit may have already cleared).
package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tieo/proj/internal/config"
	"github.com/tieo/proj/internal/projects"
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
	BaseDir     string
	Poll        time.Duration
	MaxWait     time.Duration // fallback retry interval when the banner has no parseable time
	DismissGap  time.Duration // pause between Escape and "continue"
	ResumeText  string
	CompactText string // slash command to compact a stuck session
	ClearText   string // slash command to clear when compact itself fails
	Capture     int
	StatePath   string
	KeepAlive   bool                       // recreate vanished sessions that weren't cleanly closed
	Tools       map[string]config.ToolSpec // resolved launch recipes keyed by tool name, "claude" included
	ClaudeHome  string                     // [claude] home override; where Claude Code keeps transcripts (the Windows home under WSL)
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

// bulletErrorRE matches recoverable API errors that Claude Code renders as an
// assistant bullet ("● API Error: ...") rather than ⎿ tool output: dropped or
// interrupted connections, the transient gateway rate limit, and server
// overload ("529 Overloaded", which carries no JSON so apiErrorRE misses it
// too). The ⎿-anchored
// apiErrorRE (HTTP status + JSON) and transientPattern miss the bullet form, so
// without this the session sits stalled (the observed case: a 1-hour stall on
// "● API Error: Server is temporarily limiting requests"). The leading ● is
// required - it tells a live, TUI-rendered error apart from the same phrase in
// tool output, a code block, prose, or this source. A "continue" recovers all
// of them; bulletErrorResumable adds the idle guards (the ● line persists in
// scrollback, so a bare match would otherwise re-fire every poll).
var bulletErrorRE = regexp.MustCompile(`(?m)^` + sp + `*●` + sp + `+API Error:` + sp + `+(?:Connection closed mid-response|Connection error|stream (?:closed|disconnected|error)|fetch failed|socket hang ?up|terminated|premature close|network (?:error|connection lost)|ECONNRESET|Server is temporarily limiting requests|\d{3}` + sp + `+Overloaded)`)

// connDropNewerRE matches a newer assistant (●) or tool (⎿) line. If one sits
// between the error and the live prompt, Claude already produced fresh output
// (resumed), so the error is stale scrollback, not a live stall.
var connDropNewerRE = regexp.MustCompile(`(?m)^` + sp + `*[●⎿]`)

// connDropBusyRE matches an active-generation signal: the interrupt hint, or a
// running spinner line ("Whisking… (2m 42s · …"). The "… (" timer signature is
// present only while generating; a finished status ("Cogitated for 2m 27s")
// has no ellipsis. If busy, the session is not stalled - do not inject.
var connDropBusyRE = regexp.MustCompile(`esc to interrupt|…` + sp + `*\(`)

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

// feedbackPromptRE matches Claude Code's post-session rating prompt ("How is
// Claude doing this session?" with its 1:Bad / 2:Fine / 3:Good / 0:Dismiss
// options). Claude renders it as chrome above the input box when a session
// ends - including the moment a usage limit is hit, right below the limit
// banner. Its ●-prefixed header is not assistant output, so it must neither be
// mistaken for conversation that ages out the limit banner (see Detect's
// stale-banner guard) nor left sitting on screen (the daemon dismisses it).
var feedbackPromptRE = regexp.MustCompile(`(?im)^.*How is Claude doing this session\?.*$`)

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

var trustPromptRE = regexp.MustCompile(`(?is)Accessing workspace:\s+(.+?)\s+Quick safety check:\s+Is this a project you created or one you trust\?.*❯\s+1\.\s+Yes, I trust this folder`)

func HasTrustPrompt(content string) bool {
	return trustPromptRE.MatchString(content)
}

func autoTrustPath(baseDir, path string) bool {
	if path == "" {
		return false
	}
	clean := filepath.Clean(path)
	if baseDir != "" {
		if rel, err := filepath.Rel(filepath.Clean(baseDir), clean); err == nil && rel != "." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != ".." {
			return true
		}
	}
	slash := filepath.ToSlash(clean)
	return strings.HasPrefix(slash, "/tmp/claude-") && strings.Contains(slash, "/scratchpad/")
}

// submitPrompt types text into a pane and submits it as a turn of its own.
// Codex reads a carriage return that arrives in the same input burst as the
// text as a pasted newline rather than a submit, so send-keys with a trailing
// Enter leaves the line sitting in the composer. Every later resume then
// appends to that same buffer, and the whole stack goes out as one prompt
// whenever the user finally presses Enter. Typing the text literally, letting
// the pane settle, then sending Enter on its own keeps the two bursts apart.
// Literal mode also stops tmux from resolving the text against its key names.
func submitPrompt(cfg Config, paneID, text string) error {
	if err := tmux.SendLiteral(paneID, text); err != nil {
		return err
	}
	time.Sleep(cfg.DismissGap)
	return tmux.SendKey(paneID, "Enter")
}

// rcActiveRE matches Claude Code's status-bar marker shown while Remote Control
// is bound ("Remote Control active" or "/rc active"). Its ABSENCE on a live
// claude pane means RC has dropped - claude has no auto-reconnect, so the
// session silently vanishes from claude.ai/code until someone runs /rc.
// Matched against the TUI chrome zone (see rcTUIZone): the ⏵⏵ status line
// plus the session-context line above it. Both are below the conversation
// separator, so they cannot be spoofed by prose or user input.
var rcActiveRE = regexp.MustCompile(`(?i)remote control active|/rc active`)

// rcFailedRE matches the failed-bind indicator so the watchdog re-tries it too.
// Like rcActiveRE, status-line-only: "/rc failed" in prose must not fire a nudge.
var rcFailedRE = regexp.MustCompile(`(?i)/rc failed|remote control failed`)

// statusLineRE isolates Claude Code's bottom status/affordance line - the one
// carrying the ⏵⏵ permission-mode cycler (U+23F5×2) or, in default mode, the
// "? for shortcuts" hint. Both anchors are TUI chrome: never emitted in
// Claude's prose or user-typed input.
var statusLineRE = regexp.MustCompile(`(?m)^.*(?:⏵⏵|\? for shortcuts).*$`)

// rcStatusLine returns Claude Code's bottom status line and whether one was
// found. Absence means the pane is not a live, idle Claude TUI (startup,
// trust-folder prompt, a plain shell), so the watchdog must leave it alone.
// The last match wins: the live status bar is the bottom-most such line.
func rcStatusLine(content string) (string, bool) {
	ms := statusLineRE.FindAllString(content, -1)
	if len(ms) == 0 {
		return "", false
	}
	return ms[len(ms)-1], true
}

// rcTUIZone returns the TUI chrome zone at the bottom of the pane and whether
// a status line was found. The zone starts at the session-context line above ⏵⏵
// and extends to the end of the pane content, capturing all lines that follow
// the ⏵⏵ line. RC state can appear on any of these lines:
//
//	[CAVEMAN] …  /rc active         ← above ⏵⏵ (most sessions)
//	⏵⏵ …        Remote Control active  ← on ⏵⏵ itself
//	             10% until auto-compact
//	             /rc active          ← below ⏵⏵ (e.g. when auto-compact % shown)
//
// Everything from the context line downward is TUI chrome — below the
// conversation separator (────) — and is never user-typed input or prose.
func rcTUIZone(content string) (zone string, ok bool) {
	ms := statusLineRE.FindAllStringIndex(content, -1)
	if len(ms) == 0 {
		return "", false
	}
	last := ms[len(ms)-1]
	// Walk back one line to include the session-context line above ⏵⏵.
	// content[:last[0]] ends with the \n that terminates the context line;
	// strip it before searching so LastIndex finds the newline that starts it.
	prefix := content[:last[0]]
	if len(prefix) > 0 && prefix[len(prefix)-1] == '\n' {
		prefix = prefix[:len(prefix)-1]
	}
	start := 0
	if prev := strings.LastIndex(prefix, "\n"); prev >= 0 {
		start = prev + 1
	}
	// Extend to end of content to include any extra status lines below ⏵⏵
	// (e.g. auto-compact percentage, /rc active on a separate line).
	return content[start:], true
}

// rcLinkRE matches the connected-RC marker in an escape-preserving capture:
// Claude Code renders "/rc" as an OSC 8 hyperlink to the session's
// claude.ai/code URL only while Remote Control is bound. The link (not any text)
// is the signal; a dropped session shows a plain "/rc" hint with no link. The
// anchor is the literal "/rc" immediately after the OSC 8 string terminator
// (ESC \\), so a claude.ai/code URL merely printed in the conversation cannot
// match.
var rcLinkRE = regexp.MustCompile("\x1b\\]8;[^\x1b]*claude\\.ai/code/session_[^\x1b]*\x1b\\\\/rc")

// rcLinkConnected reports whether the connected-RC hyperlink is present in the
// bottom chrome of an escape-preserving capture. It scopes to the last lines so
// a claude.ai/code link scrolled up in the conversation cannot be mistaken for
// the live status marker.
func rcLinkConnected(esc string) bool {
	lines := strings.Split(strings.TrimRight(esc, "\n"), "\n")
	if len(lines) > 8 {
		lines = lines[len(lines)-8:]
	}
	return rcLinkRE.MatchString(strings.Join(lines, "\n"))
}

// RCStatus returns the Remote Control state visible in a captured pane. plain is
// the escape-stripped capture (used to confirm a TUI zone is present at all);
// esc is the escape-preserving capture, where a bound session's "/rc" carries a
// claude.ai/code hyperlink. The older text markers ("/rc active", "Remote
// Control active") remain a fallback for builds that render them.
//
//	"active"   — the /rc hyperlink (or a legacy active marker) is present
//	"offline"  — zone present but no active marker (dropped or never bound)
//	""         — no TUI zone (splash, trust prompt, plain shell)
func RCStatus(plain, esc string) string {
	zone, ok := rcTUIZone(plain)
	if !ok {
		return ""
	}
	if rcLinkConnected(esc) || rcActiveRE.MatchString(zone) {
		return "active"
	}
	return "offline"
}

// rcPickerRE matches the Remote Control dialog that `/rc` opens regardless of
// whether RC is currently bound (header "Remote Control", options "Disconnect
// this session" / "Show QR code" / "Continue"). It is not a usage-limit picker,
// so HasSelector misses it (its options have no "❯ <digit>." form). The correct
// response is Enter (selects ❯ Continue): completes the binding if RC was
// dropped, or confirms/dismisses if already active. Escape cancels the binding.
// Anchored on the two option labels together - unique to this dialog - so prose
// mentioning "Disconnect" alone can't trip it.
var rcPickerRE = regexp.MustCompile(`(?s)Disconnect this session.*Show QR code`)

// rcChromeTail returns the last rcTailLines of a pane capture - the region
// where live TUI overlays render (the /rc picker, the status line). RC dialog
// matching runs against this tail, never the full capture: the daemon captures
// hundreds of scrollback lines, and a conversation that quotes the picker text
// ("Disconnect this session ... Show QR code" - e.g. Claude pasting the menu)
// would otherwise look like a real open modal and get an Enter keystroke every
// tick. A genuine picker always renders at the bottom; quoted prose scrolls up.
const rcTailLines = 25

func rcChromeTail(content string) string {
	lines := strings.Split(content, "\n")
	if len(lines) > rcTailLines {
		lines = lines[len(lines)-rcTailLines:]
	}
	return strings.Join(lines, "\n")
}

// rcNudgeCooldown bounds how often the watchdog re-sends /rc to one pane, so a
// session that genuinely can't bind (or one mid-restart) isn't spammed.
const rcNudgeCooldown = 5 * time.Minute

// rcStartupGrace is how long after a pane is first seen that the RC watchdog
// holds off. Claude Code with --remote-control auto-binds RC during startup;
// the binding is an async network round-trip that completes a few seconds after
// the input box first appears. Firing the watchdog immediately on the first
// tick after Claude is ready sends /rc into an already-binding session, opening
// a spurious picker on top of the auto-bind. Two minutes is conservative but
// well inside the 5-minute cooldown and far longer than any real bind latency.
const rcStartupGrace = 2 * time.Minute

// rcNudgedAt tracks the last /rc nudge per pane id. Tick runs single-threaded
// from Run, so no mutex is needed. Pruned alongside dead panes each tick.
var rcNudgedAt = map[string]time.Time{}

// rcPaneFirstSeen tracks when each pane was first observed by the watchdog.
// Used to enforce rcStartupGrace. Pruned alongside dead panes each tick.
var rcPaneFirstSeen = map[string]time.Time{}

// rcEverActive records whether RC was ever observed bound for a pane during its
// current life. The watchdog only re-binds a pane that was once active and then
// lost it (a silent drop), never one that has never bound - --remote-control
// auto-binds at startup, so an unbound-so-far pane is still connecting and a /rc
// nudge would only open the picker into live input. Pruned with dead panes.
//
// This is per-pane and wiped on daemon restart; ManagedSession.RCEverActive
// mirrors it durably (keyed by session) so a restart doesn't orphan a session
// that bound before it. The watchdog ORs the two.
var rcEverActive = map[string]bool{}

// rcConnLastGood holds the last non-nil RCBridges result and when it was read.
// RCBridges returns nil only if the sessions directory is momentarily unreadable;
// a nil result would make the watchdog fall back to the chrome marker and nudge -
// popping the /rc picker over live input on a session that is actually connected.
// Trusting the last good snapshot for a short window absorbs those blips: a
// genuine drop is still recovered once the snapshot ages past rcConnCacheTTL.
// Tick is single-threaded, so no mutex.
var (
	rcConnLastGood   map[string]bool
	rcConnLastGoodAt time.Time
)

// rcConnCacheTTL bounds how long an unreadable-directory blip keeps trusting the
// last good bridge snapshot before the watchdog falls back to the chrome marker.
// Long enough to ride out a blip, short enough that a real drop still recovers.
const rcConnCacheTTL = 3 * time.Minute

// rcEnabled reports whether the configured claude launch command opts into
// Remote Control, so the watchdog only nudges sessions that are supposed to
// have it.
func rcEnabled(cfg Config) bool {
	return strings.Contains(cfg.Tools[config.DefaultTool].Command, "--remote-control")
}

// RCName formats the Remote Control session title shown on claude.ai. A proj
// session name is "name@tag1+tag2" (no tags: just "name"); the RC title renders
// it as "name @host [tag1,tag2]" - a space before the @host suffix, tags
// trailing and lowercased, brackets dropped when none. ':' is avoided as a
// separator because it is not a legal
// Windows path char (claude.exe runs via WSL interop); '@' and the brackets are
// legal on both Linux and Windows.
func RCName(session, host string) string {
	name, tags := session, ""
	if i := strings.IndexByte(session, '@'); i >= 0 {
		name, tags = session[:i], session[i+1:]
	}
	out := name + " @" + host
	if tags != "" {
		out += " [" + strings.ToLower(strings.ReplaceAll(tags, "+", ",")) + "]"
	}
	return out
}

// RCBridges reports, per Remote Control session title, whether Claude Code
// currently holds a live bridge for it. Claude writes one <pid>.json per running
// session under <claude>/sessions/, carrying the RC title (name) and a
// bridgeSessionId: a session_... id while the bridge is bound, null when it is
// not. This is a local, network-free, non-rotating signal - unlike the sessions
// API (unreachable from some networks) and the TUI /rc marker (rotates with
// slash-hints). The title is keyed by RCName, matching how list and the watchdog
// look it up. A nil map (unreadable dir) tells callers to fall back. Titles are
// not unique across a session's restarts, so any live bridge under a title wins.
func RCBridges(homeOverride string) map[string]bool {
	dir := filepath.Join(claudeRoot(homeOverride), "sessions")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var s struct {
			Name            string  `json:"name"`
			BridgeSessionID *string `json:"bridgeSessionId"`
		}
		if json.Unmarshal(raw, &s) != nil || s.Name == "" {
			continue
		}
		out[s.Name] = out[s.Name] || (s.BridgeSessionID != nil && *s.BridgeSessionID != "")
	}
	return out
}

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

// inputPromptRE matches the Claude Code input prompt at the start of a line.
// Current builds render it as "> " where the space is a non-breaking space
// (U+00A0); older builds used "❯". Both are matched. Picker option lines
// (e.g. "❯ 1. Stop and wait") always have leading spaces, so they cannot start
// at column 0 and never match. The NBSP after ">" is what keeps this from
// matching ordinary "> " shell/diff/quote lines. Text in the input buffer
// ("> commit this") is intentionally matched; a session with unsent text AND a
// recent API error is still idle and should be recovered.
//
// NOTE: matching the live prompt is required for DetectAPIError's idle gate and
// for bulletErrorResumable. When Claude Code last changed this glyph (❯ → "> "),
// the old ^❯-only form silently disabled both - hence both alternatives here.
var inputPromptRE = regexp.MustCompile(`(?m)^(?:❯|>\x{00a0})`)

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

// bulletErrorResumable reports whether `content` shows a Claude session stalled
// at the prompt by a ●-rendered recoverable API error (see bulletErrorRE) that a
// "continue" would recover, and returns the matched error text. It is
// deliberately strict to avoid re-firing on the same error every poll (the ●
// line stays in scrollback):
//
//  1. The error must be a live ●-rendered "API Error: <recoverable phrase>".
//  2. The session must be idle - a live input prompt (inputPromptRE) present,
//     with the error ABOVE the last prompt (not pasted into the input buffer).
//  3. The error must be the latest output - no newer ●/⎿ line between it and the
//     prompt (otherwise Claude already resumed; the error is stale scrollback).
//  4. Not mid-generation - no interrupt hint or running spinner after the error.
//
// Found false → leave it alone (manual nudge); a false "continue" injected into
// a working session is worse than a missed auto-resume.
func bulletErrorResumable(content string) (string, bool) {
	locs := bulletErrorRE.FindAllStringIndex(content, -1)
	if len(locs) == 0 {
		return "", false
	}
	prompts := inputPromptRE.FindAllStringIndex(content, -1)
	if len(prompts) == 0 {
		return "", false
	}
	lastPrompt := prompts[len(prompts)-1][0]
	for i := len(locs) - 1; i >= 0; i-- {
		m := locs[i]
		if m[0] >= lastPrompt {
			continue // below the live prompt: quoted/pasted into the input buffer
		}
		region := content[m[1]:lastPrompt]
		if connDropNewerRE.MatchString(region) {
			continue // newer ●/⎿ output → already resumed; stale error
		}
		if connDropBusyRE.MatchString(region) {
			continue // still generating → not stalled
		}
		text := strings.Join(strings.Fields(content[m[0]:m[1]]), " ")
		if len(text) > 160 {
			text = text[:160]
		}
		return text, true
	}
	return "", false
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
	// ●-rendered recoverable API errors (dropped connection, or the transient
	// rate limit when it renders as a bullet instead of ⎿): same short-backoff
	// "continue", but only when the session is genuinely stalled at the prompt
	// (see bulletErrorResumable for the idle guards). Checked after the ⎿
	// rate-limit pattern; the idle guards reject stale scrollback, so it does
	// not need to precede the usage-limit banners.
	if text, ok := bulletErrorResumable(content); ok {
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
			// Stale-banner guard: a real usage-limit banner is the last thing on
			// screen - Claude hit the limit and stopped. If newer assistant (●)
			// or tool (⎿) output follows it, the banner is scrollback, not a live
			// stall: either a quoted/fixture banner (this repo's own tests embed
			// ⎿ banner fixtures, which is exactly how the proj pane self-flagged
			// "out of tokens") or one Claude already resumed past. Acting on it
			// would be a false positive.
			//
			// The post-limit feedback prompt is the exception: its ●-prefixed
			// header renders directly below the banner the instant the limit is
			// hit, so counting it as newer output would hide a live limit stall
			// (session shows healthy, never auto-resumes). Drop those lines before
			// the check - they confirm the limit, they do not postdate it.
			afterBanner := feedbackPromptRE.ReplaceAllString(content[end:], "")
			if connDropNewerRE.MatchString(afterBanner) {
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

// feedbackPromptVisible reports whether Claude Code's post-session rating
// prompt is on screen right now (in the recent portion of the capture, not
// buried in scrollback), so the daemon dismisses only a live one.
func feedbackPromptVisible(content string) bool {
	loc := feedbackPromptRE.FindStringIndex(content)
	return loc != nil && loc[0] >= len(content)-recentWindow
}

// PaneState summarises what the daemon currently sees for one pane.
type PaneState struct {
	Pane     tmux.Pane
	Tool     string    // tool running in the pane's project ("claude", "codex", ...)
	Banner   *Banner   // non-nil if a usage-limit banner is visible
	Selector bool      // a dismissable interactive picker is visible
	APIError *APIError // non-nil if stuck after a permanent API error
	Model    string    // Claude model ID extracted from TUI status bar, empty if not visible
	RC       string    // RCStatus: "active", "connecting", "dropped", or "" (no zone)
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
// The usage-limit / transient-error banner is read from the pane's session
// transcript (durable, scroll-proof), matching the daemon's resume path; the
// selector, RC, and model still come from the live pane. homeOverride selects
// the .claude root (WSL-aware) for locating each pane's transcript.
func ScanPanes(homeOverride string, captureLines int) []PaneState {
	panes := tmux.ListPanes()
	reg, _ := projects.LoadRegistry()
	now := time.Now()
	out := make([]PaneState, 0, len(panes))
	for _, p := range panes {
		// Non-claude panes are not classified: every detector here reads Claude
		// Code's TUI and transcript formats, and matching them against another
		// tool's output only invites false labels.
		workDir := tmux.PaneCurrentPath(p.ID)
		if tool := ToolName(reg.Tool(filepath.Base(workDir))); tool != config.DefaultTool {
			out = append(out, PaneState{Pane: p, Tool: tool})
			continue
		}
		content := tmux.CapturePane(p.ID, captureLines)
		sessFile := recentSessionFile(homeOverride, workDir)
		out = append(out, PaneState{
			Pane:     p,
			Tool:     config.DefaultTool,
			Banner:   DetectFromTranscript(sessFile, now),
			Selector: HasSelector(content),
			APIError: DetectAPIError(content),
			Model:    modelRE.FindString(content),
			RC:       RCStatus(content, tmux.CapturePaneEsc(p.ID)),
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

// transcriptTailBytes bounds how much of a session transcript's end is read per
// poll. Large enough to hold recent activity (the last error plus any turns
// that resumed past it), small enough to parse cheaply on a multi-megabyte file.
const transcriptTailBytes = 256 * 1024

// usageLimitTextRE matches the reset-bearing usage-limit errors as Claude Code
// records them in the transcript. There is no ⎿ marker to lean on here - the
// isApiErrorMessage flag on the record already vouches this is a real limit
// event, not prose - so this matches the phrasing directly. The three lead
// forms ("out of extra usage", "session limit", "hit your limit") share the
// "· resets <time> (tz)" tail; date is optional, timezone optional.
var usageLimitTextRE = regexp.MustCompile(`(?i)(?:out of extra usage|session limit|hit your limit).*?resets\s+(?:([A-Za-z]+\s+\d{1,2}),\s+)?(\d{1,2}(?::\d{2})?\s*(?:am|pm))(?:\s*\(([A-Za-z_/+\-0-9]+)\))?`)

// transientTextRE matches the recoverable API errors whose fix is a short-delay
// retry ("continue"), not deferral to a usage reset: gateway rate limits,
// overloads, and dropped/failed connections.
var transientTextRE = regexp.MustCompile(`(?i)Server is temporarily limiting requests|\bOverloaded\b|Connection (?:error|closed|refused)|Unable to connect to API|stream (?:closed|disconnected|error)|fetch failed|socket hang ?up|ECONNRESET|terminated|premature close`)

// transcriptRecord is the subset of a transcript jsonl line the detector needs.
type transcriptRecord struct {
	Type              string `json:"type"`
	IsAPIErrorMessage bool   `json:"isApiErrorMessage"`
	Message           struct {
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type codexRolloutRecord struct {
	Type    string `json:"type"`
	Payload struct {
		Type             string  `json:"type"`
		Role             string  `json:"role"`
		Message          string  `json:"message"`
		LastAgentMessage *string `json:"last_agent_message"`
		RateLimits       *struct {
			LimitID string `json:"limit_id"`
			Primary *struct {
				UsedPercent float64 `json:"used_percent"`
				ResetsAt    int64   `json:"resets_at"`
			} `json:"primary"`
			Credits *struct {
				HasCredits bool `json:"has_credits"`
				Unlimited  bool `json:"unlimited"`
			} `json:"credits"`
			RateLimitReachedType *string `json:"rate_limit_reached_type"`
		} `json:"rate_limits"`
	} `json:"payload"`
}

func (r *codexRolloutRecord) isUserTurn() bool {
	return (r.Type == "event_msg" && r.Payload.Type == "user_message") ||
		(r.Type == "response_item" && r.Payload.Type == "message" && r.Payload.Role == "user")
}

func (r *codexRolloutRecord) isAgentTurn() bool {
	if r.Type == "event_msg" && r.Payload.Type == "agent_message" {
		return r.Payload.Message != ""
	}
	if r.Type == "event_msg" && r.Payload.Type == "task_complete" {
		return r.Payload.LastAgentMessage != nil && *r.Payload.LastAgentMessage != ""
	}
	if r.Type == "response_item" && r.Payload.Type == "message" && r.Payload.Role == "assistant" {
		return true
	}
	return false
}

func (r *codexRolloutRecord) codexReset() time.Time {
	rl := r.Payload.RateLimits
	if rl == nil || rl.LimitID != "codex" || rl.Primary == nil || rl.Primary.ResetsAt == 0 {
		return time.Time{}
	}
	return time.Unix(rl.Primary.ResetsAt, 0)
}

func (r *codexRolloutRecord) codexLimitReached() bool {
	rl := r.Payload.RateLimits
	if rl == nil {
		return false
	}
	if rl.RateLimitReachedType != nil && *rl.RateLimitReachedType != "" {
		return true
	}
	if rl.LimitID == "codex" && rl.Primary != nil && rl.Primary.UsedPercent >= 100 {
		return true
	}
	return rl.LimitID == "premium" && rl.Credits != nil && !rl.Credits.HasCredits && !rl.Credits.Unlimited
}

// isRealTurn reports whether the record proves the session resumed past an
// earlier error: a real-model assistant reply, and only that. The model
// emitting output is the sole evidence the API served the session again.
//
// A user record proves nothing. Claude Code stamps role "user" on injected
// system lines too - background <task-notification>s, tool results, queued
// commands - and none of those clear a usage limit, so counting them as a
// resume abandons a still-stalled session (its limit refills but no continue
// is ever sent). A genuinely typed message is no different: typing does not
// clear a limit, and if the limit had already passed the model's reply follows,
// which this catches on its own.
func (r *transcriptRecord) isRealTurn() bool {
	if r.IsAPIErrorMessage {
		return false
	}
	return r.Type == "assistant" && r.Message.Model != "" && r.Message.Model != "<synthetic>"
}

// isSyntheticError reports whether the record is one of Claude Code's locally
// injected error/limit messages (model "<synthetic>", isApiErrorMessage set).
func (r *transcriptRecord) isSyntheticError() bool {
	return r.IsAPIErrorMessage && r.Message.Model == "<synthetic>"
}

// recordContentText flattens a message's content (a bare string, or an array of
// {type,text} blocks) to plain text.
func recordContentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var b strings.Builder
		for _, x := range blocks {
			if x.Text != "" {
				b.WriteString(x.Text)
				b.WriteByte(' ')
			}
		}
		return strings.TrimSpace(b.String())
	}
	return ""
}

// bannerFromErrorText classifies a synthetic error's text into the retry the
// daemon should schedule: a usage-limit deferral to its reset time, or a
// short-backoff retry for a transient failure. Permanent errors that need the
// user (login, model unavailable) return nil - the daemon must not auto-resume
// them.
func bannerFromErrorText(text string, now time.Time) *Banner {
	clean := strings.Join(strings.Fields(text), " ")
	if len(clean) > 160 {
		clean = clean[:160]
	}
	if m := usageLimitTextRE.FindStringSubmatch(text); m != nil {
		reset, _ := parseReset(m[1], m[2], m[3], now)
		return &Banner{Reset: reset, Text: clean}
	}
	if transientTextRE.MatchString(text) {
		return &Banner{Backoff: transientShortBackoff, Text: clean}
	}
	return nil
}

// DetectFromTranscript reports a live usage-limit or transient-error stall read
// from a session transcript, or nil. It reads only the file's tail, finds the
// most recent synthetic error record, and returns a banner for it only if no
// real conversation turn follows - i.e. the session is still stalled there, not
// resumed past it. This is the durable, scroll-proof counterpart to scraping
// the pane: the transcript carries an explicit isApiErrorMessage flag (no prose
// false positives) and is unaffected by mouse-mode scroll position or viewport
// truncation.
func DetectFromTranscript(path string, now time.Time) *Banner {
	if path == "" {
		return nil
	}
	data := readFileTail(path, transcriptTailBytes)
	if len(data) == 0 {
		return nil
	}
	lines := strings.Split(string(data), "\n")
	recs := make([]*transcriptRecord, len(lines))
	lastErr := -1
	for i, ln := range lines {
		if ln == "" {
			continue
		}
		var r transcriptRecord
		if json.Unmarshal([]byte(ln), &r) != nil {
			continue
		}
		recs[i] = &r
		if r.isSyntheticError() {
			lastErr = i
		}
	}
	if lastErr < 0 {
		return nil
	}
	for _, r := range recs[lastErr+1:] {
		if r != nil && r.isRealTurn() {
			return nil // resumed past the error
		}
	}
	return bannerFromErrorText(recordContentText(recs[lastErr].Message.Content), now)
}

// DetectCodexFromRollout reports a Codex session stalled on a rate-limit turn.
// Codex records limits as structured token_count events. A real stalled turn is
// the latest user turn followed by a reached-limit token_count and no later
// agent_message/task_complete carrying an answer. The reset timestamp may sit
// on the previous ordinary codex limit record; premium credit exhaustion reuses
// that reset even though its own record has no primary window.
func DetectCodexFromRollout(path string, now time.Time) *Banner {
	if path == "" {
		return nil
	}
	data := readFileTail(path, transcriptTailBytes)
	if len(data) == 0 {
		return nil
	}
	var lastReset time.Time
	var candidate *Banner
	inTurn := false
	for _, ln := range strings.Split(string(data), "\n") {
		if ln == "" {
			continue
		}
		var rec codexRolloutRecord
		if json.Unmarshal([]byte(ln), &rec) != nil {
			continue
		}
		if reset := rec.codexReset(); !reset.IsZero() {
			lastReset = reset
		}
		if rec.isUserTurn() {
			inTurn = true
			candidate = nil
			continue
		}
		if !inTurn {
			continue
		}
		if rec.isAgentTurn() {
			inTurn = false
			candidate = nil
			continue
		}
		if rec.Type == "event_msg" && rec.Payload.Type == "token_count" && rec.codexLimitReached() {
			reset := rec.codexReset()
			if reset.IsZero() {
				reset = lastReset
			}
			text := "Codex usage limit reached"
			if rec.Payload.RateLimits != nil && rec.Payload.RateLimits.LimitID != "" {
				text += " (" + rec.Payload.RateLimits.LimitID + ")"
			}
			if !reset.IsZero() && !now.Before(reset) {
				candidate = &Banner{Backoff: transientShortBackoff, Text: text}
			} else {
				candidate = &Banner{Reset: reset, Text: text}
			}
		}
	}
	return candidate
}

// readFileTail returns the last n bytes of the file (all of it when smaller),
// dropping a leading partial line so callers get whole records.
func readFileTail(path string, n int64) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil
	}
	start := int64(0)
	if info.Size() > n {
		start = info.Size() - n
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil
	}
	if start > 0 {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		}
	}
	return data
}

// launchSession recreates a tmux session for name at dir, running the Claude
// launch command as the pane's program with the resume flag appended when
// there is prior history. The pane closes (and the session ends) when claude
// exits, matching the `proj <name>` open path; previously the daemon trailed
// `; exec $SHELL` here, which kept the pane alive in a fresh shell and left
// the user stranded in a tmux pane after closing claude.
//
// The launch command marks the session cleanly closed when claude itself exits
// successfully, so a recreated keep-alive session that the user then quits
// (Ctrl-D) stays closed instead of being resurrected again. The mark hangs off
// && rather than ;: tmux runs the command under a shell that outlives claude, so
// with ; the mark also ran when claude was killed (OOM, kill -9) and a killed
// session was recorded as a deliberate close, then dropped and never recreated.
// Gating on a zero exit keeps a signal death looking like what it is, leaving
// keep-alive to recreate it. Mirrors the suffix in cmd/proj/open.go; both are
// needed because claude is the pane program with no wrapping shell to carry the
// shells/proj.* exit trap.
func launchSession(cfg Config, name, dir string) {
	// Project key in the registry is the dir's basename - the flat layout means
	// every project lives at baseDir/<name>/.
	reg, regErr := projects.LoadRegistry()
	toolName := ToolName(reg.Tool(filepath.Base(dir)))
	spec, known := cfg.Tools[toolName]
	if !known {
		slog.Error("recreate skipped; project's tool has no launch recipe",
			"session", name, "dir", dir, "tool", toolName)
		return
	}
	command := ""
	if spec.Command != "" {
		command = LaunchCommand(spec, cfg.ClaudeHome, name, name, dir)
	}
	pane, err := tmux.NewSession(name, dir, command)
	if err != nil {
		slog.Error("recreate session failed", "session", name, "err", err)
		return
	}
	// Apply the project's configured slash-skills (e.g. "caveman") once claude's
	// input box is up, same as `proj <name>` does. Skills are Claude Code slash
	// commands; other tools don't get them.
	if regErr == nil && toolName == config.DefaultTool {
		if skills := reg.Skills(filepath.Base(dir)); len(skills) > 0 {
			tmux.ApplySlashCommands(pane, skills, 30*time.Second)
		}
	}
}

// ToolName normalizes a registry tool value: empty means claude.
func ToolName(tool string) string {
	if tool == "" {
		return config.DefaultTool
	}
	return tool
}

// LaunchCommand renders the pane command that launches spec's tool for a
// project. projName fills {name}; session names the tmux session (it carries
// the tag block) and feeds {rc} and the clean-close mark. The resume command
// is used instead of the base one only when the tool has prior history for
// dir, because tools don't treat resume-with-no-history as a no-op (claude -c
// exits with an error, tearing the fresh pane down before anyone can attach).
//
// The trailing mark hangs off && rather than ;: tmux runs the command under a
// shell that outlives the tool, so with ; the mark also ran when the tool
// was killed (OOM, kill -9) and a killed session was recorded as a deliberate
// close, then dropped and never recreated. Gating on a zero exit keeps a
// signal death looking like what it is, leaving keep-alive to recreate it.
func LaunchCommand(spec config.ToolSpec, claudeHome, projName, session, dir string) string {
	tpl := spec.Command
	if spec.ResumeCommand != "" && ToolHasHistory(spec.Name, claudeHome, dir) {
		tpl = spec.ResumeCommand
	}
	return renderLaunch(tpl, "", projName, session, dir)
}

// PromptLaunchCommand renders a fresh launch of spec's tool with an initial
// prompt, for handoffs to tools whose native store cannot be written. The
// prompt rides as a command-line argument (positional, or behind the tool's
// PromptFlag), never as injected keystrokes.
func PromptLaunchCommand(spec config.ToolSpec, projName, session, dir, prompt string) string {
	extra := ""
	if spec.PromptFlag != "" {
		extra = " " + spec.PromptFlag
	}
	// The prompt is appended after placeholder substitution: it is conversation
	// text and must never have {name}/{dir}/{rc} sequences rewritten inside it.
	extra += " " + shellout.Quote(prompt)
	return renderLaunch(spec.Command, extra, projName, session, dir)
}

func renderLaunch(tpl, extra, projName, session, dir string) string {
	host, _ := os.Hostname()
	cmdLine := strings.NewReplacer(
		"{name}", shellout.Quote(projName),
		"{dir}", shellout.Quote(dir),
		"{host}", host,
		"{rc}", shellout.Quote(RCName(session, host)),
	).Replace(tpl)
	return cmdLine + extra + " && proj daemon mark-closed " + shellout.Quote(session)
}

// RecentSessionFile exposes the newest Claude transcript for workDir to other
// packages (the handoff reader resolves its source file through it).
func RecentSessionFile(homeOverride, workDir string) string {
	return recentSessionFile(homeOverride, workDir)
}

// ToolHasHistory reports whether the named tool has a prior session for dir.
// Tools without a history detector always launch fresh.
func ToolHasHistory(tool, claudeHome, dir string) bool {
	switch ToolName(tool) {
	case config.DefaultTool:
		return HasHistory(claudeHome, dir)
	case "codex":
		return CodexHasHistory(dir)
	case "agy":
		return AgyHasHistory(dir)
	default:
		return false
	}
}

// AgyHasHistory reports whether the Antigravity CLI recorded a conversation
// for dir. agy appends one record per prompt to history.jsonl, each carrying
// the workspace it ran in; `agy --continue` restores the most recent
// conversation of the current workspace, so a workspace match here lines up
// with what the resume will find.
func AgyHasHistory(dir string) bool {
	home, _ := os.UserHomeDir()
	path := filepath.Join(home, ".gemini", "antigravity-cli", "history.jsonl")
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var rec struct {
			Workspace string `json:"workspace"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) == nil && rec.Workspace == dir {
			return true
		}
	}
	return false
}

// HasHistory reports whether Claude Code has a prior session transcript for dir.
// homeOverride is the [claude] home setting (may be ""); claudeRoot uses it to
// find the right .claude, which is what makes this correct under WSL, where
// transcripts live in the Windows home rather than $HOME.
func HasHistory(homeOverride, dir string) bool {
	pd := locateProjectDir(claudeRoot(homeOverride), dir, tmux.IsWSL())
	if pd == "" {
		return false
	}
	entries, err := os.ReadDir(pd)
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

// codexHome returns the Codex home directory ($CODEX_HOME, default ~/.codex).
// Rollout transcripts live under <home>/sessions/YYYY/MM/DD/rollout-*.jsonl.
func codexHome() string {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return h
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex")
}

// codexMetaBytes bounds how much of a rollout's head is read to find its cwd.
// The first line is the session_meta record; it embeds the full base
// instructions, so it runs to tens of KB.
const codexMetaBytes = 128 * 1024

// CodexHasHistory reports whether Codex recorded a session for dir. Codex
// keys rollouts by date, not by working directory; the cwd sits in each
// rollout's first line (the session_meta record), so this scans heads until
// one matches. `codex resume --last` filters sessions by cwd the same way,
// which is what makes gating the resume command on this check line up with
// what the resume will actually find.
func CodexHasHistory(dir string) bool {
	root := filepath.Join(codexHome(), "sessions")
	found := false
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		name := d.Name()
		if !strings.HasPrefix(name, "rollout-") || !strings.HasSuffix(name, ".jsonl") {
			return nil
		}
		if codexRolloutCwd(path) == dir {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// CodexModelFromDir reads the latest model recorded in Codex's rollout for dir.
func CodexModelFromDir(dir string) string {
	f := recentCodexRollout(dir)
	if f == "" {
		return ""
	}
	data, err := os.ReadFile(f)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := lines[i]
		if !strings.Contains(line, `"turn_context"`) || !strings.Contains(line, `"model"`) {
			continue
		}
		var rec struct {
			Type    string `json:"type"`
			Payload struct {
				Model string `json:"model"`
			} `json:"payload"`
		}
		if json.Unmarshal([]byte(line), &rec) == nil && rec.Type == "turn_context" && rec.Payload.Model != "" {
			return rec.Payload.Model
		}
	}
	return ""
}

func recentCodexRollout(dir string) string {
	root := filepath.Join(codexHome(), "sessions")
	var best string
	var bestTime time.Time
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		name := d.Name()
		if !strings.HasPrefix(name, "rollout-") || !strings.HasSuffix(name, ".jsonl") {
			return nil
		}
		info, err := d.Info()
		if err != nil || !info.ModTime().After(bestTime) {
			return nil
		}
		if codexRolloutCwd(path) == dir {
			best, bestTime = path, info.ModTime()
		}
		return nil
	})
	return best
}

// codexRolloutCwd extracts the cwd from a rollout's session_meta head line,
// or "" when the head is unreadable or not a session_meta record.
func codexRolloutCwd(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	head := make([]byte, codexMetaBytes)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	if i := bytes.IndexByte(head, '\n'); i >= 0 {
		head = head[:i]
	}
	var rec struct {
		Type    string `json:"type"`
		Payload struct {
			Cwd string `json:"cwd"`
		} `json:"payload"`
	}
	if json.Unmarshal(head, &rec) != nil || rec.Type != "session_meta" {
		return ""
	}
	return rec.Payload.Cwd
}

func agyHome() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".gemini", "antigravity-cli")
}

// AgyModelFromDir reads the latest selected Antigravity model label for dir.
func AgyModelFromDir(dir string) string {
	raw, err := os.ReadFile(filepath.Join(agyHome(), "cache", "last_conversations.json"))
	if err != nil {
		return ""
	}
	var byDir map[string]string
	if json.Unmarshal(raw, &byDir) != nil {
		return ""
	}
	conv := byDir[dir]
	if conv == "" {
		return ""
	}

	logs, _ := filepath.Glob(filepath.Join(agyHome(), "log", "cli-*.log"))
	sort.Slice(logs, func(i, j int) bool {
		ii, ierr := os.Stat(logs[i])
		ji, jerr := os.Stat(logs[j])
		if ierr != nil || jerr != nil {
			return logs[i] > logs[j]
		}
		return ii.ModTime().After(ji.ModTime())
	})

	labelRE := regexp.MustCompile(`label="([^"]+)"`)
	for _, path := range logs {
		if label := agyModelFromLog(path, conv, labelRE); label != "" {
			return label
		}
	}
	return ""
}

func agyModelFromLog(path, conv string, labelRE *regexp.Regexp) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	active := false
	latest := ""
	for sc.Scan() {
		line := sc.Text()
		if strings.Contains(line, conv) {
			active = true
		}
		if !active || !strings.Contains(line, "Propagating selected model override") {
			continue
		}
		if m := labelRE.FindStringSubmatch(line); len(m) == 2 {
			latest = m[1]
		}
	}
	return latest
}

// claudeRoot returns the .claude directory Claude Code actually uses for a
// working directory's transcripts. On bare Linux that is $HOME/.claude. Under
// WSL proj runs in Linux but launches claude.exe via interop, which writes to
// the *Windows* user's home, so probing $HOME/.claude always comes up empty -
// the bug that made keep-alive recreate sessions with no history. homeOverride
// ([claude] home) wins when set; otherwise under WSL we look under
// /mnt/c/Users/<user>, taking the Windows username to match the WSL one (the
// common setup) and falling back to the sole Users profile that has a .claude.
func claudeRoot(homeOverride string) string {
	if homeOverride != "" {
		return homeOverride
	}
	if tmux.IsWSL() {
		if home, err := os.UserHomeDir(); err == nil {
			if cand := filepath.Join("/mnt/c/Users", filepath.Base(home), ".claude"); isDir(cand) {
				return cand
			}
		}
		if m, _ := filepath.Glob("/mnt/c/Users/*/.claude"); len(m) == 1 {
			return m[0]
		}
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

// locateProjectDir returns the existing Claude Code project directory for
// workDir under root, or "" if none exists. On bare Linux the directory name is
// a direct encoding of workDir. Under WSL, claude.exe keys history on the
// \\wsl.localhost\<distro>\... UNC path; the daemon can't see the distro name
// (it is absent from the systemd service environment), but the encoded UNC name
// always ends with the encoding of workDir, so match by that suffix instead.
func locateProjectDir(root, workDir string, wsl bool) string {
	projectsDir := filepath.Join(root, "projects")
	if !wsl {
		if d := filepath.Join(projectsDir, encodeClaudePath(workDir)); isDir(d) {
			return d
		}
		return ""
	}
	suffix := encodeClaudePath(workDir)
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasSuffix(e.Name(), suffix) {
			return filepath.Join(projectsDir, e.Name())
		}
	}
	return ""
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// claudeProjectDir derives the Claude Code project directory for a given
// working directory on the local home. NOTE: this assumes $HOME/.claude and the
// plain workDir encoding, so it is correct on bare Linux but not under WSL
// (where claude.exe writes to the Windows home under a UNC-encoded name). For
// history detection use HasHistory, which resolves the real location.
func claudeProjectDir(workDir string) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "projects", encodeClaudePath(workDir))
}

// encodeClaudePath applies Claude Code's project-dir encoding: every
// non-alphanumeric rune becomes '-' (so '/', '.', '_', ' ', '\' all collapse).
func encodeClaudePath(p string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return '-'
	}, p)
}

// claudeMemoryPath derives the Claude Code project memory directory for a
// given working directory, following Claude Code's path-encoding convention:
// replace every "/" with "-" (the leading "/" becomes a leading "-").
func claudeMemoryPath(workDir string) string {
	return filepath.Join(claudeProjectDir(workDir), "memory")
}

// ModelFromDir reads the model name from the most recent JSONL session for
// the given working directory. Returns "" if the project has no session files.
func ModelFromDir(homeOverride, workDir string) string {
	f := recentSessionFile(homeOverride, workDir)
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
func recentSessionFile(homeOverride, workDir string) string {
	// Resolve the real transcript dir, which under WSL is the Windows-home,
	// UNC-encoded one (see locateProjectDir/claudeRoot), not $HOME/.claude.
	dir := locateProjectDir(claudeRoot(homeOverride), workDir, tmux.IsWSL())
	if dir == "" {
		return ""
	}
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
	RCEverActive  bool      `json:"rc_ever_active"` // Remote Control was observed bound at least once
}

// ManagedState is the persisted map of all sessions proj daemon knows about.
type ManagedState map[string]ManagedSession // session name → entry

func managedStatePath(statePath string) string {
	ext := filepath.Ext(statePath)
	return statePath[:len(statePath)-len(ext)] + "-sessions" + ext
}

// LoadManagedState reads the managed-state file. A missing file is an empty
// state and no error - the first run has nothing to remember. Anything else
// (unreadable, truncated, not valid JSON) is an error the caller must not
// paper over: pins live only here, so treating a corrupt file as "no sessions"
// and then saving over it destroys every pin silently.
func LoadManagedState(path string) (ManagedState, error) {
	data, err := os.ReadFile(managedStatePath(path))
	if errors.Is(err, os.ErrNotExist) {
		return make(ManagedState), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read managed state: %w", err)
	}
	var s ManagedState
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse managed state %s: %w", managedStatePath(path), err)
	}
	if s == nil {
		s = make(ManagedState)
	}
	return s, nil
}

// Clone returns a deep-enough copy to serve as the base of a later merge.
// ManagedSession is a flat comparable struct, so a value copy suffices.
func (s ManagedState) Clone() ManagedState {
	out := make(ManagedState, len(s))
	for k, v := range s {
		out[k] = v
	}
	return out
}

// withStateLock serializes read-modify-write cycles on the managed-state file
// across the daemon and every CLI command. The lock is advisory (flock) and
// tied to the open file description, so it is released even if the process
// dies. Nothing long-running may happen inside fn: a session launch takes tens
// of seconds and would block `proj pin` for that whole time.
func withStateLock(statePath string, fn func() error) error {
	p := managedStatePath(statePath) + ".lock"
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock managed state: %w", err)
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	return fn()
}

// UpdateManagedState applies mutate to the managed state under the state lock,
// reading the file inside the lock so the write cannot clobber a concurrent
// one. Every CLI mutation of pins, keep-alive, or the clean-exit mark goes
// through here.
func UpdateManagedState(statePath string, mutate func(ManagedState) error) error {
	return withStateLock(statePath, func() error {
		managed, err := LoadManagedState(statePath)
		if err != nil {
			return err
		}
		if err := mutate(managed); err != nil {
			return err
		}
		return SaveManagedState(statePath, managed)
	})
}

// CommitManagedState writes the daemon's post-Tick state back, merging it onto
// whatever is on disk now rather than overwriting. A tick spans tens of seconds
// (pane captures, transcript reads, session launches), and a `proj pin` landing
// inside that window used to be erased by the daemon's stale write.
func CommitManagedState(statePath string, base, ours ManagedState) error {
	return withStateLock(statePath, func() error {
		theirs, err := LoadManagedState(statePath)
		if err != nil {
			return err
		}
		return SaveManagedState(statePath, mergeManaged(base, ours, theirs))
	})
}

// mergeManaged three-way merges the daemon's view (ours, derived from base) with
// the file as it stands now (theirs). Where theirs is untouched since base the
// daemon's version wins. Where it changed, a CLI wrote concurrently and owns the
// intent fields (pinned, keep-alive, the clean-exit mark); the daemon only
// layers back the bookkeeping it alone maintains. An entry the daemon dropped is
// dropped only if nobody touched it meanwhile - otherwise a pin racing a delete
// would vanish.
func mergeManaged(base, ours, theirs ManagedState) ManagedState {
	out := theirs.Clone()
	for name, o := range ours {
		t, inTheirs := theirs[name]
		b, inBase := base[name]
		if !inTheirs {
			if !inBase {
				out[name] = o // the daemon added it; disk never had it
			}
			continue // otherwise: deleted on disk, respect that
		}
		if t == b {
			out[name] = o // disk untouched since our read
			continue
		}
		t.SeenAt = o.SeenAt
		t.RCEverActive = t.RCEverActive || o.RCEverActive
		if t.Name == "" {
			t.Name = o.Name
		}
		if t.Dir == "" {
			t.Dir = o.Dir
		}
		out[name] = t
	}
	for name, t := range theirs {
		if _, inOurs := ours[name]; inOurs {
			continue
		}
		if b, inBase := base[name]; inBase && t == b {
			delete(out, name) // the daemon dropped it and nobody else wrote it
		}
	}
	return out
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

func Decide(b *Banner, prev Tracked, now time.Time) Action {
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
	// Usage limit (no backoff). When the reset time is known and still ahead,
	// wait for it: a "continue" sent now only earns another limit message. A
	// reset already in the past (the limit has expired) or an unparseable time
	// falls through to an immediate resume. The transcript detector only
	// surfaces a banner the session is genuinely stalled on, so an old
	// already-reset limit yields no banner at all - the reason this can trust
	// the reset time rather than resuming on sight as a hedge against stale data.
	if !b.Reset.IsZero() && now.Before(b.Reset) {
		return ActWait
	}
	if !prev.NextAttempt.IsZero() && now.Before(prev.NextAttempt) {
		return ActWait
	}
	return ActResume
}

// transientBackoff grows the base retry delay geometrically with the number of
// prior consecutive attempts, capped at max. A persistent outage thus backs off
// from seconds toward hours instead of hammering a dead endpoint every minute,
// while a brief blip still retries fast. The loop doubles rather than shifting
// to avoid overflow and to stop once the cap is reached.
func transientBackoff(base time.Duration, attempts int, max time.Duration) time.Duration {
	d := base
	for i := 0; i < attempts && d < max; i++ {
		d *= 2
	}
	if d > max {
		d = max
	}
	return d
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
				slog.Info("drop tracked; session exited cleanly", "session", name, "dir", ms.Dir)
				delete(managed, name)
				continue
			}
		}
		// A recreate target whose directory is gone is a removed project, not a
		// vanished session: recreating it would spawn an orphan session at a
		// non-existent path (which tmux drops back to $HOME), and keep-alive would
		// respawn it every tick forever. Removing the project directory - however
		// it happened, `proj rm` or by hand - is the stop signal. Renamed projects
		// are already reconciled above (mergeRenamedAliases), so a missing dir here
		// means gone, not moved. Applies to pinned too: a pin cannot outlive its
		// directory.
		if ms.Dir != "" && !isDir(ms.Dir) {
			slog.Info("drop tracked; project directory removed", "session", name, "dir", ms.Dir)
			delete(managed, name)
			continue
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
			slog.Info("drop tracked; session gone, not pinned or kept alive", "session", name, "dir", ms.Dir)
			delete(managed, name)
		}
	}

	// --- Existing pane-level banner/error loop ---
	// The registry maps each pane's project (dir basename, flat layout) to its
	// tool, so the loop below can leave non-claude panes alone.
	reg, _ := projects.LoadRegistry()
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
	for id := range rcNudgedAt {
		if _, ok := live[id]; !ok {
			delete(rcNudgedAt, id)
		}
	}
	for id := range rcPaneFirstSeen {
		if _, ok := live[id]; !ok {
			delete(rcPaneFirstSeen, id)
		}
	}
	for id := range rcEverActive {
		if _, ok := live[id]; !ok {
			delete(rcEverActive, id)
		}
	}

	// Authoritative RC connection state, from Claude's own per-session bridge
	// files (RCBridges). This is a local, non-rotating signal: bridgeSessionId is
	// set while the bridge is bound and null when it is not. It replaces the
	// sessions API, which is unreachable from some networks (a 10s timeout every
	// tick, always "unknown", so the watchdog never rebound a real drop) and the
	// TUI marker, which rotates with slash-hints and gave false "offline". nil
	// only if the directory is unreadable, in which case the watchdog falls back
	// to the chrome-marker rules below and holds off (never nudges on unknown).
	rcConn := RCBridges(cfg.ClaudeHome)
	rcHost, _ := os.Hostname()
	// Keep the last good snapshot so an unreadable-dir blip doesn't flip a live
	// session to "unknown" for one tick. rcConnStale marks a cached snapshot.
	rcConnStale := false
	if rcConn != nil {
		rcConnLastGood, rcConnLastGoodAt = rcConn, now
	} else if rcConnLastGood != nil && now.Sub(rcConnLastGoodAt) < rcConnCacheTTL {
		rcConn, rcConnStale = rcConnLastGood, true
	}

	for _, p := range panes {
		paneDir := tmux.PaneCurrentPath(p.ID)
		tool := ToolName(reg.Tool(filepath.Base(paneDir)))
		if tool == "codex" {
			b := DetectCodexFromRollout(recentCodexRollout(paneDir), now)
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
			if b.Backoff > 0 {
				b.Backoff = transientBackoff(b.Backoff, prev.Attempts, cfg.MaxWait)
			}
			switch Decide(b, prev, now) {
			case ActWait:
				hadReset := !prev.Reset.IsZero()
				prev.LastSeen = now
				prev.Banner = b.Text
				prev.Reset = b.Reset
				if b.Backoff > 0 && !prev.LastActed.IsZero() && (prev.NextAttempt.IsZero() || prev.NextAttempt.Before(now) || hadReset) {
					prev.NextAttempt = prev.LastActed.Add(b.Backoff + jitter())
				} else if prev.NextAttempt.IsZero() {
					prev.NextAttempt = nextAttemptAfter(b, now, cfg)
				}
				state[p.ID] = prev
			case ActResume:
				slog.Info("resume codex",
					"session", p.Session, "pane", p.ID,
					"attempt", prev.Attempts+1, "banner", b.Text)
				if err := submitPrompt(cfg, p.ID, cfg.ResumeText); err != nil {
					slog.Error("send-keys failed", "session", p.Session, "err", err)
					continue
				}
				t := recordAction(prev, p, b, now, cfg)
				state[p.ID] = t
				slog.Info("deferred", "session", p.Session,
					"next", t.NextAttempt.Format("Mon 15:04 MST"))
			}
			continue
		}

		// Every detector and keystroke below speaks Claude Code's TUI
		// (its ⎿/● markers, /compact, /rc, "continue"); typing those into
		// other tools would inject garbage into a live session. Non-claude
		// panes only get the session-level keep-alive/pin handling above.
		if tool != config.DefaultTool {
			continue
		}
		content := tmux.CapturePane(p.ID, cfg.Capture)

		if HasTrustPrompt(content) {
			if autoTrustPath(cfg.BaseDir, paneDir) {
				slog.Info("accept trust prompt", "session", p.Session, "pane", p.ID, "dir", paneDir)
				if err := tmux.SendKey(p.ID, "Enter"); err != nil {
					slog.Error("send Enter failed", "session", p.Session, "err", err)
				}
				continue
			}
			slog.Warn("trust prompt left for user", "session", p.Session, "pane", p.ID, "dir", paneDir)
			continue
		}

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

		// 1a) Always clear Claude Code's post-session feedback prompt. It is not a
		//     decision the daemon should route around a running session: it blocks
		//     the pane and (below a usage-limit banner) hides a live limit stall.
		//     Escape does NOT close it; only its "0: Dismiss" option does, which
		//     records no rating. The feedbackPromptVisible gate keeps the bare "0"
		//     from ever reaching the input box when no prompt is up.
		if feedbackPromptVisible(content) {
			slog.Info("dismiss feedback prompt", "session", p.Session, "pane", p.ID)
			if err := tmux.SendKey(p.ID, "0"); err != nil {
				slog.Error("send feedback dismiss failed", "session", p.Session, "err", err)
			} else {
				time.Sleep(cfg.DismissGap)
				content = tmux.CapturePane(p.ID, cfg.Capture) // re-read post-dismiss
			}
		}

		// Compute TUI zone once; reused in both the picker-confirm and watchdog.
		zone, zoneOk := rcTUIZone(content)
		// Latch "ever active" the moment RC binds, so the watchdog can later tell
		// a silent drop (was active, now blank) from a pane still auto-binding.
		// Mirror it into the persisted managed state, keyed by session name: the
		// in-memory map is per-pane and wiped on every daemon restart, so without
		// this a session whose RC dropped while the daemon was down (or restarted)
		// would be treated as never-bound forever and never recovered.
		if zoneOk && rcActiveRE.MatchString(zone) {
			rcEverActive[p.ID] = true
			if ms, ok := managed[p.Session]; ok && !ms.RCEverActive {
				ms.RCEverActive = true
				managed[p.Session] = ms
			}
		}

		// 1b-pre) The /rc binding dialog (rcPickerRE) needs Enter, not Escape.
		//     The dialog appears whenever /rc is typed - whether RC is already
		//     bound or not - with ❯ Continue highlighted. Enter selects Continue:
		//     completes the binding if RC was dropped, or simply confirms if it
		//     was already active. Escape cancels the binding, which is exactly
		//     wrong and caused the TagHistory loop (daemon sent /rc, then Escaped
		//     every tick, so RC never bound, cooldown expired, repeat forever).
		//     HasSelector misses this picker (no "❯ <digit>." option), so it gets
		//     its own branch here with the correct key.
		//     Spoof guard: a real /rc modal REPLACES the input box - it renders
		//     "Disconnect this session ... Continue / Esc to continue" with no
		//     ⏵⏵ status line. So if the ⏵⏵ line is present (zoneOk), the normal
		//     input UI is up and any picker text is conversation prose (Claude
		//     quoting the menu, which renders right above the input every turn) -
		//     NOT a modal. Only confirm when ⏵⏵ is absent, i.e. the modal took
		//     over. Tail-matching alone is not enough: the quoted prose lands in
		//     the tail too, since it's the latest message above the prompt.
		if !zoneOk && rcPickerRE.MatchString(rcChromeTail(content)) {
			slog.Info("confirm RC binding dialog", "session", p.Session, "pane", p.ID)
			if err := tmux.SendKey(p.ID, "Enter"); err != nil {
				slog.Error("send Enter failed", "session", p.Session, "err", err)
			} else {
				time.Sleep(cfg.DismissGap)
				content = tmux.CapturePane(p.ID, cfg.Capture) // re-read post-confirm
				zone, zoneOk = rcTUIZone(content)             // refresh zone
			}
		}

		// 1b) Remote-Control watchdog. Claude Code's RC has no auto-reconnect:
		//     after a >10min network gap or a stale poll, the binding drops and
		//     the session disappears from claude.ai/code, even though the
		//     process is alive. The only recovery is the /rc slash command. If
		//     this is a live claude pane (input box present) whose RC marker is
		//     gone, re-send /rc. Cooldown-gated so a session that can't bind
		//     isn't hammered. Only acts when the launch command opted into RC.
		//     RC state is checked against the two-line TUI chrome zone (rcTUIZone):
		//     the ⏵⏵ status line and the session-context line above it. Both are
		//     below the conversation separator and are never user input or prose.
		//     This catches the intermediate connecting state (/rc active on the
		//     context line) that appears during --remote-control auto-bind, and
		//     Remote Control active on the status line when fully bound. rcFailedRE
		//     is checked against the status line only (failure is always on ⏵⏵).
		//     rcPickerRE guards against re-sending /rc while the binding dialog is
		//     already open (the 1b-pre block above is handling it with Enter).
		//     rcStartupGrace holds off on freshly-seen panes: --remote-control
		//     auto-binds RC during startup; the binding completes a few seconds
		//     after the input box appears, so the first tick after Claude is ready
		//     must not fire /rc into an already-binding session.
		if rcEnabled(cfg) && zoneOk &&
			!rcActiveRE.MatchString(zone) && !HasSelector(content) &&
			!rcPickerRE.MatchString(rcChromeTail(content)) {
			statusLine, _ := rcStatusLine(content)
			if _, known := rcPaneFirstSeen[p.ID]; !known {
				rcPaneFirstSeen[p.ID] = now
			}
			pastGrace := now.Sub(rcPaneFirstSeen[p.ID]) >= rcStartupGrace
			failed := rcFailedRE.MatchString(statusLine)
			// Recover only a drop or an explicit failure. A pane that has never
			// bound is still connecting (--remote-control auto-binds at startup);
			// nudging it just opens the picker into live input - the bug that
			// typed /rc into freshly-recreated sessions.
			// "ever active" is kept for the log line only; it no longer gates the
			// nudge (see below).
			everActive := rcEverActive[p.ID] || managed[p.Session].RCEverActive
			// Nudge only when a drop is POSITIVELY confirmed - never on the
			// unreliable chrome marker alone. The source of truth is RCBridges
			// (bridgeSessionId in Claude's per-session file): non-null means bound,
			// null means the bridge is down. It is authoritative and does not
			// rotate, so a connected session is never mistaken for disconnected -
			// which is what the false-picker guards (everActive, the API-unknown
			// hold-off) were protecting against. With a reliable signal the drop
			// itself is the trigger; rcStartupGrace still covers the initial
			// auto-bind window so a starting session (transiently null) is left be.
			//   - bridgeDropped: the file reports this session's bridge null, or
			//   - failed: the status line shows an explicit "/rc failed".
			// When RCBridges is UNKNOWN (rcConn nil - unreadable dir), hold off.
			bridgeDropped := rcConn != nil && !rcConn[RCName(p.Session, rcHost)]
			trigger := failed || (bridgeDropped && now.Sub(rcNudgedAt[p.ID]) >= rcNudgeCooldown)
			if pastGrace && trigger {
				slog.Info("remote-control inactive, re-binding",
					"session", p.Session, "pane", p.ID,
					"reason", map[bool]string{true: "failed", false: "drop"}[failed],
					"ever_active", everActive,
					"bridge_known", rcConn != nil,
					"bridge_stale", rcConnStale,
					"status_line", strconv.Quote(statusLine),
					"zone", strconv.Quote(zone))
				if err := tmux.SendKeys(p.ID, "/rc"); err != nil {
					slog.Error("send /rc failed", "session", p.Session, "err", err)
				}
				rcNudgedAt[p.ID] = now
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
				// Extract context BEFORE /clear; /clear causes Claude Code to
				// start a new session file, so the pre-clear file has the history.
				sessFile := recentSessionFile(cfg.ClaudeHome, paneDir)
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

		// 3) Handle a usage-limit / transient-error stall, read from the session
		//    transcript rather than the pane: the transcript carries an explicit
		//    isApiErrorMessage flag (no prose false positives) and is immune to
		//    scroll position (mouse mode) and viewport truncation, which the pane
		//    scrape is not. The pane is still used above for selectors and RC.
		sessFile := recentSessionFile(cfg.ClaudeHome, paneDir)
		b := DetectFromTranscript(sessFile, now)
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
		// A transient error that never clears escalates its backoff with each
		// failed attempt so a long outage self-throttles toward MaxWait instead
		// of retrying every minute forever. Written into the banner so Decide's
		// wait-gate and nextAttemptAfter schedule against the same value.
		if b.Backoff > 0 {
			b.Backoff = transientBackoff(b.Backoff, prev.Attempts, cfg.MaxWait)
		}
		switch Decide(b, prev, now) {
		case ActWait:
			hadReset := !prev.Reset.IsZero()
			prev.LastSeen = now
			prev.Banner = b.Text
			prev.Reset = b.Reset
			// Seed the scheduled retry on first sight so the wait is visible in
			// status and survives to fire once the reset passes.
			if b.Backoff > 0 && !prev.LastActed.IsZero() && (prev.NextAttempt.IsZero() || prev.NextAttempt.Before(now) || hadReset) {
				prev.NextAttempt = prev.LastActed.Add(b.Backoff + jitter())
			} else if prev.NextAttempt.IsZero() {
				prev.NextAttempt = nextAttemptAfter(b, now, cfg)
			}
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
			// are picked up without requiring a daemon restart. A corrupt file is
			// never written over: the tick runs against a throwaway state so the
			// pane loop still works, and the file is left for the user to inspect.
			managed, err := LoadManagedState(cfg.StatePath)
			commit := err == nil
			if err != nil {
				slog.Error("managed state unreadable; not touching it this tick (pins are only stored there)", "err", err)
				managed = make(ManagedState)
			}
			base := managed.Clone()
			Tick(cfg, state, errorState, managed, time.Now())
			if err := SaveState(cfg.StatePath, state); err != nil {
				slog.Error("save state failed", "err", err)
			}
			if err := SaveErrorState(cfg.StatePath, errorState); err != nil {
				slog.Error("save error state failed", "err", err)
			}
			if commit {
				if err := CommitManagedState(cfg.StatePath, base, managed); err != nil {
					slog.Error("save managed state failed", "err", err)
				}
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
