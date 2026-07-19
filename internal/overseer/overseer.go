// Package overseer is the fleet overseer: it reads each idle coding session's
// recent transcript, asks the overseer whether the session reached its goal or
// stopped short, and reports a verdict per session. The daemon runs it as a
// gated pass; `proj overseer once` runs the same Look() manually for a dry-run.
//
// The overseer runs through `claude -p` (the subscription, not the paid API) in a
// fixed scratch directory so Claude Code's own context stays prompt-cached
// across looks. Every look records its token usage - including cache_read vs
// cache_creation - so cache warmth can be watched over real use.
package overseer

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/tieo/proj/internal/config"
	"github.com/tieo/proj/internal/daemon"
	"github.com/tieo/proj/internal/handoff"
	"github.com/tieo/proj/internal/projects"
	"github.com/tieo/proj/internal/tmux"
)

// SessionState is one session's input to the overseer: its name, tool, and a
// recent slice of its transcript.
type SessionState struct {
	Name string `json:"name"`
	Tool string `json:"tool"`
	Tail string `json:"tail"`
}

// Verdict is the overseer's per-session decision.
type Verdict struct {
	Name       string `json:"name"`
	Goal       string `json:"goal"`
	State      string `json:"state"` // done | stopped_short | blocked | working
	Callout    string `json:"callout"`
	NeedsUser  bool   `json:"needs_user"`
	UserReason string `json:"user_reason"`
}

// Usage is the overseer call's token accounting, the numbers that matter for the
// budget: cache_read is billed at ~10% and cache_creation at the write rate.
type Usage struct {
	Input       int `json:"input"`
	Output      int `json:"output"`
	CacheRead   int `json:"cache_read"`
	CacheCreate int `json:"cache_create"`
}

// LookResult is the outcome of one look: what was judged, the verdicts, and the
// overseer call's usage.
type LookResult struct {
	Sessions []SessionState
	Verdicts []Verdict
	Usage    Usage
	Raw      string // the overseer's raw text, kept when the JSON fails to parse
}

const (
	tailTurns    = 12  // recent turns per session fed to the overseer
	tailTurnCap  = 500 // chars per turn in the tail
	scratchDirRC = "overseer"
)

// ScratchDir is the fixed non-git directory the overseer runs in, so Claude Code's
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

// BuildSnapshot reads the recent transcript tail of every live claude or codex
// session under baseDir. Sessions whose tool has no transcript reader, or whose
// transcript can't be read, are skipped.
func BuildSnapshot(cfg config.Config) []SessionState {
	reg, _ := projects.LoadRegistry()
	// Budget the whole snapshot to max_tokens (~4 chars/token). The judge reads
	// this fresh each look, so an unbounded snapshot is the per-look cost.
	budget := cfg.Daemon.Overseer.MaxTokens
	if budget <= 0 {
		budget = 4000
	}
	remaining := budget * 4
	var out []SessionState
	for _, s := range tmux.ListSessions() {
		if remaining <= 0 {
			break
		}
		dir := s.Path
		tool := daemon.ToolName(reg.Tool(filepath.Base(dir)))
		tail := transcriptTail(cfg, tool, dir)
		if tail == "" {
			continue
		}
		if len(tail) > remaining {
			tail = "…" + tail[len(tail)-remaining:]
		}
		remaining -= len(tail)
		out = append(out, SessionState{Name: s.Name, Tool: tool, Tail: tail})
	}
	return out
}

// transcriptTail returns the last tailTurns of a session's transcript as
// role-prefixed lines, or "" when there is no readable transcript.
func transcriptTail(cfg config.Config, tool, dir string) string {
	var tr *handoff.Transcript
	var err error
	switch tool {
	case config.DefaultTool:
		path := daemon.RecentSessionFile(cfg.Claude.Home, dir)
		if path == "" {
			return ""
		}
		tr, err = handoff.ReadClaude(path, dir)
	case "codex":
		path := handoff.RecentCodexRollout(dir)
		if path == "" {
			return ""
		}
		tr, err = handoff.ReadCodex(path, dir)
	default:
		return ""
	}
	if err != nil || tr == nil || len(tr.Turns) == 0 {
		return ""
	}
	turns := tr.Turns
	if len(turns) > tailTurns {
		turns = turns[len(turns)-tailTurns:]
	}
	var b strings.Builder
	for _, t := range turns {
		text := t.Text
		if len(text) > tailTurnCap {
			text = text[:tailTurnCap] + "…"
		}
		role := t.Role
		if t.Name != "" {
			role += ":" + t.Name
		}
		fmt.Fprintf(&b, "[%s] %s\n", role, text)
	}
	return b.String()
}

// overseerSystemPrompt is the overseer's role, passed as the session's system
// prompt (--system-prompt) so it defines the overseer role at the system level rather
// than competing with Claude Code's coding-agent framing in a user turn.
const overseerSystemPrompt = `You are the overseer of a fleet of autonomous coding-agent sessions. You do not write code or use tools. Your only job: read each session's recent transcript and judge whether it reached its goal or stopped short.

Each user message is a JSON array of sessions, each with a name, tool, and a recent transcript tail. For each session:
- infer its goal from the tail,
- judge its state: "working" (actively mid-task), "done" (goal met), "stopped_short" (idle with the goal unmet and a path still open), or "blocked" (needs something it cannot get itself),
- if stopped_short, write callout: one imperative sentence telling the agent to continue toward the goal,
- set needs_user true ONLY when a real decision is required that the agent cannot make on its own.

Reply with ONLY a JSON array, one object per session, no prose, no markdown fences:
[{"name":"...","goal":"...","state":"working|done|stopped_short|blocked","callout":"...","needs_user":false,"user_reason":""}]`

// Look builds the snapshot, runs one overseer call, and returns the verdicts and
// usage. It takes no action on the sessions; the daemon acts on the returned
// verdicts. sessions may be passed pre-built (the daemon filters to newly-idle
// ones); when nil, Look builds the full fleet snapshot itself.
func Look(cfg config.Config, sessions []SessionState) (LookResult, error) {
	if sessions == nil {
		sessions = BuildSnapshot(cfg)
	}
	res := LookResult{Sessions: sessions}
	if len(sessions) == 0 {
		return res, nil
	}

	payload, err := json.Marshal(sessions)
	if err != nil {
		return res, err
	}
	prompt := string(payload)

	dir := ScratchDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return res, fmt.Errorf("overseer scratch dir: %w", err)
	}

	model := cfg.Daemon.Overseer.Model
	if model == "" {
		model = "sonnet"
	}
	// Resume the standing session so Claude Code's system prompt and the global
	// CLAUDE.md stay in the warm prompt-cache prefix (read at 0.1x); only the new
	// turn is written. A fresh session each look would re-establish, and re-write,
	// that whole prefix. resume falls back to fresh on any failure (the session
	// was cleared, or Claude Code upgraded and its prefix changed).
	sid := readSessionID()
	out, err := runOverseer(dir, model, sid, prompt)
	if err != nil && sid != "" {
		out, err = runOverseer(dir, model, "", prompt)
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

type overseerOutput struct {
	Result    string `json:"result"`
	SessionID string `json:"session_id"`
	Usage     struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

func runOverseer(dir, model, resumeID, prompt string) (overseerOutput, error) {
	args := []string{"-p", "--model", model, "--output-format", "json", "--dangerously-skip-permissions"}
	if resumeID != "" {
		// The session carries its system prompt and settings from creation; resume
		// just continues it.
		args = append(args, "--resume", resumeID)
	} else {
		// Fresh session: define the overseer as a pure judge via its own system
		// prompt, and drop user settings so the global CLAUDE.md (coding persona,
		// caveman mode) and hooks don't load into it or bloat the cached prefix.
		args = append(args,
			"--system-prompt", overseerSystemPrompt,
			"--setting-sources", "project")
	}
	args = append(args, prompt)
	cmd := exec.Command("claude", args...)
	cmd.Dir = dir
	raw, err := cmd.Output()
	if err != nil {
		return overseerOutput{}, fmt.Errorf("overseer call: %w", err)
	}
	var out overseerOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		return overseerOutput{}, fmt.Errorf("parse overseer output: %w", err)
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

// parseVerdicts extracts the JSON array from the overseer's reply, tolerating a
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
	return v
}

// UsageLogPath is where each look's usage is appended as JSONL, so cache warmth
// (cache_read vs cache_create) can be analysed over real use.
func UsageLogPath() string {
	return filepath.Join(filepath.Dir(ScratchDir()), "overseer-usage.jsonl")
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
