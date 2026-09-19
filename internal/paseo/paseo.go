// Package paseo reaches the local Paseo daemon through its CLI.
//
// Paseo runs the sessions, so anything that wants to speak to one speaks to an
// agent. The CLI is the supported surface for that and is on PATH wherever the
// daemon runs, which keeps this to process calls rather than a second
// implementation of the daemon's wire protocol.
//
// A machine without Paseo is not a broken machine: Available reports whether
// there is anything to talk to.
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

// Idle reports whether the agent is between turns. Only an idle agent can be
// stuck: one that is working will speak for itself when it is done.
func (a Agent) Idle() bool { return a.Status == "idle" }

// Dir is Cwd with the tilde Paseo prints expanded.
func (a Agent) Dir() string { return Resolve(a.Cwd) }

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

// AgentFor returns the agent working in dir, or nil.
func AgentFor(dir string) *Agent {
	want := Resolve(dir)
	if want == "" {
		return nil
	}
	agents := Agents()
	for i := range agents {
		if agents[i].Dir() == want {
			return &agents[i]
		}
	}
	return nil
}

// SessionID is the provider session an agent is running, which is what names
// its transcript. Paseo keeps one record per agent beside its own state.
func SessionID(home, agentID, dir string) string {
	slug := strings.ReplaceAll(strings.TrimPrefix(Resolve(dir), "/"), "/", "-")
	path := filepath.Join(home, "agents", slug, agentID+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var rec struct {
		RuntimeInfo struct {
			SessionID string `json:"sessionId"`
		} `json:"runtimeInfo"`
		Persistence struct {
			SessionID string `json:"sessionId"`
		} `json:"persistence"`
	}
	if json.Unmarshal(data, &rec) != nil {
		return ""
	}
	if rec.RuntimeInfo.SessionID != "" {
		return rec.RuntimeInfo.SessionID
	}
	return rec.Persistence.SessionID
}

// Home is where the Paseo daemon keeps its state.
func Home() string {
	if v := os.Getenv("PASEO_HOME"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".paseo")
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

// Resolve turns a path Paseo printed into one comparable with a path from the
// registry: tilde expanded, absolute, symlinks followed where they exist.
func Resolve(path string) string {
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
