package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/tieo/proj/internal/config"
	"github.com/tieo/proj/internal/doner"
	"github.com/tieo/proj/internal/paseo"
)

// The Stop hook decides whether a session may stop. This is the other half:
// what happens to one that already stopped and should not have stayed stopped.
//
// A session that answers "Waiting on <id>" is let go deliberately, because
// holding it would spin it through turns against a job it cannot hurry. The
// nudge promises it will be asked again in half an hour, and something has to
// keep that promise. Nothing did between the move to Paseo and this: the old
// backstop read tmux panes, and there are none, so a session that reported
// itself waiting waited until a person noticed. One sat for 74 minutes on a
// test run that had already finished.
//
// A hook cannot do this job. It only runs when a session stops, and the whole
// problem is a session that is not running at all.

var (
	sweepDryRun bool
	sweepAfter  time.Duration
	sweepCmd    = &cobra.Command{
		Use:   "sweep",
		Short: "nudge doner-tagged sessions that have been stopped too long",
		Long: `Ask again any doner-tagged session that stopped without reporting done and
has been quiet for longer than the wait window.

Run from a timer. The Stop hook handles a session that is trying to stop; this
handles one that already did.`,
		Args: cobra.NoArgs,
		RunE: runSweep,
	}
)

func init() {
	sweepCmd.Flags().BoolVar(&sweepDryRun, "dry-run", false, "report what would be nudged without sending anything")
	// The window is a flag so the sweep can be exercised on demand: with the
	// half-hour default, a run proves nothing until something has been stuck
	// for half an hour.
	sweepCmd.Flags().DurationVar(&sweepAfter, "after", doner.WaitWindow, "how long a session must have been quiet")
	donerCmd.AddCommand(sweepCmd)
}

// sweepStampPath records when each agent was last nudged, so a session that
// does not answer is not nudged again on every tick.
func sweepStampPath() string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "proj", "doner-sweep.json")
}

func loadStamps() map[string]time.Time {
	out := map[string]time.Time{}
	data, err := os.ReadFile(sweepStampPath())
	if err != nil {
		return out
	}
	raw := map[string]string{}
	if json.Unmarshal(data, &raw) != nil {
		return out
	}
	for k, v := range raw {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			out[k] = t
		}
	}
	return out
}

func saveStamps(s map[string]time.Time) {
	raw := map[string]string{}
	for k, v := range s {
		raw[k] = v.Format(time.RFC3339)
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return
	}
	p := sweepStampPath()
	if os.MkdirAll(filepath.Dir(p), 0o755) != nil {
		return
	}
	_ = os.WriteFile(p, data, 0o644)
}

func runSweep(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if !cfg.Daemon.Doner.Active() {
		return nil
	}
	if !paseo.Available() {
		return nil
	}
	root := claudeRoot(cfg.Claude.Home)
	home := paseo.Home()
	stamps := loadStamps()
	now := time.Now()
	nudged := 0

	for _, a := range paseo.Agents() {
		dir := a.Dir()
		if !a.Idle() || !projectHasDonerTag(dir) {
			continue
		}
		sid := paseo.SessionID(home, a.ID, dir)
		if sid == "" {
			continue
		}
		reply, at, err := doner.LastReply(doner.TranscriptPath(root, dir, sid))
		if err != nil || at.IsZero() {
			continue
		}
		quiet := now.Sub(at)
		if quiet < sweepAfter {
			continue
		}
		// The two answers the nudge asks for are the two it must respect. A
		// refused turn is left alone for the same reason the hook leaves it
		// alone: asking again only refuses again.
		if doner.IsDone(reply) || doner.IsAPIError(reply) {
			continue
		}
		// Nudging again before the session has said anything new would be
		// talking over the last nudge rather than following it up.
		if last, ok := stamps[a.ID]; ok && !last.Before(at) {
			continue
		}
		why := "stopped without reporting done"
		if doner.IsWaiting(reply) {
			why = "waiting"
		}
		fmt.Printf("%s: %s for %s\n", lastPathSegment(dir), why, quiet.Round(time.Minute))
		if sweepDryRun {
			continue
		}
		if err := paseo.Send(a.ID, doner.Reason); err != nil {
			fmt.Fprintf(os.Stderr, "  %s: %v\n", lastPathSegment(dir), err)
			continue
		}
		stamps[a.ID] = now
		nudged++
	}
	if !sweepDryRun {
		saveStamps(stamps)
	}
	if nudged > 0 {
		fmt.Printf("nudged %d session(s)\n", nudged)
	}
	return nil
}
