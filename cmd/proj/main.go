// Command proj carries doner: a Claude Code Stop hook that keeps a tagged
// project's session working until it reports that it is finished.
//
// Sessions themselves live in Paseo, which starts, holds and shows them. Doner
// is the one thing Paseo has no answer for, because it is a decision made at
// the moment a session would stop, inside Claude Code, where only a hook runs.
//
// See `proj --help` for usage. Subcommands live in sibling files of this
// package and register themselves with `rootCmd` in their init().
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

const Version = "0.1.0"

var rootCmd = &cobra.Command{
	Use:   "proj <subcommand>",
	Short: "keep a tagged session working until it reports done",
	Long: `proj holds doner: a Claude Code Stop hook that reads a session's last reply
when it would stop, and sends it back to work unless it reported that it is
finished, blocked, waiting, or needs an answer from you.

The control surface is the "doner" tag. Tag a project and its sessions are
held; untag it and they stop when they like.`,
	SilenceUsage:  true,
	SilenceErrors: true,
	// Flag and argument validation run before any PreRun, so reaching here means
	// the command actually started: a later error is a runtime one, and main
	// should not bury it under a usage dump.
	PersistentPreRun: func(cmd *cobra.Command, args []string) { cmdStarted = true },
}

var cmdStarted bool

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "print the version",
	Args:  cobra.NoArgs,
	Run:   func(*cobra.Command, []string) { fmt.Println("proj", Version) },
}

func init() { rootCmd.AddCommand(versionCmd) }

func main() {
	cmd, err := rootCmd.ExecuteC()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		// A usage error (bad/missing args, unknown subcommand or flag) is reported
		// before the command starts; follow it with that command's help so the
		// user can see what it expects. Runtime errors get just the message.
		if !cmdStarted {
			fmt.Fprintln(os.Stderr)
			cmd.SetOut(os.Stderr)
			_ = cmd.Help()
		}
		os.Exit(1)
	}
}
