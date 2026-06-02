// Command proj is the tmux + Claude Code project session manager.
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
	Use:   "proj [name-or-prefix | <subcommand>]",
	Short: "tmux + Claude Code project session manager",
	Long: `proj opens a tmux session per project, optionally launching Claude Code inside it, and (via "proj daemon") auto-resumes those sessions when usage limits expire.

Each project is a uniquely-named directory under base_dir; open one by its name or a unique prefix. Tags are labels for grouping and never affect identity.`,
	Args:          cobra.ArbitraryArgs,
	SilenceUsage:  true,
	SilenceErrors: true,
	// Flag and argument validation run before any PreRun, so reaching here means
	// the command actually started: a later error is a runtime one, and main
	// should not bury it under a usage dump.
	PersistentPreRun: func(cmd *cobra.Command, args []string) { cmdStarted = true },
	RunE:             runOpen,
}

var (
	headless   bool
	cmdStarted bool
)

func init() {
	rootCmd.PersistentFlags().BoolVar(&headless, "headless", false, "don't attach to the tmux session after opening")
	rootCmd.PersistentFlags().BoolVar(&listAll, "all", false, "show all projects regardless of age")
}

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
