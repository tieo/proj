package main

import (
	"fmt"
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
		state := "off"
		if ov.Enabled {
			state = "on"
		}
		fmt.Printf("overseer: %s  (model=%s interval=%s max_nudges=%d max_tokens=%d)\n",
			state, ov.Model, ov.Interval, ov.MaxNudges, ov.MaxTokens)
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
	// Effective tokens against the plan: fresh input + output at full weight,
	// cache reads at 0.1x, cache writes at ~1.25x. The prefix (Claude Code system
	// + CLAUDE.md) is warm when cache_read is high; cache_create is dominated by
	// the fresh snapshot, which caching cannot avoid.
	eff := u.Input + u.Output + u.CacheRead/10 + u.CacheCreate*5/4
	fmt.Printf("usage: judged=%d input=%d output=%d cache_read=%d cache_create=%d  ~effective=%d\n",
		len(res.Sessions), u.Input, u.Output, u.CacheRead, u.CacheCreate, eff)
	if u.CacheRead < 15000 {
		fmt.Printf("note: cache_read=%d low — the system/CLAUDE.md prefix did not cache (cold or evicted this look)\n", u.CacheRead)
	}

	if err := overseer.LogUsage(time.Now(), res); err != nil {
		fmt.Printf("warning: could not log usage: %v\n", err)
	}
	return nil
}
