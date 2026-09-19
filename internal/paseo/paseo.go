// Package paseo reaches the local Paseo daemon through its CLI.
//
// Paseo runs the coding sessions that used to live in tmux panes, so the parts
// of proj that spoke to a pane need a way to speak to an agent. The CLI is the
// supported surface for that and it is already on PATH wherever the daemon
// runs, which keeps this to process calls rather than a second implementation
// of the daemon's wire protocol.
//
// A machine without Paseo is not a broken machine: Available reports whether
// there is anything to talk to, and callers fall back to what they did before.
package paseo

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Bin is the command used to reach the daemon, overridable for tests.
var Bin = "paseo"

// Agent is one agent as `paseo agent ls --json` reports it.
type Agent struct {
	ID      string `json:"id"`
	ShortID string `json:"shortId"`
	Name    string `json:"name"`
	Status  string `json:"status"`
	Cwd     string `json:"cwd"`
}

// Running reports whether the agent is working on a turn right now.
func (a Agent) Running() bool { return a.Status == "running" }

// Available reports whether the Paseo CLI is installed.
func Available() bool {
	_, err := exec.LookPath(Bin)
	return err == nil
}

// Agents lists the agents across all directories. An unreachable daemon yields
// no agents rather than an error, since every caller treats the two the same.
func Agents() []Agent {
	out, err := exec.Command(Bin, "agent", "ls", "--global", "--json").Output()
	if err != nil {
		return nil
	}
	var agents []Agent
	if json.Unmarshal(out, &agents) != nil {
		return nil
	}
	return agents
}

// AgentFor returns the agent working in dir, or nil. Paseo abbreviates a path
// under the home directory with a tilde, so both sides are expanded before
// they are compared.
func AgentFor(dir string) *Agent {
	want := resolve(dir)
	if want == "" {
		return nil
	}
	agents := Agents()
	for i := range agents {
		if resolve(agents[i].Cwd) == want {
			return &agents[i]
		}
	}
	return nil
}

// Send delivers text to an agent as a turn of its own. It does not wait for the
// agent to work through it: the caller is a messenger, and a long reply is not
// its business.
func Send(id, text string) error {
	cmd := exec.Command(Bin, "agent", "send", id, "--prompt", text, "--no-wait")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("paseo agent send: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Stop interrupts a running agent. An idle agent is unaffected.
func Stop(id string) error {
	if out, err := exec.Command(Bin, "agent", "stop", id).CombinedOutput(); err != nil {
		return fmt.Errorf("paseo agent stop: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// resolve turns a path Paseo printed into one comparable with a path from the
// registry: tilde expanded, absolute, and with symlinks followed where they
// exist, since a project reached through a link is the same project.
func resolve(path string) string {
	if path == "" {
		return ""
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		path = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(path, "~"), "/"))
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return filepath.Clean(abs)
}
