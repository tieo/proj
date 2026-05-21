package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
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
extra usage · resets 3am"). When the reset time passes, sends "continue"
to the pane so the session picks up where it left off.

Run as a background service (` + "`proj unreset enable`" + `) or in the
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

func runUnresetStatus(cmd *cobra.Command, args []string) error {
	cfg := unresetConfig()
	state := unreset.LoadState(cfg.StatePath)
	fmt.Printf("state file:        %s\n", cfg.StatePath)
	fmt.Printf("poll interval:     %s\n", cfg.Poll)
	fmt.Printf("max wait:          %s\n", cfg.MaxWait)
	fmt.Printf("tracked sessions:  %d\n", len(state))
	if len(state) > 0 {
		fmt.Println()
		now := time.Now()
		for _, t := range state {
			seenFor := now.Sub(t.FirstSeen).Truncate(time.Second)
			fmt.Printf("  %s [pane %s]\n", t.Session, t.Pane)
			fmt.Printf("    banner:     %s\n", t.Banner)
			fmt.Printf("    seen for:   %s\n", seenFor)
			fmt.Printf("    attempts:   %d\n", t.Attempts)
			if !t.NextAttempt.IsZero() {
				until := time.Until(t.NextAttempt).Truncate(time.Second)
				when := t.NextAttempt.Local().Format("Mon 15:04 MST")
				if until < 0 {
					fmt.Printf("    next try:   due (next tick)\n")
				} else {
					fmt.Printf("    next try:   %s (in %s)\n", when, until)
				}
			}
		}
	}
	fmt.Println()
	fmt.Println("service:")
	switch runtime.GOOS {
	case "linux":
		out, _ := exec.Command("systemctl", "--user", "is-active", "proj-unreset").Output()
		s := strings.TrimSpace(string(out))
		if s == "" {
			s = "(not installed)"
		}
		fmt.Printf("  %s\n", s)
	case "darwin":
		fmt.Println("  use `launchctl print gui/$UID/com.proj.unreset`")
	default:
		fmt.Println("  (service management not supported on this OS)")
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
