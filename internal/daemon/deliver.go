package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/tieo/proj/internal/projstate"
	"github.com/tieo/proj/internal/tmux"
)

// Delivering a prompt into a Claude Code session means typing it into a TUI,
// and a TUI drops input it cannot keep up with. Measured against live sessions:
// a single send-keys burst arrives whole up to ~800 characters, collapses into
// a "[Pasted text #1]" placeholder around 1000, and silently truncates above
// that; tmux paste-buffer is worse, delivering only the tail (35 of 2083
// characters in one run). Bursts of 500 characters 50ms apart (see
// tmux.SendLiteral) arrive whole in 39 of 40 runs at 2000 characters - good,
// not certain. So delivery is not trusted: the composer is read back before the
// prompt is submitted, and a mismatch is retried. Text too long to render on
// screen cannot be read back at all, so it travels as a file instead.

// composerSettle is how long the TUI gets to render what was typed before the
// composer is read back.
const composerSettle = 900 * time.Millisecond

// composerChrome is how much of the pane the conversation, the rules and the
// status lines take, leaving the rest for the input box. Deliberately
// pessimistic: overestimating the box means trusting a read-back that was
// actually cut off by the screen edge.
const composerChrome = 6

var composerPrompt = regexp.MustCompile(`^(?:❯|>\x{00a0})`)

// ComposerBox returns the text standing in a session's input box, whether it
// is a paste placeholder rather than readable text, and whether there is an
// input box on screen at all. The box is the region below the last prompt
// marker up to the rule that closes it; continuation lines are indented by two
// spaces, which is undone here so the text compares against what was typed.
//
// present is false when the session shows no input box: it is starting up, or
// standing on the trust-folder question or a picker. Typing there does not
// compose a message, it works the menu.
func ComposerBox(content string) (text string, placeholder, present bool) {
	lines := strings.Split(content, "\n")
	start := -1
	for i, ln := range lines {
		if composerPrompt.MatchString(ln) {
			start = i
		}
	}
	if start < 0 {
		return "", false, false
	}
	body := []string{composerPrompt.ReplaceAllString(lines[start], "")}
	for _, ln := range lines[start+1:] {
		if strings.Contains(ln, "─") {
			break
		}
		body = append(body, strings.TrimPrefix(ln, "  "))
	}
	text = strings.TrimSpace(strings.Join(body, "\n"))
	return text, strings.Contains(text, "Pasted text #"), true
}

// verifiableLen is how much text can be typed into target and still be read
// back off the screen. Zero when the pane cannot be measured, which turns every
// send into a file handover rather than an unverifiable fill.
func verifiableLen(target string) int {
	w, h := tmux.PaneSize(target)
	rows := h/2 - composerChrome // the box grows to about half the pane
	if w <= 0 || rows <= 0 {
		return 0
	}
	return w * rows
}

// composerEndsWith reports whether the box ends with what was just typed. It is
// a suffix check rather than an equality one because the prompt is appended to
// whatever already stood in the box. Whitespace is ignored: the TUI wraps long
// input across lines and pads it, so the rendered text differs from the input
// in whitespace alone.
func composerEndsWith(box, typed string) bool {
	strip := func(s string) string {
		return strings.Map(func(r rune) rune {
			if r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == ' ' {
				return -1
			}
			return r
		}, s)
	}
	return strings.HasSuffix(strip(box), strip(typed))
}

// paneIO is the pane side of a verified fill, injected so the check can be
// tested without a tmux server.
type paneIO struct {
	send func(target, text string) error
	read func(target string) (text string, placeholder, present bool)
}

var livePane = paneIO{
	send: tmux.SendLiteral,
	read: func(target string) (string, bool, bool) {
		time.Sleep(composerSettle)
		return ComposerBox(tmux.CapturePane(target, 0))
	},
}

// typeVerified types text into target and reports whether the input box ends
// with it. Whatever stood in the box before is left alone and goes out with the
// prompt: the target sorts that out, and touching it has cost more than it ever
// saved. A mangled fill is not retyped either - a second attempt would append a
// second copy - so the caller falls back to handing over a file.
func typeVerified(target, text string) (bool, error) {
	return typeVerifiedVia(livePane, target, text)
}

func typeVerifiedVia(io paneIO, target, text string) (bool, error) {
	if err := io.send(target, text); err != nil {
		return false, err
	}
	box, _, present := io.read(target)
	// Whether the box holds a paste marker somewhere is not the question: the
	// question is whether it ends with what was just typed. A marker left in the
	// box by an earlier message used to veto every send into that session for
	// good, and the text piled up unsent behind it. When the marker swallowed
	// this text, the suffix check fails on its own and the file handover
	// follows, which is what it is for.
	return present && composerEndsWith(box, text), nil
}

// SendPrompt delivers text into a session's input box and submits it as a turn
// of its own. Anything already standing in the box is left where it is and goes
// out in front of the prompt: reading it, clearing it and putting it back cost
// two sends that never arrived, and the target can make sense of a stray line
// far more easily than of a message that was never delivered.
//
// Text that fits on screen is typed and verified. Anything longer, or anything
// that will not type correctly, is written to a file and announced with a short
// prompt that names it: a path is short enough to deliver reliably, and the
// target reads the message from disk rather than through the keyboard.
// backgroundHint is the TUI's own offer, shown while a foreground command holds
// the input box: press it and the command keeps running out of the way.
var backgroundHint = regexp.MustCompile(`ctrl\+b[^\n]*?to run in background`)

// busyHint marks a turn in flight: the interrupt hint, or the spinner's running
// timer ("Perusing… (5s · thinking)"). The hint alone is not enough, since the
// TUI shows it only some of the time, and the "… (" signature belongs to a
// spinner that is still counting - a finished status ("Cogitated for 2m") has
// no ellipsis. Nothing is interrupted unless one of them is on screen.
var busyHint = regexp.MustCompile(`(?i)esc to interrupt|…[ \t]*\(`)

// interrupt stops a turn so the message that follows is read now. It is the
// expensive move: the turn's work is dropped where it stands, so it is only
// made when the session says it is busy and the gentler offer does not apply.
func interrupt(target string) bool {
	screen := tmux.CapturePane(target, 0)
	if backgroundHint.MatchString(screen) {
		// A foreground command has an offer of its own that costs nothing.
		return false
	}
	if !busyHint.MatchString(screen) {
		return false
	}
	if err := tmux.SendKey(target, "Escape"); err != nil {
		return false
	}
	// An interrupted turn puts its own line on screen ("Interrupted · What
	// should Claude do instead?") and repaints around it. Typing into that
	// repaint is what a fixed pause gets wrong, so the box is waited for.
	return waitForComposer(target, 5*time.Second)
}

// waitForComposer waits until the input box is on screen and has stopped being
// repainted, and reports whether it got there.
func waitForComposer(target string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if _, _, present := ComposerBox(tmux.CapturePane(target, 0)); present {
			time.Sleep(composerSettle)
			_, _, present = ComposerBox(tmux.CapturePane(target, 0))
			return present
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// toBackground takes the offer. Interrupting would throw away work the session
// is in the middle of; waiting would drop the message. Backgrounding is the one
// move that costs nothing, and the TUI only offers it when it applies.
func toBackground(target string) bool {
	if !backgroundHint.MatchString(tmux.CapturePane(target, 0)) {
		return false
	}
	for i := 0; i < 2; i++ {
		if err := tmux.SendKey(target, "C-b"); err != nil {
			return false
		}
		time.Sleep(120 * time.Millisecond)
	}
	time.Sleep(composerSettle)
	return true
}

func SendPrompt(cfg Config, target, text string) error {
	return sendPrompt(cfg, target, text, false)
}

// SendPromptNow delivers text into a session that is busy with something the
// TUI does not offer to put aside: a turn being thought out, a tool call, a
// stream still arriving. It stops that first, so the message is read now rather
// than whenever the turn ends. What the session was doing is lost, which is the
// difference between this and SendPrompt, and why it is asked for by name.
func SendPromptNow(cfg Config, target, text string) error {
	return sendPrompt(cfg, target, text, true)
}

func sendPrompt(cfg Config, target, text string, now bool) error {
	if strings.TrimSpace(text) == "" {
		// An empty send used to clear the target's box, submit nothing, and -
		// once the box would not verify - hand over a file holding a single
		// newline. Whatever produced the empty text is the caller's bug, and it
		// must not reach the target as a task.
		return fmt.Errorf("refusing to send an empty prompt to %s", target)
	}
	if now {
		// Asked for by name: whatever the session is doing yields, including
		// the work no offer covers.
		interrupt(target)
	}
	if _, _, present := ComposerBox(tmux.CapturePane(target, 0)); !present {
		// A command running in the foreground hides the box behind it. The TUI
		// offers to put it in the background; taking that offer is how the
		// message gets in without stopping what is already running.
		toBackground(target)
	}
	if _, _, present := ComposerBox(tmux.CapturePane(target, 0)); !present {
		// No input box: the session is starting up, or waiting on the
		// trust-folder question or a picker. Keystrokes there work the menu
		// instead of composing a message, so a send would pick an option nobody
		// chose and still deliver nothing.
		return fmt.Errorf("%s is not at an input box (starting up, or on a prompt or picker); nothing sent", target)
	}
	clearBox(target)
	if err := deliver(target, text); err != nil {
		return err
	}
	time.Sleep(cfg.DismissGap)
	return tmux.SendKey(target, "Enter")
}

// clearBox empties the input box before a prompt goes into it.
//
// Leaving what stood there was the older rule, and it cost more than it saved:
// a paste marker or an abandoned half-sentence stayed in the box, the typed
// prompt was appended behind it, and every send after that went out carrying
// somebody else's words. What is in the box is a draft nobody submitted; the
// message being delivered is one somebody did.
func clearBox(target string) {
	for i := 0; i < 40; i++ {
		text, _, present := ComposerBox(tmux.CapturePane(target, 0))
		if !present || strings.TrimSpace(text) == "" {
			return
		}
		if err := tmux.SendKey(target, "C-u"); err != nil {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func deliver(target, text string) error {
	if len(text) <= verifiableLen(target) {
		ok, err := typeVerified(target, text)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
	}
	path, err := writeHandoff(target, text)
	if err != nil {
		return fmt.Errorf("hand the prompt over as a file: %w", err)
	}
	pointer := fmt.Sprintf("Read %s and act on it: it is a message from the proj daemon, too long to type into your input box.", path)
	ok, err := typeVerified(target, pointer)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("could not type even the handover line into %s (prompt is at %s)", target, path)
	}
	return nil
}

// writeHandoff stores a prompt too long to type and returns its path. The name
// carries the session so a stale file says who it was meant for.
func writeHandoff(target, text string) (string, error) {
	dir := projstate.Dir("sends")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	safe := strings.Map(func(r rune) rune {
		if r == '/' || r == ' ' || r == '@' {
			return '-'
		}
		return r
	}, target)
	path := filepath.Join(dir, fmt.Sprintf("%s-%d.md", safe, time.Now().Unix()))
	if err := os.WriteFile(path, []byte(text+"\n"), 0o644); err != nil {
		return "", err
	}
	return path, nil
}
