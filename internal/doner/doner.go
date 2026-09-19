// Package doner holds the rules that decide whether a session may stop.
//
// They are predicates over one string, the session's last reply, because that
// is all a Stop hook is given and all the decision needs. Keeping them apart
// from whatever calls them is what lets the hook stay a hook: no session list,
// no transcript reading, no process to run.
package doner

import (
	"regexp"
	"strings"
	"time"
)

// Tag opts a project into doner.
const Tag = "doner"

// Reason is the nudge: what a session is told when it tried to stop and was
// sent back to work.
const Reason = "done with everything you were granted to do, hard blocked by something, " +
	"or it needs the user (being done with a big chunk, arriving at a 'good place to end', " +
	"or being at the end of your context do NOT count!), reply exactly: Yes. " +
	"Waiting on a job you started and cannot hurry? reply exactly: Waiting on <id>, " +
	"naming the agent, task or command you are waiting for; you will be asked again in " +
	"half an hour and nothing will disturb you before then. ANYTHING else? continue."

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

// replyAPIErrorRE matches a turn that ended because the API refused it rather than
// because the model had something to say. Claude Code writes the failure as the
// whole assistant message, so the prefix is the message.
var replyAPIErrorRE = regexp.MustCompile(`(?i)^api error\b`)

// IsAPIError reports whether a turn ended in an API failure.
//
// Nudging one of these is a loop with no exit. The nudge makes the session take
// another turn, the turn resends the same conversation, and the API refuses it
// for the same reason: a poisoned conversation stays poisoned, so the next
// attempt fails exactly like the last. One such session spent an hour retrying
// an image the API had already rejected and could not have accepted on any
// later try.
//
// This is not the "it errored, so give up" rule it looks like. A tool that
// fails, a build that breaks, a test that goes red are all work the session can
// act on, and they are not this: this is the turn itself never reaching the
// model. There is nothing to continue toward.
func IsAPIError(text string) bool {
	return replyAPIErrorRE.MatchString(strings.TrimSpace(text))
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
