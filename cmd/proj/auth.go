package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
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
	authWait      time.Duration
	authNow       bool
	authLoginName string
)

var authCmd = &cobra.Command{
	Use:   "auth",
	Short: "manage the Claude Code logins sessions run with",
	Long: `Manage the Claude Code logins proj's sessions run with.

proj keeps any number of logins and one of them is in use by every session at
once. On a terminal, ` + "`proj auth`" + ` lists them with the one in use marked: enter
switches to the selected login, a adds a login, r removes one. Elsewhere it
prints the status.

A login is its tokens and account details. Conversations, memory, settings and
Remote Control belong to no login and stay as they are when the login changes.

Changing the login in use stops every Claude session first, since a running
session keeps its login in memory and writes it back, and resumes each one
afterwards with its conversation. Idle sessions stop at once; a session that is
working, holding an unsent draft or waiting on an answer stops when that ends.
If that takes longer than --wait, nothing changes and the stopped sessions are
resumed. --now stops busy sessions without waiting.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		a, err := newAuth()
		if err != nil {
			return err
		}
		if !stdinIsTTY() {
			return a.status()
		}
		return a.interactive()
	},
}

var authStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "show the login in use and the saved ones",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		a, err := newAuth()
		if err != nil {
			return err
		}
		return a.status()
	},
}

var authLoginCmd = &cobra.Command{
	Use:   "login",
	Short: "log in with another account and use it",
	Long: `Log in with another Claude account and use it for every session.

The login in use is kept (it is saved first, under a name asked for if it has
none), sessions are stopped, Claude Code's own browser login runs, and the new
login is saved under --name or a name asked for. Sessions resume on the new
login. If the login is cancelled or fails, the previous login is put back.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		a, err := newAuth()
		if err != nil {
			return err
		}
		return a.login(authLoginName)
	},
}

var authLogoutCmd = &cobra.Command{
	Use:   "logout <name>",
	Short: "remove a saved login",
	Long: `Remove a saved login. The login in use cannot be removed; switch to another
one first.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		a, err := newAuth()
		if err != nil {
			return err
		}
		return a.logout(args[0])
	},
}

var authSwitchCmd = &cobra.Command{
	Use:   "switch [name]",
	Short: "use another saved login for every session",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		a, err := newAuth()
		if err != nil {
			return err
		}
		if len(args) == 0 {
			return a.interactive()
		}
		return a.switchTo(args[0])
	},
}

func init() {
	authCmd.PersistentFlags().DurationVar(&authWait, "wait", 30*time.Minute, "how long to wait for busy sessions before giving up")
	authCmd.PersistentFlags().BoolVar(&authNow, "now", false, "stop busy sessions without waiting for them")
	authLoginCmd.Flags().StringVar(&authLoginName, "name", "", "name to save the new login under")
	authCmd.AddCommand(authStatusCmd, authLoginCmd, authLogoutCmd, authSwitchCmd)
	rootCmd.AddCommand(authCmd)
}

// auth is the saved logins and the Claude Code root whose login they replace.
type auth struct {
	cfg   config.Config
	root  string
	store claudeauth.Store
}

func newAuth() (auth, error) {
	cfg, err := config.Load()
	if err != nil {
		return auth{}, err
	}
	return auth{
		cfg:   cfg,
		root:  daemon.ClaudeRoot(cfg.Claude.Home),
		store: claudeauth.Store{Dir: filepath.Join(filepath.Dir(daemonConfig().StatePath), "accounts", config.DefaultTool)},
	}, nil
}

// current reads the login in use and refreshes its saved copy, returning the
// name it is saved under, empty when it is not saved. Claude Code rotates tokens
// as it refreshes them, so a saved copy is only usable if it is kept current.
func (a auth) current() (claudeauth.Live, string, error) {
	live, err := claudeauth.ReadLive(a.root)
	if err != nil {
		return live, "", err
	}
	name, err := a.store.Refresh(live)
	if err != nil {
		return live, "", fmt.Errorf("update the saved copy of the login in use: %w", err)
	}
	return live, name, nil
}

func (a auth) status() error {
	accounts, err := a.store.List()
	if err != nil {
		return err
	}
	live, active, err := a.current()
	if err != nil {
		fmt.Printf("in use: none (%v)\n", err)
	} else {
		name := active
		if name == "" {
			name = "not saved"
		}
		fmt.Printf("in use: %s\n", name)
		for _, line := range accountLines(live.Identity, live.Plan, a.cfg) {
			fmt.Printf("  %s\n", line)
		}
	}
	if len(accounts) == 0 {
		fmt.Println("saved: none")
		return nil
	}
	fmt.Println("saved:")
	for _, acc := range accounts {
		mark := " "
		if acc.Name == active {
			mark = "*"
		}
		fmt.Printf("  %s %-14s %s\n", mark, acc.Name, plain(details(acc.Email, acc.Org, acc.Plan)))
	}
	return nil
}

// accountLines describes one login in full: who it is, what it may use, and the
// organization policies proj has learned, which are what actually differs
// between two logins of the same person.
func accountLines(id claudeauth.Identity, plan claudeauth.Plan, cfg config.Config) []string {
	org := id.OrganizationName
	if id.OrganizationType != "" {
		org += " (" + id.OrganizationType + ")"
	}
	lines := []string{
		"account:  " + id.EmailAddress,
		"org:      " + org,
	}
	sub := plan.Subscription
	if plan.Seat != "" {
		sub += ", seat " + plan.Seat
	}
	lines = append(lines, "plan:     "+sub)
	if plan.Tier != "" {
		lines = append(lines, "limits:   "+plan.Tier+extraUsageNote(plan))
	}
	if daemon.RCPolicyOff(cfg.Claude.Home) {
		lines = append(lines, "remote:   off (the organization's policy)")
	}
	return lines
}

func extraUsageNote(plan claudeauth.Plan) string {
	if plan.ExtraUsage {
		return ", extra usage past the limit allowed"
	}
	return ", no extra usage past the limit"
}

func (a auth) interactive() error {
	for {
		live, active, err := a.current()
		if err != nil {
			return err
		}
		accounts, err := a.store.List()
		if err != nil {
			return err
		}
		// A login in use that is not saved leads the list, so the cursor starts
		// on the one thing that has to happen before any switch: saving it.
		unsaved := active == ""
		lines := make([]string, 0, len(accounts)+1)
		if unsaved {
			lines = append(lines, fmt.Sprintf("\033[33m+\033[0m save the login in use  %s",
				details(live.Identity.EmailAddress, live.Identity.OrganizationName, live.Plan)))
		}
		for _, acc := range accounts {
			mark := "\033[90m○\033[0m"
			if acc.Name == active {
				mark = "\033[32m●\033[0m"
			}
			lines = append(lines, fmt.Sprintf("%s %-14s %s", mark, acc.Name, details(acc.Email, acc.Org, acc.Plan)))
		}
		footer := "↑/↓ move · enter switch · a add · r remove · esc quit"
		idx, act := selectAction("Claude Code logins", lines, footer, "ar")
		if idx < 0 {
			return nil
		}
		if act == 'a' {
			return a.login("")
		}
		if unsaved {
			idx--
		}
		if idx < 0 {
			if act != '\r' {
				continue
			}
			if name := promptLine("name for this login: "); name != "" {
				if err := a.store.Save(name, live); err != nil {
					fmt.Fprintf(os.Stderr, "save: %v\n", err)
				} else {
					fmt.Printf("saved %s as %s\n", live.Identity.EmailAddress, name)
				}
			}
			continue
		}
		acc := accounts[idx]
		switch act {
		case '\r':
			return a.switchTo(acc.Name)
		case 'r':
			if !confirm(fmt.Sprintf("remove the saved login %s (%s)? [y/N] ", acc.Name, acc.Email)) {
				continue
			}
			if err := a.logout(acc.Name); err != nil {
				fmt.Fprintf(os.Stderr, "remove: %v\n", err)
			}
		}
	}
}

func (a auth) switchTo(name string) error {
	live, active, err := a.current()
	if err != nil {
		return err
	}
	target, err := a.find(name)
	if err != nil {
		return err
	}
	if active == name {
		fmt.Printf("already using %s (%s)\n", name, target.Email)
		return nil
	}
	// Leaving a login nobody saved would throw its tokens away with no way back
	// short of logging in again.
	if active == "" {
		return fmt.Errorf("the login in use (%s) is not saved; save it with `proj auth` first", live.Identity.EmailAddress)
	}
	return a.whileStopped(active, func() error {
		if err := a.store.Apply(name, a.root); err != nil {
			return fmt.Errorf("switch to %s: %w", name, err)
		}
		fmt.Printf("switched to %s (%s)\n", name, plain(details(target.Email, target.Org, target.Plan)))
		return nil
	})
}

func (a auth) login(name string) error {
	if name != "" {
		if err := claudeauth.ValidName(name); err != nil {
			return err
		}
	}
	live, active, err := a.current()
	if err != nil {
		return err
	}
	if active == "" {
		fmt.Printf("the login in use (%s) is kept so you can switch back to it\n", live.Identity.EmailAddress)
		active = promptLine("name for it: ")
		if active == "" {
			return fmt.Errorf("the login in use needs a name before another one can be added")
		}
		if err := a.store.Save(active, live); err != nil {
			return err
		}
	}
	claude, err := exec.LookPath(config.DefaultTool)
	if err != nil {
		return fmt.Errorf("find the claude command to log in with: %w", err)
	}
	return a.whileStopped(active, func() error {
		login := exec.Command(claude, "auth", "login")
		login.Stdin, login.Stdout, login.Stderr = os.Stdin, os.Stdout, os.Stderr
		runErr := login.Run()
		after, readErr := claudeauth.ReadLive(a.root)
		switch {
		case runErr != nil || readErr != nil:
			a.restore(active)
			if runErr != nil {
				return fmt.Errorf("claude auth login: %w; kept %s", runErr, active)
			}
			return fmt.Errorf("read the new login: %w; kept %s", readErr, active)
		case after.Identity == live.Identity:
			fmt.Printf("still logged in as %s; nothing added\n", after.Identity.EmailAddress)
			return nil
		}
		if existing, ok, err := a.store.Find(after); err == nil && ok {
			if _, err := a.store.Refresh(after); err != nil {
				return err
			}
			fmt.Printf("%s is saved as %s already; using it\n", after.Identity.EmailAddress, existing.Name)
			return nil
		}
		for name == "" {
			name = promptLine(fmt.Sprintf("name for %s: ", after.Identity.EmailAddress))
		}
		if err := a.store.Save(name, after); err != nil {
			a.restore(active)
			return fmt.Errorf("save the new login: %w; kept %s", err, active)
		}
		fmt.Printf("logged in as %s, saved as %s\n",
			plain(details(after.Identity.EmailAddress, after.Identity.OrganizationName, after.Plan)), name)
		return nil
	})
}

// restore puts a saved login back in use after a failed login, reporting the
// case where even that fails, because sessions then resume logged out.
func (a auth) restore(name string) {
	if err := a.store.Apply(name, a.root); err != nil {
		fmt.Fprintf(os.Stderr, "could not put %s back in use, run `proj auth switch %s`: %v\n", name, name, err)
	}
}

func (a auth) logout(name string) error {
	if _, err := a.find(name); err != nil {
		return err
	}
	if _, active, err := a.current(); err == nil && active == name {
		return fmt.Errorf("%s is the login in use; switch to another one first", name)
	}
	if err := a.store.Remove(name); err != nil {
		return err
	}
	fmt.Printf("removed %s\n", name)
	return nil
}

func (a auth) find(name string) (claudeauth.Account, error) {
	accounts, err := a.store.List()
	if err != nil {
		return claudeauth.Account{}, err
	}
	names := make([]string, 0, len(accounts))
	for _, acc := range accounts {
		if acc.Name == name {
			return acc, nil
		}
		names = append(names, acc.Name)
	}
	if len(names) == 0 {
		return claudeauth.Account{}, fmt.Errorf("no saved login %q; none are saved", name)
	}
	return claudeauth.Account{}, fmt.Errorf("no saved login %q; saved: %s", name, strings.Join(names, ", "))
}

// whileStopped runs change with every Claude session stopped and resumes them
// afterwards. When the sessions cannot all be stopped, change does not run and
// the ones already stopped resume on the login named active, which is unchanged.
func (a auth) whileStopped(active string, change func() error) error {
	running, unmanaged := claudeSessions(a.cfg)
	if len(unmanaged) > 0 {
		fmt.Printf("not started by proj, restart these yourself afterwards: %s\n", strings.Join(unmanaged, ", "))
	}
	// A Claude outside tmux belongs to something proj cannot stop, and one that
	// is still running when the login changes writes the old login back over the
	// new one. Reported rather than stopped: another supervisor's agents are its
	// own to restart.
	if spec, err := a.cfg.Tool(config.DefaultTool); err == nil {
		if outside := claudeOutsideTmux(spec.Command); len(outside) > 0 {
			fmt.Printf("running outside tmux and will overwrite the new login when they stop; stop these first: %s\n",
				strings.Join(outside, ", "))
		}
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
	stopped, err := stopSessions(ctx, a.cfg, others)
	if err != nil {
		resumeSessions(a.cfg, stopped)
		return fmt.Errorf("%w; the stopped sessions were resumed and %s is still in use", err, active)
	}
	changeErr := change()
	resumeSessions(a.cfg, stopped)
	if self != nil {
		line := daemon.LaunchCommand(self.spec, a.cfg.Claude.Home, self.project.Name, self.session, self.dir)
		if err := tmux.DeferredRespawnSession(self.session, self.dir, line); err != nil {
			fmt.Fprintf(os.Stderr, "could not schedule a restart of this session %s, restart it yourself: %v\n", self.session, err)
		} else {
			fmt.Printf("this session (%s) restarts in a couple of seconds\n", self.session)
		}
	}
	return changeErr
}

// authSession is a running Claude session a login change stops and resumes.
type authSession struct {
	session string
	pane    string
	dir     string
	project projects.Project
	spec    config.ToolSpec
	self    bool
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

// details is the one-line form the lists use: who the login is and what it may
// use, with the organization left out when it is the personal one every account
// has (it is named after the account itself and says nothing).
func details(email, org string, plan claudeauth.Plan) string {
	parts := []string{email}
	if org != "" && !strings.HasPrefix(org, email) {
		parts = append(parts, org)
	}
	parts = append(parts, plan.String())
	return "\033[2m" + strings.Join(parts, " · ") + "\033[0m"
}

func plain(s string) string { return ansiSeq.ReplaceAllString(s, "") }

func promptLine(prompt string) string {
	fmt.Print(prompt)
	ans, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimSpace(ans)
}
