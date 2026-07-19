package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/tieo/proj/internal/config"
	"github.com/tieo/proj/internal/daemon"
	"github.com/tieo/proj/internal/overseer"
	"github.com/tieo/proj/internal/tmux"
)

var daemonCmd = &cobra.Command{
	Use:   "daemon",
	Short: "background daemon: auto-resume after usage limits, recreate pinned sessions",
	Long: `Polls tmux panes for Claude Code's usage-limit banner ("You're out of
extra usage · resets 3am"). On detection, sends Escape to dismiss the
/rate-limit-options selector (if present) and types "continue". It also
recreates pinned and keep-alive sessions that have vanished.

Pin a project from the top level with ` + "`proj pin`" + `. Run the daemon as a
user service (` + "`proj daemon enable`" + `) or in the foreground for
debugging (` + "`proj daemon run`" + `).`,
	RunE: runDaemonStatus,
}

var daemonRunCmd = &cobra.Command{
	Use:   "run",
	Short: "run the daemon in foreground (service unit calls this)",
	RunE:  runDaemonForeground,
}

var (
	daemonStartCmd   = systemctlCmd("start", "start the service")
	daemonStopCmd    = systemctlCmd("stop", "stop the service")
	daemonRestartCmd = systemctlCmd("restart", "restart the service")
	daemonEnableCmd  = systemctlCmd("enable --now", "enable and start the service")
	daemonDisableCmd = systemctlCmd("disable --now", "stop and disable the service")
	daemonLogsCmd    = &cobra.Command{
		Use:   "logs",
		Short: "tail the service logs",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runForeground("journalctl", "--user", "-u", "proj-daemon", "-f")
		},
	}
)

var daemonKeepAliveCmd = &cobra.Command{
	Use:   "keep-alive [on|off]",
	Short: "show or set the global keep-alive flag",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runDaemonKeepAlive,
}

var daemonMarkClosedCmd = &cobra.Command{
	Use:    "mark-closed <session>",
	Short:  "mark a session as intentionally closed (called by shell exit trap)",
	Args:   cobra.ExactArgs(1),
	RunE:   runDaemonMarkClosed,
	Hidden: true,
}

func init() {
	rootCmd.AddCommand(daemonCmd)
	daemonCmd.AddCommand(daemonRunCmd, daemonStartCmd, daemonStopCmd,
		daemonRestartCmd, daemonEnableCmd, daemonDisableCmd, daemonLogsCmd,
		daemonKeepAliveCmd, daemonMarkClosedCmd, overseerCmd)
}

// ----- status output -----

type serviceInfo struct {
	exists        bool
	loadState     string
	activeState   string
	subState      string
	unitFileState string
	fragmentPath  string
	mainPID       int
	memory        uint64
	activeEnter   time.Time
}

func (s serviceInfo) dot() string {
	switch s.activeState {
	case "active":
		return "\033[32m●\033[0m"
	case "failed":
		return "\033[31m●\033[0m"
	case "activating", "reloading":
		return "\033[33m●\033[0m"
	default:
		return "\033[90m○\033[0m"
	}
}

func gatherService() serviceInfo {
	if runtime.GOOS != "linux" {
		return serviceInfo{}
	}
	out, err := exec.Command("systemctl", "--user", "show",
		"-p", "LoadState",
		"-p", "ActiveState",
		"-p", "SubState",
		"-p", "FragmentPath",
		"-p", "UnitFileState",
		"-p", "ActiveEnterTimestamp",
		"-p", "MainPID",
		"-p", "MemoryCurrent",
		"proj-daemon").Output()
	if err != nil {
		return serviceInfo{}
	}
	s := serviceInfo{}
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch k {
		case "LoadState":
			s.loadState = v
		case "ActiveState":
			s.activeState = v
		case "SubState":
			s.subState = v
		case "FragmentPath":
			s.fragmentPath = v
		case "UnitFileState":
			s.unitFileState = v
		case "ActiveEnterTimestamp":
			if v != "" && v != "n/a" {
				if t, err := time.ParseInLocation("Mon 2006-01-02 15:04:05 MST", v, time.Local); err == nil {
					s.activeEnter = t
				}
			}
		case "MainPID":
			s.mainPID, _ = strconv.Atoi(v)
		case "MemoryCurrent":
			if n, err := strconv.ParseUint(v, 10, 64); err == nil {
				s.memory = n
			}
		}
	}
	s.exists = s.loadState == "loaded"
	return s
}

func formatBytes(b uint64) string {
	switch {
	case b < 1024:
		return fmt.Sprintf("%dB", b)
	case b < 1024*1024:
		return fmt.Sprintf("%.1fK", float64(b)/1024)
	case b < 1024*1024*1024:
		return fmt.Sprintf("%.1fM", float64(b)/(1024*1024))
	default:
		return fmt.Sprintf("%.1fG", float64(b)/(1024*1024*1024))
	}
}

// formatDur prints a Duration without the trailing-zero noise of String()
// ("1m0s" -> "1m", "5h0m0s" -> "5h"). Used for compact config display.
func formatDur(d time.Duration) string {
	h := int(d / time.Hour)
	d %= time.Hour
	m := int(d / time.Minute)
	d %= time.Minute
	s := int(d / time.Second)
	var parts []string
	if h > 0 {
		parts = append(parts, fmt.Sprintf("%dh", h))
	}
	if m > 0 {
		parts = append(parts, fmt.Sprintf("%dm", m))
	}
	if s > 0 || len(parts) == 0 {
		parts = append(parts, fmt.Sprintf("%ds", s))
	}
	return strings.Join(parts, "")
}

func formatAgo(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dmin %ds", int(d.Minutes()), int(d.Seconds())%60)
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %dmin", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}

// renderManaged prints the sessions the daemon is responsible for keeping
// around: the pinned and keep-alive entries from its managed state, each with a
// live/dead marker. The full project list lives in `proj` / `proj list`; the
// daemon status deliberately shows only what the daemon itself acts on.
func renderManaged(cfg daemon.Config) {
	managed, err := daemon.LoadManagedState(cfg.StatePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "managed state unreadable: %v\n", err)
		return
	}
	live := make(map[string]bool)
	for _, s := range tmux.ListSessions() {
		live[s.Name] = true
	}

	type row struct {
		name, dir, role string
		alive           bool
	}
	var rows []row
	pinned, kept := 0, 0
	for _, ms := range managed {
		switch {
		case ms.Pinned:
			pinned++
			rows = append(rows, row{ms.Name, ms.Dir, "pinned", live[ms.Name]})
		case ms.KeepAlive:
			kept++
			rows = append(rows, row{ms.Name, ms.Dir, "keep-alive", live[ms.Name]})
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].name < rows[j].name })

	fmt.Println()
	if len(rows) == 0 && !cfg.KeepAlive {
		fmt.Println("    Managed: none (pin a project with `proj pin <name>`)")
		return
	}
	note := ""
	if cfg.KeepAlive {
		note = "; global keep-alive on"
	}
	fmt.Printf("    Managed: %d pinned, %d keep-alive%s\n", pinned, kept, note)
	for _, r := range rows {
		dot, suffix := "\033[32m●\033[0m", ""
		if !r.alive {
			dot, suffix = "\033[90m○\033[0m", "  \033[90m(will recreate)\033[0m"
		}
		fmt.Printf("      %s %-10s %s  \033[90m%s\033[0m%s\n", dot, r.role, r.name, r.dir, suffix)
	}
}

// renderOverseer prints the daemon status's overseer block: one line when off,
// and when on, its cadence, last look, and today's spend against the budget, so
// the fleet judge is visible in the same view as the resume watchdog. Full
// detail (per-session nudges, recent looks) lives in `proj daemon overseer`.
func renderOverseer() {
	cfg, err := config.Load()
	if err != nil {
		return
	}
	ov := cfg.Daemon.Overseer
	fmt.Println()
	if !ov.Enabled {
		fmt.Printf("   Overseer: off  (model=%s interval=%s; enable with `proj daemon overseer on`)\n",
			ov.Model, ov.Interval)
		return
	}
	recs := overseer.ReadUsageLog()
	lastLook, sessions := overseer.ReadLookState()
	now := time.Now()
	last := "none yet"
	if !lastLook.IsZero() {
		last = formatAgo(now.Sub(lastLook)) + " ago"
	}
	looks, eff := overseer.TodayUsage(recs, now)
	pct := 0
	if overseer.DayBudget > 0 {
		pct = eff * 100 / overseer.DayBudget
	}
	fmt.Printf("   Overseer: on · model=%s interval=%s · last look %s\n", ov.Model, ov.Interval, last)
	fmt.Printf("             today %d looks · ~%s eff (%d%% of %s budget)\n",
		looks, formatK(eff), pct, formatK(overseer.DayBudget))
	pending := 0
	for _, s := range sessions {
		if s.Nudges > 0 || s.Notified {
			pending++
		}
	}
	if pending > 0 {
		fmt.Printf("             %d session(s) with pending nudge/notify\n", pending)
	}
}

func runDaemonStatus(cmd *cobra.Command, args []string) error {
	cfg := daemonConfig()
	state := daemon.LoadState(cfg.StatePath)
	svc := gatherService()

	fmt.Printf("%s proj-daemon; auto-resume after usage limits, recreate pinned sessions\n", svc.dot())

	if svc.exists {
		enabledStr := svc.unitFileState
		if enabledStr == "" {
			enabledStr = "unmanaged"
		}
		fmt.Printf("     Loaded: %s (%s)\n", svc.fragmentPath, enabledStr)

		active := svc.activeState
		if svc.subState != "" && svc.subState != svc.activeState {
			active = fmt.Sprintf("%s (%s)", svc.activeState, svc.subState)
		}
		since := ""
		if !svc.activeEnter.IsZero() {
			since = fmt.Sprintf(" since %s; %s ago",
				svc.activeEnter.Format("Mon 2006-01-02 15:04:05 MST"),
				formatAgo(time.Since(svc.activeEnter)))
		}
		fmt.Printf("     Active: %s%s\n", active, since)
		if svc.mainPID > 0 {
			fmt.Printf("   Main PID: %d (proj)\n", svc.mainPID)
		}
		if svc.memory > 0 {
			fmt.Printf("     Memory: %s\n", formatBytes(svc.memory))
		}
	} else if runtime.GOOS == "linux" {
		fmt.Printf("     Loaded: (not installed; `proj daemon enable` or use the nix module)\n")
	} else if runtime.GOOS == "darwin" {
		fmt.Println("     Loaded: (manage via `launchctl print gui/$UID/com.proj.daemon`)")
	}

	fmt.Printf("     Config: poll=%s  max_wait=%s  resume=%q\n",
		formatDur(cfg.Poll), formatDur(cfg.MaxWait), cfg.ResumeText)
	fmt.Printf("      State: %s\n", cfg.StatePath)
	if !svc.activeEnter.IsZero() && cfg.Poll > 0 {
		n := int64(time.Since(svc.activeEnter)/cfg.Poll) + 1
		next := svc.activeEnter.Add(time.Duration(n) * cfg.Poll)
		fmt.Printf("  Next tick: in %s (%s)\n", formatAgo(time.Until(next)), next.Format("15:04:05"))
	}
	renderManaged(cfg)
	renderOverseer()

	now := time.Now()
	deferredCount := 0
	for _, t := range state {
		if !t.NextAttempt.IsZero() && t.NextAttempt.After(now) {
			deferredCount++
		}
	}
	if len(state) > 0 {
		fmt.Println()
		fmt.Printf("  Tracked: %d (deferred: %d)\n", len(state), deferredCount)
		for _, t := range state {
			deferred := !t.NextAttempt.IsZero() && t.NextAttempt.After(now)
			marker := "\033[32m●\033[0m"
			status := "due next tick"
			if deferred {
				marker = "\033[33m●\033[0m"
				status = fmt.Sprintf("deferred until %s (in %s)",
					t.NextAttempt.Format("Mon 15:04:05 MST"),
					formatAgo(time.Until(t.NextAttempt)))
			}
			fmt.Printf("    %s %s [pane %s]\n", marker, t.Session, t.Pane)
			fmt.Printf("        banner:   %s\n", t.Banner)
			fmt.Printf("        seen for: %s · %d attempt(s)\n",
				formatAgo(now.Sub(t.FirstSeen)), t.Attempts)
			fmt.Printf("        next:     %s\n", status)
		}
	}

	if svc.exists && runtime.GOOS == "linux" {
		fmt.Println()
		out, _ := exec.Command("journalctl", "--user", "-u", "proj-daemon",
			"-n", "5", "--no-pager", "-o", "short").Output()
		if len(out) > 0 {
			fmt.Print(string(out))
		}
	}
	return nil
}

func runDaemonForeground(cmd *cobra.Command, args []string) error {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, nil)))
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	// The overseer runs as a per-tick pass; wiring it through daemon.PostTick
	// keeps the daemon package unaware of the overseer (which imports it). It
	// reloads config each look so `overseer on/off` takes effect without a
	// restart, and no-ops while disabled.
	daemon.PostTick = func(now time.Time) {
		cfg, err := config.Load()
		if err != nil {
			return
		}
		overseer.Pass(cfg, now)
	}
	return daemon.Run(ctx, daemonConfig())
}

func daemonConfig() daemon.Config {
	user, _ := config.Load()
	out := daemon.DefaultConfig()
	out.BaseDir = user.BaseDir
	out.Poll = config.Duration(user.Daemon.PollInterval, out.Poll)
	out.MaxWait = config.Duration(user.Daemon.MaxWait, out.MaxWait)
	if user.Daemon.ResumeText != "" {
		out.ResumeText = user.Daemon.ResumeText
	}
	if user.Daemon.CaptureLines > 0 {
		out.Capture = user.Daemon.CaptureLines
	}
	out.KeepAlive = user.Daemon.KeepAlive
	out.Tools = make(map[string]config.ToolSpec)
	for _, name := range user.ToolNames() {
		if spec, err := user.Tool(name); err == nil {
			out.Tools[name] = spec
		}
	}
	out.ClaudeHome = user.Claude.Home
	return out
}

func systemctlCmd(action, summary string) *cobra.Command {
	verb := strings.Fields(action)[0]
	return &cobra.Command{
		Use:   verb,
		Short: summary,
		RunE: func(cmd *cobra.Command, args []string) error {
			if runtime.GOOS != "linux" {
				return fmt.Errorf("systemctl-based actions are linux-only; on macOS use launchctl directly")
			}
			parts := append([]string{"--user"}, strings.Fields(action)...)
			parts = append(parts, "proj-daemon")
			return runForeground("systemctl", parts...)
		},
	}
}

func runForeground(bin string, args ...string) error {
	c := exec.Command(bin, args...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return c.Run()
}

// currentSessionName returns the tmux session name from the environment.
// Returns "" when not inside tmux.
func currentSessionName() string {
	if os.Getenv("TMUX") == "" {
		return ""
	}
	out, err := exec.Command("tmux", "display-message", "-p", "#S").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func runDaemonKeepAlive(cmd *cobra.Command, args []string) error {
	userCfg, err := config.Load()
	if err != nil {
		return err
	}
	if len(args) == 0 {
		if userCfg.Daemon.KeepAlive {
			fmt.Println("keep-alive: on")
		} else {
			fmt.Println("keep-alive: off")
		}
		return nil
	}
	switch args[0] {
	case "on":
		userCfg.Daemon.KeepAlive = true
	case "off":
		userCfg.Daemon.KeepAlive = false
	default:
		return fmt.Errorf("expected on or off, got %q", args[0])
	}
	if err := config.Write(userCfg); err != nil {
		return err
	}
	fmt.Printf("keep-alive: %s\n", args[0])
	return nil
}

func runDaemonMarkClosed(cmd *cobra.Command, args []string) error {
	name := args[0]
	cfg := daemonConfig()
	return daemon.UpdateManagedState(cfg.StatePath, func(managed daemon.ManagedState) error {
		ms := managed[name]
		ms.Name = name
		ms.ExitedCleanly = true
		managed[name] = ms
		return nil
	})
}
