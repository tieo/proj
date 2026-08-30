package daemon

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
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
	"Waiting on a job you started and cannot hurry? reply exactly: Waiting on <id>, " +
	"naming the agent, task or command you are waiting for; you will be asked again in " +
	"half an hour and nothing will disturb you before then. ANYTHING else? continue."

// donerNudgedAt is the last time each session was nudged, so a session that
// stays quiet is not nudged every tick. In memory only: a daemon restart
// costing one extra nudge is not worth a state file.
var donerNudgedAt = map[string]time.Time{}

// donerBounces counts, per session, the nudges in a row that produced nothing
// but an instant reply. In memory only, like donerNudgedAt: a daemon restart
// forgiving a stuck session one more try is cheaper than a state file.
var donerBounces = map[string]int{}

// A nudge is meant to restart work, so a session that answers it in seconds and
// falls straight back to silence did not take it: it bounced. The usual cause
// is a session that cannot answer at all - out of quota, a dead token, a model
// error - and the nudge then repeats for as long as the cause lasts. One such
// loop ran 260 times against a single session over a weekly limit.
//
// Detecting the causes one by one does not scale: each needs its own phrase,
// and the phrase is only learned after the loop has already run. Counting
// bounces needs to know none of them. Three is the allowance: enough that a
// single odd reply does not stand a session down, few enough that a loop dies
// within three grace periods rather than hundreds. A session that answers
// properly, or takes its time, clears the count.
const (
	bounceReplyWindow = 90 * time.Second
	maxBounces        = 3
)

// bounced reports whether the last nudge produced an instant reply and nothing
// else. lastWrite is the transcript's final write, which is the session's own
// answer to the nudge; a session that went off and worked writes for far longer
// than the window.
func bounced(nudgedAt, lastWrite time.Time) bool {
	if nudgedAt.IsZero() || lastWrite.Before(nudgedAt) {
		return false
	}
	return lastWrite.Sub(nudgedAt) < bounceReplyWindow
}

// donerTick nudges one idle doner-tagged session that has gone quiet past the
// grace. content is the pane capture, sessFile its transcript.
func donerTick(cfg Config, reg projects.Registry, p tmux.Pane, dir, content, sessFile string, banner *Banner, now time.Time) {
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
	// A session that has run out of quota cannot answer at all: the nudge lands,
	// the model refuses with the limit banner, and the whole exchange repeats
	// every grace period until the reset. Nudging it is not a backstop, it is a
	// loop, so the limit is left to the resume path that watches for the reset.
	if banner != nil {
		return
	}
	last := lastAssistantText(sessFile)
	// Already reported done. The Stop hook lets such a session go, and the
	// backstop has to agree: without this it re-nudged a session that had
	// answered, every grace period, for as long as it sat there.
	if IsDone(last) {
		return
	}
	// Waiting on a job it started. The nudge asks for that answer by name, and
	// asking again straight away is what the answer exists to prevent: the
	// session cannot make the job finish sooner, so it is left alone for the
	// wait window and asked again only once that has passed, in case whatever
	// it named never came back.
	if IsWaiting(last) && now.Sub(transcriptMTime(sessFile)) < WaitWindow {
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
	// Whatever the last nudge achieved is visible now: either the session went
	// away and worked, or it answered in seconds and stopped again. Only the
	// second kind counts against it, and any other outcome clears the tally.
	if bounced(donerNudgedAt[p.Session], transcriptMTime(sessFile)) {
		donerBounces[p.Session]++
	} else {
		delete(donerBounces, p.Session)
	}
	if donerBounces[p.Session] >= maxBounces {
		if donerBounces[p.Session] == maxBounces {
			slog.Warn("doner: standing down, the nudge is bouncing",
				"session", p.Session, "bounces", donerBounces[p.Session],
				"last_reply", strings.TrimSpace(firstLine(last)))
			donerBounces[p.Session]++ // log once, then stay quiet
		}
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

// WaitWindow is how long a session that reported itself waiting is left alone.
// It is the one number the answer buys: long enough that a job worth waiting
// for has a chance to finish, short enough that a job which never returns does
// not park the session for the rest of the day.
const WaitWindow = 30 * time.Minute

// waitingRE matches the answer the nudge asks for when a session is waiting on
// something it started ("Waiting on agent-7", "waiting on the deploy job").
// What is named is not checked: the daemon cannot verify someone else's id, and
// the claim is only ever worth half an hour of quiet.
var waitingRE = regexp.MustCompile(`(?i)^waiting on\s+\S+`)

// IsWaiting reports whether a reply says the session is waiting on a job it
// started. Like IsDone it reads the last line, so a reason line above the
// answer does not hide it.
func IsWaiting(text string) bool {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return waitingRE.MatchString(line)
		}
	}
	return false
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

// firstLine is the head of a reply, for a log line that has to stay one line.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}
