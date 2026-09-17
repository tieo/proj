package daemon

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/tieo/proj/internal/claudeauth"
	"github.com/tieo/proj/internal/projstate"
)

// An organization can forbid Remote Control for its members. Claude Code then
// refuses to bind, every session reads as dropped, and the watchdog keeps
// running /remote-control at panes that can never come back while the list
// paints them all as offline. Nothing in Claude Code's own files says the
// policy is on; the only statement of it is the refusal Claude prints, so proj
// learns it from a pane and remembers it.
//
// It is remembered per organization, because the policy belongs to the
// organization rather than the machine: logging in with another account (see
// `proj auth`) is enough to have Remote Control back, and the answer for the
// organization it was learned from stays right.
var rcPolicyRE = regexp.MustCompile(`(?i)remote control (?:is )?disabled by (?:your )?organization`)

// rcPolicyLogged keeps the daemon from repeating the same line every tick. In
// memory only: a restart costing one more log line is not worth a state field.
var rcPolicyLogged bool

// RCPolicyOff reports whether Claude has said Remote Control is off for the
// organization the login in use belongs to.
func RCPolicyOff(claudeHome string) bool {
	path, ok := rcPolicyPath(claudeHome)
	if !ok {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

// NoteRCPolicy records that Remote Control is off for the current organization
// when content carries Claude's refusal. It reports whether the refusal was
// there, so a caller can skip the rest of its Remote Control work either way.
func NoteRCPolicy(claudeHome, content string) bool {
	if !rcPolicyRE.MatchString(content) {
		return false
	}
	if path, ok := rcPolicyPath(claudeHome); ok {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err == nil {
			_ = os.WriteFile(path, nil, 0o600)
		}
	}
	return true
}

// rcPolicyPath names the marker file for the organization of the login in use.
// Without a readable login there is no organization to answer for.
func rcPolicyPath(claudeHome string) (string, bool) {
	live, err := claudeauth.ReadLive(ClaudeRoot(claudeHome))
	if err != nil {
		return "", false
	}
	org := strings.Map(func(r rune) rune {
		if strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_", r) {
			return r
		}
		return '-'
	}, live.Identity.OrganizationUUID)
	if org == "" {
		return "", false
	}
	return projstate.Dir("rc-policy-off", org), true
}
