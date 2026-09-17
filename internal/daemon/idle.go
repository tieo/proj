package daemon

import "github.com/tieo/proj/internal/tmux"

// Restartable reports whether the Claude Code session in pane can be stopped
// now without losing anything, and when it cannot, why. It uses the same
// readings the doner backstop trusts before it types into a pane: an input box
// on screen, no turn generating, no unsent draft, and no tool call waiting for
// its result, which covers a question or permission prompt the user has not
// answered yet.
func Restartable(homeOverride, pane, dir string, captureLines int) (bool, string) {
	content := tmux.CapturePane(pane, captureLines)
	if !inputPromptRE.MatchString(content) {
		return false, "no input box on screen"
	}
	if connDropBusyRE.MatchString(content) {
		return false, "working"
	}
	if composerHasDraft(tmux.CapturePaneEsc(pane)) {
		return false, "unsent draft"
	}
	if f := recentSessionFile(homeOverride, dir); f != "" && awaitingToolResult(f) {
		return false, "waiting on a tool call or an answer"
	}
	return true, ""
}
