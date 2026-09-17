package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/tieo/proj/internal/claudeauth"
	"github.com/tieo/proj/internal/config"
	"github.com/tieo/proj/internal/daemon"
	"github.com/tieo/proj/internal/projects"
	"github.com/tieo/proj/internal/shellout"
	"github.com/tieo/proj/internal/tmux"
)

var (
	authWait time.Duration
	authNow  bool
)

var authCmd = &cobra.Command{
	Use:   "auth <tool> [account]",
	Short: "switch the account a coding tool is logged in with",
	Long: `Pick which saved login Claude Code uses, for every session at once.

With no account, an interactive list shows the saved logins with the active
one marked: enter switches to the selected one, r removes a saved login, and
a login that is not saved yet can be saved from the list. With an account
name, it switches to that login directly.

A login is its tokens and account details; conversations, memory, settings
and Remote Control are shared by all of them and carry across a switch.

A running session holds its login in memory and would write it back, so a
switch stops every Claude session first and resumes each one afterwards with
its conversation. Idle sessions stop at once; a session that is working,
holding an unsent draft, or waiting on an answer is stopped as soon as that
ends. If that takes longer than --wait, the stopped sessions are resumed on the
old login and nothing is switched. --now stops busy sessions too.

To add a login: save the current one here, run /login in a Claude session
with the other account, then open this list again and save that one.`,
	Args: cobra.RangeArgs(1, 2),
	RunE: runAuth,
}

func init() {
	authCmd.Flags().DurationVar(&authWait, "wait", 30*time.Minute, "how long to wait for busy sessions before giving up")
	authCmd.Flags().BoolVar(&authNow, "now", false, "stop busy sessions too instead of waiting for them")
	rootCmd.AddCommand(authCmd)
}

func runAuth(cmd *cobra.Command, args []string) error {
	if args[0] != config.DefaultTool {
		return fmt.Errorf("only %s logins can be switched, not %q", config.DefaultTool, args[0])
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	root := daemon.ClaudeRoot(cfg.Claude.Home)
	store := claudeauth.Store{Dir: filepath.Join(filepath.Dir(daemonConfig().StatePath), "accounts", config.DefaultTool)}
	if len(args) == 2 {
		return switchAccount(cfg, store, root, args[1])
	}
	return authInteractive(cfg, store, root)
}

func authInteractive(cfg config.Config, store claudeauth.Store, root string) error {
	for {
		live, err := claudeauth.ReadLive(root)
		if err != nil {
			return err
		}
		active, err := store.Refresh(live)
		if err != nil {
			return fmt.Errorf("update the saved copy of the current login: %w", err)
		}
		accounts, err := store.List()
		if err != nil {
			return err
		}
		// An unsaved login leads the list: the cursor starts on the first row,
		// and every switch refuses to leave an unsaved login behind, so saving
		// it is the only thing that can happen first.
		unsaved := active == ""
		lines := make([]string, 0, len(accounts)+1)
		if unsaved {
			lines = append(lines, fmt.Sprintf("\033[33m+\033[0m save current login  %s",
				accountDetails(live.Identity.EmailAddress, live.Identity.OrganizationName, live.Plan)))
		}
		for _, a := range accounts {
			mark := "\033[90m○\033[0m"
			if a.Name == active {
				mark = "\033[32m●\033[0m"
			}
			lines = append(lines, fmt.Sprintf("%s %-14s %s", mark, a.Name, accountDetails(a.Email, a.Org, a.Plan)))
		}
		footer := "↑/↓ move · enter switch · r remove · esc quit"
		idx, act := selectAction("Claude logins", lines, footer, "r")
		if idx < 0 {
			return nil
		}
		if unsaved {
			idx--
		}
		if idx < 0 {
			if act != '\r' {
				continue
			}
			name := promptLine("name for this login: ")
			if name == "" {
				continue
			}
			if err := store.Save(name, live); err != nil {
				fmt.Fprintf(os.Stderr, "save: %v\n", err)
				continue
			}
			fmt.Printf("saved %s as %s\n", live.Identity.EmailAddress, name)
			continue
		}
		a := accounts[idx]
		switch act {
		case '\r':
			return switchAccount(cfg, store, root, a.Name)
		case 'r':
			if !confirm(fmt.Sprintf("remove the saved login %s (%s)? the current login is not affected [y/N] ", a.Name, a.Email)) {
				continue
			}
			if err := store.Remove(a.Name); err != nil {
				fmt.Fprintf(os.Stderr, "remove: %v\n", err)
				continue
			}
			fmt.Printf("removed %s\n", a.Name)
		}
	}
}

func accountDetails(email, org, plan string) string {
	parts := []string{email}
	if org != "" {
		parts = append(parts, org)
	}
	if plan != "" {
		parts = append(parts, plan)
	}
	return "\033[2m" + strings.Join(parts, " · ") + "\033[0m"
}

// authSession is a running Claude session a switch has to stop and resume.
type authSession struct {
	session string
	pane    string
	dir     string
	project projects.Project
	spec    config.ToolSpec
	self    bool
}

func switchAccount(cfg config.Config, store claudeauth.Store, root, name string) error {
	live, err := claudeauth.ReadLive(root)
	if err != nil {
		return err
	}
	active, err := store.Refresh(live)
	if err != nil {
		return fmt.Errorf("update the saved copy of the current login: %w", err)
	}
	accounts, err := store.List()
	if err != nil {
		return err
	}
	var target *claudeauth.Account
	for i := range accounts {
		if accounts[i].Name == name {
			target = &accounts[i]
		}
	}
	if target == nil {
		return fmt.Errorf("no saved login %q; saved: %s", name, accountNames(accounts))
	}
	if active == name {
		fmt.Printf("already using %s (%s)\n", name, target.Email)
		return nil
	}
	// Switching away from a login nobody saved would throw its tokens away
	// with no way back short of logging in again.
	if active == "" {
		return fmt.Errorf("the current login %s is not saved; save it with `proj auth claude` first", live.Identity.EmailAddress)
	}

	running, unmanaged := claudeSessions(cfg)
	if len(unmanaged) > 0 {
		fmt.Printf("not started by proj, restart these yourself after the switch: %s\n", strings.Join(unmanaged, ", "))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var others []authSession
	var self *authSession
	for i := range running {
		if running[i].self {
			self = &running[i]
		} else {
			others = append(others, running[i])
		}
	}
	stopped, err := stopSessions(ctx, cfg, others)
	if err != nil {
		resumeSessions(cfg, stopped)
		return fmt.Errorf("%w; the stopped sessions were resumed on %s and the login is unchanged", err, active)
	}

	if err := store.Apply(name, root); err != nil {
		resumeSessions(cfg, stopped)
		return fmt.Errorf("switch to %s: %w", name, err)
	}
	fmt.Printf("switched to %s (%s)\n", name, ansiSeq.ReplaceAllString(accountDetails(target.Email, target.Org, target.Plan), ""))
	resumeSessions(cfg, stopped)

	if self != nil {
		line := daemon.LaunchCommand(self.spec, cfg.Claude.Home, self.project.Name, self.session, self.dir)
		if err := tmux.DeferredRespawnSession(self.session, self.dir, line); err != nil {
			return fmt.Errorf("switched, but could not schedule a restart of this session %s, restart it yourself: %w", self.session, err)
		}
		fmt.Printf("this session (%s) restarts in a couple of seconds\n", self.session)
	}
	return nil
}

// claudeSessions finds the tmux panes running Claude Code. A pane whose
// directory is not a proj project cannot be relaunched with its own command, so
// it is named for the user instead. A parked pane (a plain shell left by a
// rename) has no start command and runs no tool.
func claudeSessions(cfg config.Config) (running []authSession, unmanaged []string) {
	byDir := map[string]projects.Project{}
	for _, p := range projects.All(cfg.BaseDir) {
		byDir[p.Dir] = p
	}
	reg, _ := projects.LoadRegistry()
	cwd, _ := os.Getwd()
	seen := map[string]bool{}
	for _, pane := range tmux.ListPanes() {
		if seen[pane.Session] {
			continue
		}
		seen[pane.Session] = true
		start := shellout.Run("tmux", "display-message", "-p", "-t", pane.ID, "#{pane_start_command}")
		if strings.TrimSpace(start) == "" {
			continue
		}
		dir := tmux.PaneCurrentPath(pane.ID)
		p, ok := byDir[dir]
		if !ok {
			if strings.Contains(start, config.DefaultTool) {
				unmanaged = append(unmanaged, pane.Session)
			}
			continue
		}
		p.Tool = reg.Tool(p.Name)
		if daemon.ToolName(p.Tool) != config.DefaultTool {
			continue
		}
		spec, err := cfg.Tool(p.Tool)
		if err != nil {
			continue
		}
		running = append(running, authSession{
			session: pane.Session,
			pane:    pane.ID,
			dir:     dir,
			project: p,
			spec:    spec,
			// A Claude session running this command stops itself last, after
			// the command has returned. tmux names the pane of a command run
			// inside it; claude.exe under WSL runs its shell outside tmux and
			// passes no environment across wsl.exe, so there the session's own
			// directory is the only thing that identifies it.
			self: pane.ID == os.Getenv("TMUX_PANE") ||
				(os.Getenv("TMUX_PANE") == "" && cwd == dir),
		})
	}
	return running, unmanaged
}

// stopSessions parks each session in a plain shell as soon as it can be
// stopped without losing anything, and returns the ones it parked. It gives up
// when --wait runs out or the command is interrupted, returning what it parked
// so far for the caller to resume.
func stopSessions(ctx context.Context, cfg config.Config, pending []authSession) ([]authSession, error) {
	capture := daemonConfig().Capture
	deadline := time.Now().Add(authWait)
	var stopped []authSession
	lastWaiting := ""
	for {
		var busy []authSession
		var reasons []string
		for _, s := range pending {
			ok, why := daemon.Restartable(cfg.Claude.Home, s.pane, s.dir, capture)
			if !ok && !authNow {
				busy = append(busy, s)
				reasons = append(reasons, s.project.Name+" ("+why+")")
				continue
			}
			if err := tmux.RespawnShell(s.session, s.dir); err != nil {
				return stopped, fmt.Errorf("stop %s: %w", s.session, err)
			}
			stopped = append(stopped, s)
			fmt.Printf("stopped %s\n", s.session)
		}
		if len(busy) == 0 {
			return stopped, nil
		}
		sort.Strings(reasons)
		if waiting := strings.Join(reasons, ", "); waiting != lastWaiting {
			fmt.Printf("waiting on %s\n", waiting)
			lastWaiting = waiting
		}
		if time.Now().After(deadline) {
			return stopped, fmt.Errorf("still busy after %s: %s", authWait, lastWaiting)
		}
		select {
		case <-ctx.Done():
			return stopped, fmt.Errorf("interrupted while waiting on %s", lastWaiting)
		case <-time.After(5 * time.Second):
		}
		pending = busy
	}
}

func resumeSessions(cfg config.Config, stopped []authSession) {
	for _, s := range stopped {
		line := daemon.LaunchCommand(s.spec, cfg.Claude.Home, s.project.Name, s.session, s.dir)
		if err := tmux.RespawnSession(s.session, s.dir, line); err != nil {
			fmt.Fprintf(os.Stderr, "could not resume %s, open it with `proj %s`: %v\n", s.session, s.project.Name, err)
			continue
		}
		fmt.Printf("resumed %s\n", s.session)
	}
}

func accountNames(accounts []claudeauth.Account) string {
	if len(accounts) == 0 {
		return "none"
	}
	names := make([]string, len(accounts))
	for i, a := range accounts {
		names[i] = a.Name
	}
	return strings.Join(names, ", ")
}

func promptLine(prompt string) string {
	fmt.Print(prompt)
	ans, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimSpace(ans)
}
