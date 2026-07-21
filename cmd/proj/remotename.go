package main

import (
	"fmt"
	"os"
	"time"

	"github.com/tieo/proj/internal/claudeapi"
	"github.com/tieo/proj/internal/daemon"
)

// bridgeWait bounds how long a rename waits for a relaunched session to bind
// Remote Control. Binding takes a few seconds; past this the session either
// does not use Remote Control or is still starting, and the rename is held up
// no further.
const bridgeWait = 25 * time.Second

// retitleRemote renames the project's Remote Control session at claude.ai/code
// to match the session it now has locally.
//
// The name a session shows there is fixed when its bridge first registers, and
// resuming a conversation reuses that bridge: relaunching with a new
// --remote-control name leaves the old title on the phone and in the web list,
// so a renamed or re-tagged project keeps answering under the name it had.
// Nothing local shows this - the process, the pane and Claude's own session
// file all carry the new name - which is why it goes unnoticed.
//
// id is the bridge read before the directory moved. It is empty when the
// session had not bound yet, and then dir (the project's new directory) is
// polled until it does. Best effort throughout: an offline account or an
// unreachable API is no reason to fail a rename that already happened.
func retitleRemote(claudeHome, id, dir, session string) {
	if id == "" {
		id = waitForBridge(claudeHome, dir)
	}
	if id == "" {
		return // no Remote Control session bound to this project
	}
	token, err := claudeapi.Token(daemon.ClaudeRoot(claudeHome))
	if err != nil {
		return // not logged in with a Claude account; nothing to rename
	}
	host, _ := os.Hostname()
	title := daemon.RCName(session, host)
	if err := claudeapi.RenameSession(token, id, title); err != nil {
		fmt.Fprintf(os.Stderr, "warning: remote session still named for the old project: %v\n", err)
		return
	}
	fmt.Printf("renamed the remote session to %s\n", title)
}

// waitForBridge polls for the Remote Control session bound to dir until one
// appears or bridgeWait elapses.
func waitForBridge(claudeHome, dir string) string {
	deadline := time.Now().Add(bridgeWait)
	for {
		if id := daemon.BridgeSessionID(claudeHome, dir); id != "" {
			return id
		}
		if time.Now().After(deadline) {
			return ""
		}
		time.Sleep(2 * time.Second)
	}
}
