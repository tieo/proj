package main

import (
	"fmt"
	"strconv"
	"strings"
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

// ANSI styles for the overseer report.
const (
	aReset  = "\033[0m"
	aBold   = "\033[1m"
	aDim    = "\033[90m"
	aGreen  = "\033[32m"
	aYellow = "\033[33m"
	aRed    = "\033[31m"
	aCyan   = "\033[36m"
)

// printOverseerReport is the no-argument `proj daemon overseer` view: a
// human-readable dashboard of the fleet judge - whether it's on, what it does,
// today's token spend against the budget, when it last looked, the cost pattern
// of recent looks, and which sessions it is currently acting on.
func printOverseerReport(cfg config.Config) {
	ov := cfg.Daemon.Overseer
	now := time.Now()
	recs := overseer.ReadUsageLog()
	lastLook, sessions := overseer.ReadLookState()

	badge := aDim + "○ off" + aReset
	if ov.Enabled {
		badge = aGreen + aBold + "● on" + aReset
	}
	fmt.Println()
	fmt.Printf("  %s⬢ Overseer%s  %s   %smodel %s · every %s · budget %s/day%s\n",
		aBold, aReset, badge, aDim, ov.Model, ov.Interval, formatK(overseer.DayBudget), aReset)
	fmt.Printf("  %sNudges idle sessions that stopped short of their goal; pings you only\n  when a decision genuinely needs you.%s\n\n", aDim, aReset)

	if len(recs) == 0 && lastLook.IsZero() {
		how := "run one now with `proj daemon overseer run`"
		if ov.Enabled {
			how = "the daemon will look on its next round of new work"
		}
		fmt.Printf("  %sNo looks yet — %s.%s\n\n", aDim, how, aReset)
		return
	}

	// Budget bar.
	looks, eff := overseer.TodayUsage(recs, now)
	pct := 0
	if overseer.DayBudget > 0 {
		pct = eff * 100 / overseer.DayBudget
	}
	fmt.Printf("  %-13s %s  %s%d%%%s   %s of %s tokens\n",
		"Budget today", budgetBar(pct, 22), budgetColor(pct), pct, aReset,
		formatK(eff), formatK(overseer.DayBudget))
	fmt.Printf("  %-13s %d\n", "Looks today", looks)

	// Last look, with cache warmth in plain words.
	if !lastLook.IsZero() {
		warm := aYellow + "cold, prefix rebuilt" + aReset
		if n := len(recs); n > 0 && recs[n-1].CacheRead >= 15000 {
			warm = aGreen + "warm, reads cheap" + aReset
		}
		judged := ""
		if n := len(recs); n > 0 {
			judged = fmt.Sprintf(" · %d judged", recs[n-1].Judged)
		}
		fmt.Printf("  %-13s %s %s(%s ago)%s%s · %s\n",
			"Last look", lastLook.Format("15:04"), aDim, formatAgo(now.Sub(lastLook)), aReset, judged, warm)
	}

	// Cost pattern of recent looks as a sparkline, newest on the right.
	if effs := recentEffective(recs, 16); len(effs) > 0 {
		fmt.Printf("  %-13s %s%s%s  %s%s last%s\n",
			"Cost/look", aCyan, sparkline(effs), aReset, aDim, formatK(effs[len(effs)-1]), aReset)
	}

	// Sessions the overseer is currently acting on. Ones it has judged on track
	// carry no nudge memory and are omitted; the interesting rows are the ones it
	// nudged or pinged about.
	var flagged []overseer.SessionMemory
	for _, s := range sessions {
		if s.Nudges > 0 || s.Notified {
			flagged = append(flagged, s)
		}
	}
	fmt.Println()
	if len(flagged) == 0 {
		fmt.Printf("  %sFlagged now%s   %snone — every session on track or still working%s\n",
			aBold, aReset, aDim, aReset)
	} else {
		fmt.Printf("  %sFlagged now%s\n", aBold, aReset)
		for _, s := range flagged {
			state := fmt.Sprintf("%snudged %d/%d, still short%s", aYellow, s.Nudges, ov.MaxNudges, aReset)
			if s.Notified {
				state = aRed + "waiting on you" + aReset
			}
			fmt.Printf("    ● %-22s %s\n", s.Name, state)
		}
	}
	fmt.Println()
}

// budgetBar draws a width-cell meter filled to pct, coloured by headroom.
func budgetBar(pct, width int) string {
	if pct < 0 {
		pct = 0
	}
	fill := pct * width / 100
	if fill > width {
		fill = width
	}
	return budgetColor(pct) + strings.Repeat("█", fill) + aDim + strings.Repeat("░", width-fill) + aReset
}

func budgetColor(pct int) string {
	switch {
	case pct >= 90:
		return aRed
	case pct >= 70:
		return aYellow
	default:
		return aGreen
	}
}

// sparkline renders values as eighth-block bars, scaled between their own min
// and max.
func sparkline(vals []int) string {
	if len(vals) == 0 {
		return ""
	}
	blocks := []rune("▁▂▃▄▅▆▇█")
	min, max := vals[0], vals[0]
	for _, v := range vals {
		if v < min {
			min = v
		}
		if v > max {
			max = v
		}
	}
	var b strings.Builder
	for _, v := range vals {
		idx := 0
		if max > min {
			idx = (v - min) * (len(blocks) - 1) / (max - min)
		}
		b.WriteRune(blocks[idx])
	}
	return b.String()
}

// recentEffective returns the effective-token cost of up to the last n looks,
// oldest first.
func recentEffective(recs []overseer.UsageRecord, n int) []int {
	start := len(recs) - n
	if start < 0 {
		start = 0
	}
	out := make([]int, 0, len(recs)-start)
	for _, r := range recs[start:] {
		out = append(out, r.Effective())
	}
	return out
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
