package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/tieo/proj/internal/config"
	"github.com/tieo/proj/internal/unreset"
)

var unresetCmd = &cobra.Command{
	Use:   "unreset",
	Short: "auto-resume Claude Code sessions when usage limits expire",
	Long: `Polls tmux panes for Claude Code's usage-limit banner ("You're out of
extra usage · resets 3am"). On detection, sends Escape to dismiss the
/rate-limit-options selector (if present) and types "continue".

Run as a user service (` + "`proj unreset enable`" + `) or in the
foreground for debugging (` + "`proj unreset run`" + `).`,
	RunE: runUnresetStatus,
}

var unresetRunCmd = &cobra.Command{
	Use:   "run",
	Short: "run the daemon in foreground (service unit calls this)",
	RunE:  runUnresetDaemon,
}

var (
	unresetStartCmd   = systemctlCmd("start", "start the service")
	unresetStopCmd    = systemctlCmd("stop", "stop the service")
	unresetRestartCmd = systemctlCmd("restart", "restart the service")
	unresetEnableCmd  = systemctlCmd("enable --now", "enable and start the service")
	unresetDisableCmd = systemctlCmd("disable --now", "stop and disable the service")
	unresetLogsCmd    = &cobra.Command{
		Use:   "logs",
		Short: "tail the service logs",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runForeground("journalctl", "--user", "-u", "proj-unreset", "-f")
		},
	}
)

func init() {
	rootCmd.AddCommand(unresetCmd)
	unresetCmd.AddCommand(unresetRunCmd, unresetStartCmd, unresetStopCmd,
		unresetRestartCmd, unresetEnableCmd, unresetDisableCmd, unresetLogsCmd)
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
		"proj-unreset").Output()
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

func runUnresetStatus(cmd *cobra.Command, args []string) error {
	cfg := unresetConfig()
	state := unreset.LoadState(cfg.StatePath)
	svc := gatherService()

	fmt.Printf("%s proj-unreset — auto-resume Claude Code sessions after usage-limit cooldown\n", svc.dot())

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
		fmt.Printf("     Loaded: (not installed — `proj unreset enable` or use the nix module)\n")
	} else if runtime.GOOS == "darwin" {
		fmt.Println("     Loaded: (manage via `launchctl print gui/$UID/com.proj.unreset`)")
	}

	scan := unreset.ScanPanes(cfg.Capture)
	fmt.Printf("     Config: poll=%s  max_wait=%s  jitter=%s  resume=%q\n",
		formatDur(cfg.Poll), formatDur(cfg.MaxWait), formatDur(cfg.Jitter), cfg.ResumeText)
	fmt.Printf("      State: %s\n", cfg.StatePath)

	fmt.Println()
	fmt.Printf("  Watching %d session(s):\n", len(scan))
	for _, s := range scan {
		marker, color := "○", "\033[90m"
		switch s.Label() {
		case "banner", "banner + selector":
			marker, color = "●", "\033[31m"
		case "selector":
			marker, color = "●", "\033[33m"
		}
		fmt.Printf("    %s%s\033[0m %-22s %s\n", color, marker, s.Pane.Session, s.Label())
	}

	deferredCount := 0
	now := time.Now()
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
		out, _ := exec.Command("journalctl", "--user", "-u", "proj-unreset",
			"-n", "5", "--no-pager", "-o", "short").Output()
		if len(out) > 0 {
			fmt.Print(string(out))
		}
	}
	return nil
}

func runUnresetDaemon(cmd *cobra.Command, args []string) error {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, nil)))
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	return unreset.Run(ctx, unresetConfig())
}

func unresetConfig() unreset.Config {
	user, _ := config.Load()
	out := unreset.DefaultConfig()
	out.Poll = config.Duration(user.Unreset.PollInterval, out.Poll)
	out.MaxWait = config.Duration(user.Unreset.MaxWait, out.MaxWait)
	out.Jitter = config.Duration(user.Unreset.Jitter, out.Jitter)
	if user.Unreset.ResumeText != "" {
		out.ResumeText = user.Unreset.ResumeText
	}
	if user.Unreset.CaptureLines > 0 {
		out.Capture = user.Unreset.CaptureLines
	}
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
			parts = append(parts, "proj-unreset")
			return runForeground("systemctl", parts...)
		},
	}
}

func runForeground(bin string, args ...string) error {
	c := exec.Command(bin, args...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return c.Run()
}
