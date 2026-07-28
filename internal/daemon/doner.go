package daemon

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
const DonerReason = "done with everything you were granted to do, hard blocked by something, " +
	"or it needs the user (being done with a big chunk, arriving at a 'good place to end', " +
	"or being at the end of your context do NOT count!), reply exactly: Yes. " +
	"Waiting on something? wait for it actively: a background command that ends when it does, " +
	"which wakes you. ANYTHING else? continue."

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
	// No input box means a view has taken the pane over: the shell-details
	// overlay, the background-shells list, a picker. Nothing the session does
	// clears that - the view is waiting on a keystroke nobody is there to send -
	// so it would sit there for good. Escape is what those views offer ("Esc to
	// close").
	//
	// This runs BEFORE the busy check, not after, for two reasons. A generating
	// session keeps its input box, so a pane without one is not mid-turn and
	// Escape cannot interrupt a turn here. And the busy check reads the whole
	// capture, where the shells list defeats it: it lists commands truncated
	// with an ellipsis and marked "(running)", which is exactly the spinner
	// shape it looks for, so an overlaid session looked busy forever and was
	// never reached. The trust prompt, the one place Escape ends Claude Code, is
	// handled earlier in the tick and never arrives here.
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
	// Still generating: not a session that has gone still.
	if connDropBusyRE.MatchString(content) {
		return
	}
	// A draft is the user mid-sentence. Typing now would both overwrite it and
	// nudge someone who is already here.
	if composerHasDraft(tmux.CapturePaneEsc(p.ID)) {
		return
	}
	// Already reported done. The Stop hook lets such a session go, and the
	// backstop has to agree: without this it re-nudged a session that had
	// answered, every grace period, for as long as it sat there.
	if IsDone(lastAssistantText(sessFile)) {
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

// doneReplies is the one word the nudge asks for. It was a wider set of
// affirmatives, meant to be forgiving, and every extra entry was a way to end a
// sentence about a subtask: "done", "finished", "complete". Since the nudge
// asks for this word exactly, a session that follows it always says this one,
// and the extra entries could only ever fire on a session that did not - so
// they existed purely to misread.
var doneReplies = map[string]bool{"yes": true}

// IsDone reports whether a reply reads as "finished". The nudge asks for a
// reason line and then the word, so the LAST line answers it; a whole-message
// match would reject every reply that obeyed the instruction. A "Yes" earlier
// in the message ends nothing.
func IsDone(text string) bool {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := letterWords(lines[i]); line != "" {
			return doneReplies[line]
		}
	}
	return false
}

// letterWords reduces a line to lowercase letters and single spaces, so
// punctuation and markdown around the word do not hide it.
func letterWords(s string) string {
	var b strings.Builder
	prevSpace := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
			prevSpace = false
		case r == ' ' || r == '\t' || r == '\r':
			if !prevSpace && b.Len() > 0 {
				b.WriteByte(' ')
			}
			prevSpace = true
		}
	}
	return strings.TrimSpace(b.String())
}

// lastAssistantText returns the text of the session's most recent assistant
// turn, which is what the Stop hook judges as last_assistant_message. Only the
// tail is read: the answer is at the end, and these transcripts run to tens of
// megabytes.
func lastAssistantText(sessFile string) string {
	f, err := os.Open(sessFile)
	if err != nil {
		return ""
	}
	defer f.Close()
	const readBytes = 200 * 1024
	// Measuring the file leaves the offset at its end, so the read has to be
	// positioned again even when the whole file fits: without that a short
	// transcript read nothing and no session ever looked done.
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
	for i := len(lines) - 1; i >= 0; i-- {
		var r struct {
			Type    string `json:"type"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(lines[i])), &r) != nil || r.Type != "assistant" {
			continue
		}
		if text := assistantText(r.Message.Content); text != "" {
			return text
		}
	}
	return ""
}

// assistantText flattens an assistant message's content to its text blocks.
func assistantText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var b strings.Builder
	for _, bl := range blocks {
		if bl.Type == "text" && strings.TrimSpace(bl.Text) != "" {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(bl.Text)
		}
	}
	return strings.TrimSpace(b.String())
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
