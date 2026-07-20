// Package goalnudge is the fleet judge. Given each session's recent transcript
// tail it asks, through `claude -p` on the subscription (not the paid API),
// whether the session reached its goal or stopped short, and returns a verdict
// per session. It is a pure judge: the daemon owns which sessions to look at,
// when, and what to do with the verdicts; this package only reads tails it is
// handed, calls the model, and keeps the per-session and usage state files.
//
// The call runs in a fixed scratch directory so Claude Code's own context stays
// prompt-cached across looks. Every look records its token usage - cache_read vs
// cache_creation - so cache warmth can be watched over real use.
package goalnudge

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// SessionState is one session's input to the goalnudge: its name, tool, the
// original task (the first user turn, when it can be read), a recent slice of
// its transcript, and what currently sits in its input box (pending).
type SessionState struct {
	Name    string `json:"name"`
	Tool    string `json:"tool"`
	Task    string `json:"task,omitempty"` // the session's original goal, first user turn
	Tail    string `json:"tail"`
	Pending string `json:"pending,omitempty"` // what's in the input box now: the agent's suggested next step, or a user draft
}

// Verdict is the goalnudge's per-session decision. Reason states why it chose the
// state, so a call can be understood and a wrong one caught.
type Verdict struct {
	Name       string `json:"name"`
	Goal       string `json:"goal"`
	State      string `json:"state"`  // done | stopped_short | blocked | working
	Reason     string `json:"reason"` // one sentence: why this state
	Callout    string `json:"callout"`
	NeedsUser  bool   `json:"needs_user"`
	UserReason string `json:"user_reason"`
}

// Usage is the goalnudge call's token accounting, the numbers that matter for the
// budget: cache_read is billed at ~10% and cache_creation at the write rate.
type Usage struct {
	Input       int `json:"input"`
	Output      int `json:"output"`
	CacheRead   int `json:"cache_read"`
	CacheCreate int `json:"cache_create"`
}

// LookResult is the outcome of one look: what was judged, the verdicts, and the
// goalnudge call's usage.
type LookResult struct {
	Sessions []SessionState
	Verdicts []Verdict
	Usage    Usage
	Raw      string // the goalnudge's raw text, kept when the JSON fails to parse
}

const scratchDirRC = "goalnudge"

// ScratchDir is the fixed non-git directory the goalnudge runs in, so Claude Code's
// system-prompt prefix (which embeds the working directory) stays identical
// across looks and keeps its prompt cache warm.
func ScratchDir() string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "proj", scratchDirRC)
}

// goalnudgeSystemPrompt is the goalnudge's role, passed as the session's system
// prompt (--system-prompt) so it defines the goalnudge role at the system level rather
// than competing with Claude Code's coding-agent framing in a user turn.
const goalnudgeSystemPrompt = `You are the goalnudge of a fleet of autonomous coding-agent sessions. You do not write code or use tools. Your only job: read each session's recent transcript and judge whether it reached its goal or stopped short.

Each user message is a JSON array of sessions, each with a name, tool, an optional task (the session's original goal, from its first message), a recent transcript tail, and an optional pending field (what is in the session's input box right now). For each session:
- take its goal from task when present, otherwise infer it from the tail,
- judge its state, which MUST be exactly one of these four strings: "working" (actively mid-task), "done" (the goal is fully met with nothing left to do), "stopped_short" (idle with the goal unmet and a path still open), "blocked" (needs something it cannot get itself). Use no other value,
- if the last message ends by asking the user a question or offering options to choose from, the state is "stopped_short" with needs_user true, however much was accomplished - it is waiting on the user, not done,
- a good stopping point is not "done": if any part of the goal (or a follow-up the agent itself named) remains, it is "stopped_short",
- read pending: "agent's suggested next step: X" is the agent proposing what to do next, so it is NOT done - state stopped_short and put X in callout; "user is typing ..." means the user is engaged, so leave it (working),
- if stopped_short, write callout: one imperative sentence telling the agent to continue toward the goal,
- set needs_user true when a real decision is required that the agent cannot make on its own (including when it asked the user a question),
- write reason: one sentence saying why you chose this state, citing the last thing that happened.

Reply with ONLY a JSON array, one object per session, no prose, no markdown fences:
[{"name":"...","goal":"...","state":"working|done|stopped_short|blocked","reason":"...","callout":"...","needs_user":false,"user_reason":""}]`

// promptContract is appended to every user turn to hold a resumed session to the
// output format (see the note in Look).
const promptContract = `Judge these sessions. Reply with ONLY the JSON array of verdicts, each with name, goal, state, reason, callout, needs_user, user_reason - no prose, no markdown fences.`

// Look runs one goalnudge call over the given session tails and returns the
// verdicts and token usage. It takes no action; the daemon acts on the verdicts.
// The sessions are built by the caller (the daemon, from transcripts it already
// reads). An empty model defaults to sonnet.
func Look(model string, sessions []SessionState) (LookResult, error) {
	res := LookResult{Sessions: sessions}
	if len(sessions) == 0 {
		return res, nil
	}

	payload, err := json.Marshal(sessions)
	if err != nil {
		return res, err
	}
	// Re-assert the output contract on every turn. On a resumed session the
	// system prompt is fixed at creation and not re-sent, so across turns the
	// model drifts from judging into conversational prose (echoing the tails it
	// is shown). This one line rides in the fresh, uncached delta and holds it to
	// the JSON array.
	prompt := string(payload) + "\n\n" + promptContract

	dir := ScratchDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return res, fmt.Errorf("goal-nudge scratch dir: %w", err)
	}

	if model == "" {
		model = "sonnet"
	}
	// Resume the standing session so Claude Code's system prompt and the global
	// CLAUDE.md stay in the warm prompt-cache prefix (read at 0.1x); only the new
	// turn is written. A fresh session each look would re-establish, and re-write,
	// that whole prefix. resume falls back to fresh on any failure (the session
	// was cleared, or Claude Code upgraded and its prefix changed).
	sid := readSessionID()
	out, err := runGoalNudge(dir, model, sid, prompt)
	if err != nil && sid != "" {
		out, err = runGoalNudge(dir, model, "", prompt)
	}
	if err != nil {
		return res, err
	}
	if out.SessionID != "" {
		writeSessionID(out.SessionID)
	}

	res.Usage = Usage{
		Input:       out.Usage.InputTokens,
		Output:      out.Usage.OutputTokens,
		CacheRead:   out.Usage.CacheReadInputTokens,
		CacheCreate: out.Usage.CacheCreationInputTokens,
	}
	res.Raw = out.Result
	res.Verdicts = parseVerdicts(out.Result)

	// The resumed session accumulates every look. Once its cached prefix grows
	// past resetAbove tokens, drop the id so the next look starts a fresh (small)
	// session, keeping per-look read cost bounded.
	if res.Usage.CacheRead > resetAbove {
		clearSessionID()
	}
	return res, nil
}

// resetAbove bounds the standing session's accumulated prefix. Past this the
// next look starts fresh.
const resetAbove = 120000

type goalnudgeOutput struct {
	Result    string `json:"result"`
	SessionID string `json:"session_id"`
	Usage     struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

func runGoalNudge(dir, model, resumeID, prompt string) (goalnudgeOutput, error) {
	args := []string{"-p", "--model", model, "--output-format", "json", "--dangerously-skip-permissions"}
	if resumeID != "" {
		// The session carries its system prompt and settings from creation; resume
		// just continues it.
		args = append(args, "--resume", resumeID)
	} else {
		// Fresh session: define the goalnudge as a pure judge via its own system
		// prompt, and drop user settings so the global CLAUDE.md (coding persona,
		// caveman mode) and hooks don't load into it or bloat the cached prefix.
		args = append(args,
			"--system-prompt", goalnudgeSystemPrompt,
			"--setting-sources", "project")
	}
	args = append(args, prompt)
	cmd := exec.Command("claude", args...)
	cmd.Dir = dir
	raw, err := cmd.Output()
	if err != nil {
		return goalnudgeOutput{}, fmt.Errorf("goal-nudge call: %w", err)
	}
	var out goalnudgeOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		return goalnudgeOutput{}, fmt.Errorf("parse goal-nudge output: %w", err)
	}
	return out, nil
}

func sessionIDPath() string { return filepath.Join(ScratchDir(), "session") }

func readSessionID() string {
	b, err := os.ReadFile(sessionIDPath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func writeSessionID(id string) { _ = os.WriteFile(sessionIDPath(), []byte(id), 0o644) }
func clearSessionID()          { _ = os.Remove(sessionIDPath()) }

// parseVerdicts extracts the JSON array from the goalnudge's reply, tolerating a
// markdown code fence around it.
func parseVerdicts(s string) []Verdict {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "["); i >= 0 {
		if j := strings.LastIndex(s, "]"); j > i {
			s = s[i : j+1]
		}
	}
	var v []Verdict
	if json.Unmarshal([]byte(s), &v) != nil {
		return nil
	}
	for i := range v {
		v[i].State = normalizeState(v[i].State)
	}
	return v
}

// normalizeState maps whatever the model emits to exactly one of the four valid
// states, so an off-spec value ("in_progress", "completed", …) can never reach
// the actions or the display. An unrecognised state falls back to "working":
// the safe no-op, so the goalnudge never nudges on a state it does not understand.
func normalizeState(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "done", "complete", "completed", "finished", "resolved":
		return "done"
	case "stopped_short", "stopped-short", "stoppedshort", "stopped", "short", "paused", "incomplete":
		return "stopped_short"
	case "blocked", "stuck", "waiting":
		return "blocked"
	default: // working, in_progress, active, and anything unrecognised
		return "working"
	}
}

// UsageLogPath is where each look's usage is appended as JSONL, so cache warmth
// (cache_read vs cache_create) can be analysed over real use.
func UsageLogPath() string {
	return filepath.Join(filepath.Dir(ScratchDir()), "goalnudge-usage.jsonl")
}

// LogUsage appends one look's usage record. at is passed in (not read from the
// clock) so the daemon controls the timestamp.
func LogUsage(at time.Time, r LookResult) error {
	rec := struct {
		TS          string `json:"ts"`
		Judged      int    `json:"judged"`
		Input       int    `json:"input"`
		Output      int    `json:"output"`
		CacheRead   int    `json:"cache_read"`
		CacheCreate int    `json:"cache_create"`
	}{
		TS:          at.Format(time.RFC3339),
		Judged:      len(r.Sessions),
		Input:       r.Usage.Input,
		Output:      r.Usage.Output,
		CacheRead:   r.Usage.CacheRead,
		CacheCreate: r.Usage.CacheCreate,
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(UsageLogPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}
