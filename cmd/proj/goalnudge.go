package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/tieo/proj/internal/config"
	"github.com/tieo/proj/internal/daemon"
	"github.com/tieo/proj/internal/goalnudge"
)

// goalnudgeCmd manages the daemon's goal-nudge backstop, mirroring the
// keep-alive command: no argument shows the status report, on/off toggles
// [daemon.goal_nudge].enabled in config.toml. Registered under daemonCmd.
var goalnudgeCmd = &cobra.Command{
	Use:   "goal-nudge [on|off]",
	Short: "show the goal-nudge backstop, or turn it on/off (goal-nudge run for a dry-run)",
	Long: `Show or set the daemon's goal-nudge backstop.

With no argument, prints goal-nudge's status: whether it's on, today's token
spend against the budget, and each watched session's judged state. "on" and
"off" toggle [daemon.goal_nudge].enabled in config.toml; the daemon picks the
change up on its next tick. Other settings (model, max_nudges, max_tokens,
ntfy_topic) are edited directly under [daemon.goal_nudge] in config.toml.

A /goal set on a session is Claude Code's own auto-continue loop. Goal-nudge
only backstops it: when the goal fails to re-drive an idle session within its
fire grace, it judges the session (done / stopped_short / blocked / working) and
nudges the ones that stopped short. It never touches a session with no open
/goal. "goal-nudge run" judges every session once now and prints the verdicts,
without acting - the dry-run for validating it.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runGoalNudge,
}

var goalnudgeRunCmd = &cobra.Command{
	Use:   "run",
	Short: "run one goal-nudge look now and print the verdicts (takes no action)",
	Args:  cobra.NoArgs,
	RunE:  runGoalNudgeRun,
}

func init() {
	goalnudgeCmd.AddCommand(goalnudgeRunCmd)
}

func runGoalNudge(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ov := &cfg.Daemon.GoalNudge
	if len(args) == 0 {
		printGoalNudgeReport(cfg)
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
	fmt.Printf("goal-nudge: %s\n", args[0])
	return nil
}

func runGoalNudgeRun(cmd *cobra.Command, args []string) error {
	res, err := daemon.GoalNudgeDryRun(daemonConfig())
	if err != nil {
		return err
	}
	if len(res.Sessions) == 0 {
		fmt.Println("no readable sessions to judge")
		return nil
	}

	for _, v := range res.Verdicts {
		fmt.Printf("  %-24s %-13s %s\n", v.Name, v.State, v.Goal)
		if v.Reason != "" {
			fmt.Printf("      %swhy: %s%s\n", aDim, v.Reason, aReset)
		}
		if v.State == "stopped_short" && v.Callout != "" {
			fmt.Printf("      → %s\n", v.Callout)
		}
		if v.NeedsUser {
			fmt.Printf("      ! needs you: %s\n", v.UserReason)
		}
	}
	if len(res.Verdicts) == 0 {
		fmt.Printf("  (goal-nudge returned no parseable verdicts)\n  raw: %.300s\n", res.Raw)
	}

	// A dry-run leaves no trace: the usage is printed for the operator but not
	// logged, so it never counts against the budget the live goalnudge reports.
	u := res.Usage
	fmt.Printf("usage: judged=%d input=%d output=%d cache_read=%d cache_create=%d  ~effective=%d (not logged)\n",
		len(res.Sessions), u.Input, u.Output, u.CacheRead, u.CacheCreate, goalnudge.Effective(u))
	if u.CacheRead < 15000 {
		fmt.Printf("note: cache_read=%d low — the system/CLAUDE.md prefix did not cache (cold or evicted this look)\n", u.CacheRead)
	}
	return nil
}

// ANSI styles for the goalnudge report.
const (
	aReset  = "\033[0m"
	aBold   = "\033[1m"
	aDim    = "\033[90m"
	aGreen  = "\033[32m"
	aYellow = "\033[33m"
	aRed    = "\033[31m"
	aCyan   = "\033[36m"
)

// printGoalNudgeReport is the no-argument `proj daemon goalnudge` view: a
// human-readable dashboard of the fleet judge - whether it's on, what it does,
// today's token spend against the budget, when it last looked, the cost pattern
// of recent looks, and which sessions it is currently acting on.
func printGoalNudgeReport(cfg config.Config) {
	ov := cfg.Daemon.GoalNudge
	now := time.Now()
	recs := goalnudge.ReadUsageLog()
	lastLook, sessions := goalnudge.ReadLookState()

	badge := aDim + "○ off" + aReset
	if ov.Active() {
		badge = aGreen + aBold + "● on" + aReset
	}
	fmt.Println()
	fmt.Printf("  %s⬢ Goal-nudge%s  %s   %smodel %s · budget %s/day%s\n",
		aBold, aReset, badge, aDim, ov.Model, formatK(goalnudge.DayBudget), aReset)
	fmt.Printf("  %sAs each session goes idle, judges whether it hit its goal; nudges the\n  ones that stopped short, pings you only when a decision needs you.%s\n\n", aDim, aReset)

	if len(recs) == 0 && lastLook.IsZero() {
		how := "run one now with `proj daemon goal-nudge run`"
		if ov.Active() {
			how = "the daemon will look on its next round of new work"
		}
		fmt.Printf("  %sNo looks yet — %s.%s\n\n", aDim, how, aReset)
		return
	}

	// Budget bar.
	looks, eff := goalnudge.TodayUsage(recs, now)
	pct := 0
	if goalnudge.DayBudget > 0 {
		pct = eff * 100 / goalnudge.DayBudget
	}
	fmt.Printf("  %-13s %s  %s%d%%%s   %s of %s tokens\n",
		"Budget today", budgetBar(pct, 22), budgetColor(pct), pct, aReset,
		formatK(eff), formatK(goalnudge.DayBudget))
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

	// Every judged session with the state the goalnudge last gave it and the goal
	// it inferred, so the whole fleet's status is visible at a glance.
	fmt.Println()
	if len(sessions) == 0 {
		fmt.Printf("  %sSessions%s   %snone judged yet%s\n", aBold, aReset, aDim, aReset)
	} else {
		fmt.Printf("  %sSessions%s\n", aBold, aReset)
		for _, s := range sessions {
			glyph, label := stateBadge(s.State)
			note := ""
			if s.Nudges > 0 {
				note = fmt.Sprintf("  %snudged %d/%d%s", aDim, s.Nudges, ov.MaxNudges, aReset)
			}
			if s.Notified {
				note = "  " + aRed + "waiting on you" + aReset
			}
			fmt.Printf("    %s %-8s %-20s %s%.58s%s%s\n",
				glyph, label, s.Name, aDim, s.Goal, aReset, note)
			if s.Reason != "" {
				fmt.Printf("      %s%.72s%s\n", aDim, s.Reason, aReset)
			}
		}
	}
	fmt.Println()
}

// stateBadge maps a judged state to a coloured glyph and a short label.
func stateBadge(state string) (glyph, label string) {
	switch state {
	case "working":
		return aCyan + "●" + aReset, "working"
	case "done":
		return aGreen + "✓" + aReset, "done"
	case "stopped_short":
		return aYellow + "▲" + aReset, "short"
	case "blocked":
		return aRed + "■" + aReset, "blocked"
	case goalnudge.StateNoGoal:
		return aDim + "·" + aReset, "no goal"
	case goalnudge.StateWaitGoal:
		return aDim + "◦" + aReset, "goal set"
	default:
		return aDim + "·" + aReset, "-"
	}
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
func recentEffective(recs []goalnudge.UsageRecord, n int) []int {
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
