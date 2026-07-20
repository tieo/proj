package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/tieo/proj/internal/config"
	"github.com/tieo/proj/internal/daemon"
	"github.com/tieo/proj/internal/projects"
	"github.com/tieo/proj/internal/tmux"
)

// managerSession is the fixed session name for the manager. The manager is proj
// infrastructure, tracked as a System managed session, never a base_dir project.
const managerSession = "manager"

// managerDir is the manager's working directory: its own git repo under
// XDG_DATA_HOME (the standard home for app-managed persistent data), kept
// outside base_dir so it can never be confused with a user project.
func managerDir() string {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "proj", "manager")
}

var managerCmd = &cobra.Command{
	Use:   "manager [on|off]",
	Short: "open the always-on manager session (proj infrastructure, not a project)",
	Long: `Open, enable, or disable the manager.

The manager is a talkable, always-on Claude session that oversees the fleet:
the daemon routes decisions to it, it can delegate work to other sessions, and
(later) it holds secrets via sops. It is proj's own infrastructure, so it lives
in its own git repo under XDG_DATA_HOME (not base_dir) and is tracked as a
system session with its own ⌂ marker in the list.

  proj manager       scaffold if needed, pin it, and attach
  proj manager on    same as bare
  proj manager off   unpin and close it`,
	Args: cobra.MaximumNArgs(1),
	RunE: runManager,
}

var managerInboxCmd = &cobra.Command{
	Use:   "inbox",
	Short: "show fleet decisions the daemon queued for the manager (--drain to clear)",
	Args:  cobra.NoArgs,
	RunE:  runManagerInbox,
}

var inboxDrain bool

func init() {
	managerInboxCmd.Flags().BoolVar(&inboxDrain, "drain", false, "clear the inbox after showing it")
	managerCmd.AddCommand(managerInboxCmd)
	rootCmd.AddCommand(managerCmd)
}

// runManagerInbox prints the queued decisions the daemon routed to the manager
// (goal-nudge verdicts that need a human call), newest last. --drain clears them
// once handled. The manager reads this to triage the fleet.
func runManagerInbox(cmd *cobra.Command, args []string) error {
	statePath := daemonConfig().StatePath
	items := daemon.ReadInbox(statePath)
	if len(items) == 0 {
		fmt.Println("inbox empty")
		return nil
	}
	for _, it := range items {
		fmt.Printf("• %s  %s\n", it.TS, it.Session)
		if it.Goal != "" {
			fmt.Printf("    goal: %s\n", it.Goal)
		}
		if it.Reason != "" {
			fmt.Printf("    why:  %s\n", it.Reason)
		}
		if it.Callout != "" {
			fmt.Printf("    next: %s\n", it.Callout)
		}
	}
	fmt.Printf("\n%d item(s)\n", len(items))
	if inboxDrain {
		if err := daemon.DrainInbox(statePath); err != nil {
			return err
		}
		fmt.Println("inbox drained")
	}
	return nil
}

func runManager(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if len(args) == 1 {
		switch args[0] {
		case "off":
			return managerOff()
		case "on":
			// fall through to open
		default:
			return fmt.Errorf("expected on or off, got %q", args[0])
		}
	}
	return managerOn(cfg)
}

// managerOn scaffolds the manager repo if needed, records it as a pinned system
// session (so keep-alive maintains it), and opens/attaches it.
func managerOn(cfg config.Config) error {
	dir := managerDir()
	if err := scaffoldManager(dir); err != nil {
		return err
	}
	if err := daemon.UpdateManagedState(daemonConfig().StatePath, func(m daemon.ManagedState) error {
		ms := m[managerSession]
		ms.Name = managerSession
		ms.Dir = dir
		ms.Pinned = true
		ms.System = true
		m[managerSession] = ms
		return nil
	}); err != nil {
		return err
	}
	// Reuse the normal open machinery with a synthetic project rooted at the
	// manager dir; its tool is the default (claude), its persona comes from the
	// CLAUDE.md the scaffold wrote.
	return openInTmux(cfg, projects.Project{Name: managerSession, Dir: dir})
}

// managerOff clears the manager's system/pinned flags so keep-alive stops
// maintaining it, then closes its session.
func managerOff() error {
	if err := daemon.UpdateManagedState(daemonConfig().StatePath, func(m daemon.ManagedState) error {
		ms := m[managerSession]
		ms.Pinned = false
		ms.System = false
		// Mark a clean close so keep-alive does not resurrect it after the kill.
		ms.ExitedCleanly = true
		m[managerSession] = ms
		return nil
	}); err != nil {
		return err
	}
	_ = tmux.KillSession(managerSession)
	fmt.Println("manager: off")
	return nil
}

// scaffoldManager creates the manager's git repo and seeds its persona on first
// use. It is idempotent: an existing repo (CLAUDE.md present) is left untouched,
// so the manager's own edits and history are never overwritten.
func scaffoldManager(dir string) error {
	if _, err := os.Stat(filepath.Join(dir, "CLAUDE.md")); err == nil {
		return nil // already scaffolded
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); os.IsNotExist(err) {
		c := exec.Command("git", "init", "-q")
		c.Dir = dir
		if err := c.Run(); err != nil {
			return fmt.Errorf("git init manager repo: %w", err)
		}
	}
	files := map[string]string{
		"CLAUDE.md":  managerPersona,
		"README.md":  managerReadme,
		".gitignore": managerGitignore,
	}
	for name, body := range files {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			continue
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			return err
		}
	}
	fmt.Printf("scaffolded manager repo at %s\n", dir)
	return nil
}

const managerPersona = `# proj manager

You are the manager: a talkable, always-on session that oversees the fleet of
proj-managed coding sessions on this host. You are proj's own infrastructure,
not a project. Your working directory is this git repo; keep your notes, state,
and (later) sops-encrypted secrets here, and commit as you change them.

## Role

- Be the human's single point of contact for the fleet. They talk to you here.
- Triage what the daemon routes to you (goal-nudge decisions that need a human):
  handle what you can, escalate only what genuinely needs the user.
- Delegate work to other sessions when asked, with ` + "`proj send`" + ` (below).
- Reach external services (Jira, etc.) through your own claude.ai MCP
  connectors - OAuth, so no API token ever passes through chat or proj. Run
  ` + "`/mcp`" + ` to see and authenticate connectors.

## Inbox

When the daemon's goal-nudge finds a session that stopped and needs a human
call, it queues that decision for you instead of pushing the user's phone, and
wakes you here. Run ` + "`proj manager inbox`" + ` to read the queued items
(session, goal, why, suggested next step). Handle what you can yourself - open
the session with ` + "`proj <name>`" + ` and continue or unblock it - and escalate
to the user only what genuinely needs them. Clear handled items with
` + "`proj manager inbox --drain`" + `.

## Tools

- proj CLI: ` + "`proj list`" + ` (the fleet, you are the ⌂ row), ` + "`proj <name>`" + `
  to open a session, ` + "`proj daemon goal-nudge`" + ` for judged states,
  ` + "`proj manager inbox`" + ` for queued decisions.
- Delegate: ` + "`proj send <session|project> \"<task>\"`" + ` types a task into
  another session and submits it, then goal-nudge watches it. It won't type over
  someone's unsent draft unless you pass --force.
- Connectors: your claude.ai MCP connectors (` + "`/mcp`" + `) reach Jira etc. via
  OAuth - no tokens needed.
- This repo: your durable memory and workspace. Commit your changes.

Not wired yet: a sops secrets toolkit for non-MCP credentials.
`

const managerReadme = `# proj manager repo

Working directory and git repo for the proj **manager** session (see ` + "`proj manager`" + `).
This is proj infrastructure under XDG_DATA_HOME, not a project under base_dir.
It can be pushed to a remote and managed from elsewhere.
`

const managerGitignore = `# decrypted secrets must never be committed
*.dec
*.plain
secrets.decrypted
`
