package overseer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/tieo/proj/internal/config"
	"github.com/tieo/proj/internal/daemon"
	"github.com/tieo/proj/internal/projects"
	"github.com/tieo/proj/internal/tmux"
)

// sessLook is the per-session memory carried between looks: the transcript
// signature at the last look (to detect new work), how many times the session
// has been nudged without progressing, and whether the user has already been
// notified about its current stall.
type sessLook struct {
	Sig      string `json:"sig"`
	Nudges   int    `json:"nudges"`
	Notified bool   `json:"notified"`
}

// lookState is the overseer pass's durable memory, kept in the scratch dir so a
// daemon restart does not re-nudge or re-notify sessions it already handled.
type lookState struct {
	LastLook time.Time           `json:"last_look"`
	Sessions map[string]sessLook `json:"sessions"`
}

func lookStatePath() string { return filepath.Join(ScratchDir(), "lookstate.json") }

func loadLookState() lookState {
	st := lookState{Sessions: map[string]sessLook{}}
	b, err := os.ReadFile(lookStatePath())
	if err != nil {
		return st
	}
	_ = json.Unmarshal(b, &st)
	if st.Sessions == nil {
		st.Sessions = map[string]sessLook{}
	}
	return st
}

func saveLookState(st lookState) {
	if err := os.MkdirAll(ScratchDir(), 0o755); err != nil {
		return
	}
	b, err := json.Marshal(st)
	if err != nil {
		return
	}
	_ = os.WriteFile(lookStatePath(), b, 0o644)
}

// transcriptSig is a cheap fingerprint of a transcript file: size and
// modification time. It changes whenever the session writes a new turn, so a
// look can skip any session whose transcript is byte-identical to the last look.
func transcriptSig(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d:%d", info.Size(), info.ModTime().UnixNano())
}

// nudgeGap is the pause between typing a callout and submitting it, matching the
// daemon's DismissGap: codex reads a same-burst Enter as a pasted newline, so
// the submit must arrive as its own key after the pane settles.
const nudgeGap = 300 * time.Millisecond

// Pass is the daemon's overseer pass, invoked once per poll via daemon.PostTick.
// It is event-driven and self-throttling: it does nothing until Interval has
// elapsed since the last look, then looks only at sessions that did new work and
// are idle at a prompt (not rate-limited, errored, or in a picker). It judges
// them in one overseer call and acts on the verdicts - nudging the ones that
// stopped short, notifying on a genuine user decision - taking no action while
// disabled.
func Pass(cfg config.Config, now time.Time) {
	ov := cfg.Daemon.Overseer
	if !ov.Enabled {
		return
	}
	interval := config.Duration(ov.Interval, 15*time.Minute)
	st := loadLookState()
	if !st.LastLook.IsZero() && now.Sub(st.LastLook) < interval {
		return
	}

	// Classify panes so terminal sessions (a usage-limit banner, a stuck API
	// error, an open picker) are skipped: they are not idle-with-a-path, and the
	// banner/error ones are the resume daemon's job, not the overseer's.
	panes := daemon.ScanPanes(cfg.Claude.Home, cfg.Daemon.CaptureLines)
	byName := make(map[string]daemon.PaneState, len(panes))
	for _, p := range panes {
		byName[p.Pane.Session] = p
	}

	reg, _ := projects.LoadRegistry()
	budget := ov.MaxTokens
	if budget <= 0 {
		budget = 4000
	}
	remaining := budget * 4

	var cands []SessionState
	sigNow := map[string]string{}
	for _, s := range tmux.ListSessions() {
		if remaining <= 0 {
			break
		}
		ps, ok := byName[s.Name]
		if ok && (ps.Banner != nil || ps.APIError != nil || ps.Selector) {
			continue // terminal or auto-resumed elsewhere; not the overseer's call
		}
		tool := daemon.ToolName(reg.Tool(filepath.Base(s.Path)))
		path := transcriptPath(cfg, tool, s.Path)
		if path == "" {
			continue
		}
		sig := transcriptSig(path)
		sigNow[s.Name] = sig
		if sig != "" && sig == st.Sessions[s.Name].Sig {
			continue // no new work since the last look
		}
		tail := transcriptTail(cfg, tool, s.Path)
		if tail == "" {
			continue
		}
		if len(tail) > remaining {
			tail = "…" + tail[len(tail)-remaining:]
		}
		remaining -= len(tail)
		cands = append(cands, SessionState{Name: s.Name, Tool: tool, Tail: tail})
	}

	if len(cands) == 0 {
		return // nothing new; leave LastLook so the next tick re-checks cheaply
	}

	res, err := Look(cfg, cands)
	if err != nil {
		slog.Error("overseer look failed", "err", err)
		return
	}
	if err := LogUsage(now, res); err != nil {
		slog.Warn("overseer usage log failed", "err", err)
	}

	act(cfg, res, byName, &st)

	for name, sig := range sigNow {
		sl := st.Sessions[name]
		sl.Sig = sig
		st.Sessions[name] = sl
	}
	pruneLookState(&st, byName)
	st.LastLook = now
	saveLookState(st)
}

// action is what a verdict calls for.
type action int

const (
	actNone   action = iota // let it be (working, done, or already handled)
	actNudge                // type the callout into the session to continue it
	actNotify               // push the user: a decision the agent cannot make
)

// decide maps a verdict and the session's current memory to the action to take
// and the memory to carry forward, without any side effect. It is the whole
// policy: nudge a session that stopped short with a path still open, up to
// MaxNudges times; notify (not nudge) when a real user decision is needed; and
// clear the stall memory once the session is working again or done. The Nudges
// count is advanced by act only on a nudge that actually sent (the composer
// guard can veto), so decide leaves it untouched for actNudge.
func decide(v Verdict, sl sessLook, maxNudges int) (action, sessLook) {
	switch v.State {
	case "stopped_short":
		if v.NeedsUser {
			return actNotify, sl
		}
		if v.Callout == "" || sl.Nudges >= maxNudges {
			return actNone, sl // nothing to say, or already nudged to the limit
		}
		return actNudge, sl
	case "blocked":
		if v.NeedsUser {
			return actNotify, sl
		}
		return actNone, sl
	default: // working, done: progress or terminal - clear the stall memory
		sl.Nudges = 0
		sl.Notified = false
		return actNone, sl
	}
}

// act carries out each verdict's decision against the live panes: nudging,
// notifying, or nothing, and persists the updated per-session memory.
func act(cfg config.Config, res LookResult, byName map[string]daemon.PaneState, st *lookState) {
	ov := cfg.Daemon.Overseer
	for _, v := range res.Verdicts {
		a, sl := decide(v, st.Sessions[v.Name], ov.MaxNudges)
		switch a {
		case actNudge:
			if ps, ok := byName[v.Name]; ok && nudge(ps.Pane.ID, v.Callout) {
				sl.Nudges++
				slog.Info("overseer nudged", "session", v.Name, "nudges", sl.Nudges, "callout", v.Callout)
			}
		case actNotify:
			notify(ov.NtfyTopic, v, &sl)
		}
		st.Sessions[v.Name] = sl
	}
}

// nudge types callout into a pane and submits it, unless the composer already
// holds an unsent user draft (typing then would corrupt it). Reports whether it
// sent.
func nudge(paneID, callout string) bool {
	if daemon.ComposerHasDraft(tmux.CapturePaneEsc(paneID)) {
		return false
	}
	if err := tmux.SendLiteral(paneID, callout); err != nil {
		slog.Error("overseer nudge send failed", "pane", paneID, "err", err)
		return false
	}
	time.Sleep(nudgeGap)
	if err := tmux.SendKey(paneID, "Enter"); err != nil {
		slog.Error("overseer nudge submit failed", "pane", paneID, "err", err)
		return false
	}
	return true
}

// notify pushes one ntfy message for a session that needs a user decision, once
// per stall (Notified guards the repeat). No topic configured means no push.
func notify(topic string, v Verdict, sl *sessLook) {
	if topic == "" || sl.Notified {
		return
	}
	reason := v.UserReason
	if reason == "" {
		reason = v.Callout
	}
	req, err := http.NewRequest(http.MethodPost, "https://ntfy.sh/"+topic, bytes.NewBufferString(reason))
	if err != nil {
		return
	}
	req.Header.Set("Title", "proj overseer: "+v.Name)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		slog.Error("overseer notify failed", "session", v.Name, "err", err)
		return
	}
	resp.Body.Close()
	sl.Notified = true
	slog.Info("overseer notified user", "session", v.Name, "reason", reason)
}

// pruneLookState drops memory for sessions that no longer exist, so the state
// file cannot grow without bound as projects come and go.
func pruneLookState(st *lookState, live map[string]daemon.PaneState) {
	for name := range st.Sessions {
		if _, ok := live[name]; !ok {
			delete(st.Sessions, name)
		}
	}
}
