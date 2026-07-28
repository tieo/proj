package daemon

import (
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/tieo/proj/internal/projects"
	"github.com/tieo/proj/internal/tmux"
)

// The Stop hook keeps a doner session going while it is answering, but it runs
// at the moment of a stop and has to decide then. A session that stops anyway,
// because the hook released it or because it was started before the hook was
// installed, is parked: nothing in Claude Code will wake it.
//
// This is the backstop for that. A doner-tagged session that has been quiet for
// longer than the grace, with no unsent draft and nothing running, is nudged
// with the same text the hook uses. The grace is what makes it safe to be
// wrong: a message from the user or a job finishing inside that window writes
// to the transcript, which restarts the clock, so the nudge only ever lands on
// a session that really has gone still.

// DonerTag opts a project into the doner backstop. It is the same tag
// cmd/proj writes, named here so the daemon does not import the command.
const DonerTag = "doner"

// DonerReason is the nudge, shared so the Stop hook and this backstop say the
// same thing: a session cannot tell which one reached it, and should not have
// to.
const DonerReason = "done with everything you were granted to do? if not, continue. " +
	"Waiting on something? wait for it actively: a background command that ends when it does, " +
	"which wakes you. Stopping is not waiting. " +
	"If you must stop: finished, blocked, or it needs the user " +
	"(being done with a big chunk, arriving at a 'good place to end', " +
	"or being at the end of your context do NOT count!), " +
	"say why in one line, then reply exactly: Yes"

// donerNudgedAt is the last time each session was nudged, so a session that
// stays quiet is not nudged every tick. In memory only: a daemon restart
// costing one extra nudge is not worth a state file.
var donerNudgedAt = map[string]time.Time{}

// donerTick nudges one idle doner-tagged session that has gone quiet past the
// grace. content is the pane capture, sessFile its transcript.
func donerTick(cfg Config, reg projects.Registry, p tmux.Pane, dir, content, sessFile string, now time.Time) {
	if !cfg.Doner.Active() || sessFile == "" {
		return
	}
	if !hasTag(reg.Tags(filepath.Base(dir)), DonerTag) {
		return
	}
	// Still generating: not a session that has gone still.
	if connDropBusyRE.MatchString(content) {
		return
	}
	// No input box means a view has taken the pane over: the shell-details
	// overlay, a picker. Nothing the session does clears that - it is waiting on
	// a keystroke nobody is there to send - so it would sit there for good.
	// Escape is what those views offer ("Esc to close"), and it is safe here:
	// the busy check above has ruled out interrupting a live turn, and the trust
	// prompt, the one place Escape exits Claude Code, is handled earlier in the
	// tick and never reaches this.
	if !inputPromptRE.MatchString(content) {
		slog.Info("doner: closing an overlay to reach the input box", "session", p.Session)
		if err := tmux.SendKey(p.ID, "Escape"); err != nil {
			return
		}
		time.Sleep(cfg.DismissGap)
		content = tmux.CapturePane(p.ID, cfg.Capture)
		if !inputPromptRE.MatchString(content) {
			return // still no input box; leave the pane alone
		}
	}
	// A draft is the user mid-sentence. Typing now would both overwrite it and
	// nudge someone who is already here.
	if composerHasDraft(tmux.CapturePaneEsc(p.ID)) {
		return
	}
	grace := cfg.Doner.GraceDuration()
	// The transcript's last write is when the session last did or was told
	// anything, so a reply from the user or a finishing job restarts the clock.
	if now.Sub(transcriptMTime(sessFile)) < grace {
		return
	}
	if last, ok := donerNudgedAt[p.Session]; ok && now.Sub(last) < grace {
		return
	}
	if err := SendPrompt(cfg, p.ID, DonerReason); err != nil {
		slog.Error("doner nudge failed", "session", p.Session, "err", err)
		return
	}
	donerNudgedAt[p.Session] = now
	slog.Info("doner nudged an idle session", "session", p.Session,
		"quiet_for", now.Sub(transcriptMTime(sessFile)).Round(time.Second))
}

// transcriptMTime is when the session's transcript was last written, which is
// when it last did or was told anything. Zero when it cannot be read, which
// reads as "long ago" and is caught by the caller's own checks.
func transcriptMTime(path string) time.Time {
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

func hasTag(tags []string, want string) bool {
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}

// pruneDonerNudges drops entries for sessions that are gone, so the map does
// not grow across a long-running daemon.
func pruneDonerNudges(live map[string]bool) {
	for name := range donerNudgedAt {
		if !live[name] {
			delete(donerNudgedAt, name)
		}
	}
}
