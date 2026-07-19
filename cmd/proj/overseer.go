package main

import (
	"fmt"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/tieo/proj/internal/config"
	"github.com/tieo/proj/internal/overseer"
)

// overseerCmd manages the daemon's fleet overseer, mirroring the keep-alive
// command: no argument shows status, on/off toggles [daemon.overseer].enabled
// in config.toml. Model, interval, and the other knobs are config.toml fields.
// Registered under daemonCmd in daemon.go.
var overseerCmd = &cobra.Command{
	Use:   "overseer [on|off]",
	Short: "show or set the fleet overseer, or run one look (overseer run)",
	Long: `Show or set the daemon's fleet overseer.

With no argument, prints whether the overseer is enabled and its settings. "on"
and "off" toggle [daemon.overseer].enabled in config.toml; the daemon picks the
change up on its next tick. Other settings (model, interval, max_nudges,
max_tokens, ntfy_topic) are edited directly under [daemon.overseer] in
config.toml.

The overseer reads each idle session's recent transcript and judges whether it
reached its goal or stopped short. "overseer run" does one look now and prints
the verdicts plus token usage, without acting - the dry-run for validating it
before enabling.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runOverseer,
}

var overseerRunCmd = &cobra.Command{
	Use:   "run",
	Short: "run one overseer look now and print the verdicts (takes no action)",
	Args:  cobra.NoArgs,
	RunE:  runOverseerRun,
}

func init() {
	overseerCmd.AddCommand(overseerRunCmd)
}

func runOverseer(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ov := &cfg.Daemon.Overseer
	if len(args) == 0 {
		printOverseerReport(cfg)
		return nil
	}
	switch args[0] {
	case "on":
		ov.Enabled = true
	case "off":
		ov.Enabled = false
	default:
		return fmt.Errorf("expected on or off, got %q", args[0])
	}
	if err := config.Write(cfg); err != nil {
		return err
	}
	fmt.Printf("overseer: %s\n", args[0])
	return nil
}

func runOverseerRun(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	res, err := overseer.Look(cfg, nil)
	if err != nil {
		return err
	}
	if len(res.Sessions) == 0 {
		fmt.Println("no readable sessions to judge")
		return nil
	}

	for _, v := range res.Verdicts {
		fmt.Printf("  %-24s %-13s %s\n", v.Name, v.State, v.Goal)
		if v.State == "stopped_short" && v.Callout != "" {
			fmt.Printf("      → %s\n", v.Callout)
		}
		if v.NeedsUser {
			fmt.Printf("      ! needs you: %s\n", v.UserReason)
		}
	}
	if len(res.Verdicts) == 0 {
		fmt.Printf("  (overseer returned no parseable verdicts)\n  raw: %.300s\n", res.Raw)
	}

	u := res.Usage
	fmt.Printf("usage: judged=%d input=%d output=%d cache_read=%d cache_create=%d  ~effective=%d\n",
		len(res.Sessions), u.Input, u.Output, u.CacheRead, u.CacheCreate, overseer.Effective(u))
	if u.CacheRead < 15000 {
		fmt.Printf("note: cache_read=%d low — the system/CLAUDE.md prefix did not cache (cold or evicted this look)\n", u.CacheRead)
	}

	if err := overseer.LogUsage(time.Now(), res); err != nil {
		fmt.Printf("warning: could not log usage: %v\n", err)
	}
	return nil
}

// printOverseerReport is the no-argument `proj daemon overseer` view: the
// enabled state and settings, then what the overseer has actually done -
// its last look, today's token spend against the budget, the warmth of the
// last call's cache, per-session nudge memory, and a tail of recent looks.
func printOverseerReport(cfg config.Config) {
	ov := cfg.Daemon.Overseer
	state := "off"
	if ov.Enabled {
		state = "on"
	}
	fmt.Printf("overseer: %s   model=%s interval=%s max_nudges=%d max_tokens=%d ntfy=%s\n",
		state, ov.Model, ov.Interval, ov.MaxNudges, ov.MaxTokens, orNone(ov.NtfyTopic))

	recs := overseer.ReadUsageLog()
	lastLook, sessions := overseer.ReadLookState()
	if len(recs) == 0 && lastLook.IsZero() {
		fmt.Println("  no looks recorded yet — run one with `proj daemon overseer run`")
		return
	}
	now := time.Now()

	if !lastLook.IsZero() {
		fmt.Printf("  last look:  %s (%s ago)\n", lastLook.Format("Mon 15:04:05"), formatAgo(now.Sub(lastLook)))
	}
	looks, eff := overseer.TodayUsage(recs, now)
	pct := 0
	if overseer.DayBudget > 0 {
		pct = eff * 100 / overseer.DayBudget
	}
	fmt.Printf("  today:      %d looks · ~%s effective / %s budget (%d%%)\n",
		looks, formatK(eff), formatK(overseer.DayBudget), pct)

	if n := len(recs); n > 0 {
		last := recs[n-1]
		warm := "cold — prefix not cached"
		if last.CacheRead >= 15000 {
			warm = "warm"
		}
		fmt.Printf("  last usage: judged=%d in=%d out=%d cache_read=%s cache_create=%s ~eff=%s (%s)\n",
			last.Judged, last.Input, last.Output,
			formatK(last.CacheRead), formatK(last.CacheCreate), formatK(last.Effective()), warm)
	}

	if len(sessions) > 0 {
		pending := 0
		for _, s := range sessions {
			if s.Nudges > 0 || s.Notified {
				pending++
			}
		}
		fmt.Printf("  sessions:   %d tracked", len(sessions))
		if pending > 0 {
			fmt.Printf(", %d with pending nudge/notify", pending)
		}
		fmt.Println()
		for _, s := range sessions {
			if s.Nudges == 0 && !s.Notified {
				continue
			}
			flag := ""
			if s.Notified {
				flag = " · user notified"
			}
			fmt.Printf("    ● %-22s nudges %d/%d%s\n", s.Name, s.Nudges, ov.MaxNudges, flag)
		}
	}

	fmt.Println("  recent looks:")
	start := len(recs) - 8
	if start < 0 {
		start = 0
	}
	for _, r := range recs[start:] {
		fmt.Printf("    %s  judged %-2d  eff %-6s read %-6s create %s\n",
			r.At.Format("01-02 15:04"), r.Judged, formatK(r.Effective()), formatK(r.CacheRead), formatK(r.CacheCreate))
	}
}

// formatK abbreviates a token count: 940, 3.2k, 122k.
func formatK(n int) string {
	switch {
	case n < 1000:
		return strconv.Itoa(n)
	case n < 10000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	default:
		return fmt.Sprintf("%dk", n/1000)
	}
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}
