package main

import (
	"strings"
	"testing"
	"time"
)

// Pane tails captured from real sessions. The busy one still renders its input
// box while generating, which is exactly why a prompt typed then is swallowed:
// an input box on screen is not the same as a session ready to take a turn.
// The composer's non-breaking space is spelled as an escape rather than
// carried as an invisible character in the fixture.
const composerLine = ">\u00a0\n"

const (
	idlePane = "──────────────── 32 @lwenb4004 [issue,jira] ──\n" +
		composerLine +
		"────────────────────────────────────────────────\n" +
		"  ⏵⏵ bypass permissions on (shift+tab to cycle) · ← for agents\n"

	busyPane = "● Running 4 shell commands…\n" +
		"  ⎿  $ tmux capture-pane -p\n" +
		"· Ruminating… (3m 56s · ↓ 18.0k tokens · thought for 1s)\n" +
		composerLine +
		"  ⏵⏵ bypass permissions on (shift+tab to cycle) · ← for agents\n"

	startingPane = "Welcome to Claude Code\n\n  Do you trust the files in this folder?\n"
)

// paneSource replays captures frame by frame, holding on the last one, so a
// test can let a target go idle after a few polls.
func paneSource(frames ...string) func(string) (string, string) {
	i := 0
	return func(string) (string, string) {
		f := frames[len(frames)-1]
		if i < len(frames) {
			f = frames[i]
		}
		i++
		return f, f
	}
}

func TestAwaitSendableRefusesBusyTarget(t *testing.T) {
	err := awaitSendable("s", 0, paneSource(busyPane))
	if err == nil {
		t.Fatal("a mid-turn target was accepted; the prompt would be typed into a session that swallows it")
	}
	if !strings.Contains(err.Error(), "--wait") {
		t.Errorf("error %q does not point at the way to queue the send", err)
	}
}

func TestAwaitSendableAcceptsIdleTarget(t *testing.T) {
	if err := awaitSendable("s", 0, paneSource(idlePane)); err != nil {
		t.Errorf("idle target refused: %v", err)
	}
}

// No input box at all (startup, trust prompt, a picker) is not sendable either.
func TestAwaitSendableRefusesTargetWithoutInputBox(t *testing.T) {
	if err := awaitSendable("s", 0, paneSource(startingPane)); err == nil {
		t.Error("a target with no input box was accepted")
	}
}

func TestAwaitSendableWaitsForIdle(t *testing.T) {
	sendPoll = time.Millisecond
	t.Cleanup(func() { sendPoll = 5 * time.Second })

	if err := awaitSendable("s", time.Second, paneSource(busyPane, busyPane, idlePane)); err != nil {
		t.Errorf("waiting send gave up on a target that went idle: %v", err)
	}
}

func TestAwaitSendableGivesUpAfterWait(t *testing.T) {
	sendPoll = time.Millisecond
	t.Cleanup(func() { sendPoll = 5 * time.Second })

	err := awaitSendable("s", 10*time.Millisecond, paneSource(busyPane))
	if err == nil {
		t.Fatal("a target that never went idle was accepted")
	}
	if !strings.Contains(err.Error(), "still mid-turn") {
		t.Errorf("error %q does not say the wait ran out", err)
	}
}
