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
	Use:           "proj [name | <lang> <name> | <subcommand>]",
	Short:         "tmux + Claude Code project session manager",
	Long:          `proj opens a tmux session per project, optionally launching Claude Code inside it, and (via "proj unreset") auto-resumes those sessions when usage limits expire.`,
	Args:          cobra.MaximumNArgs(2),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runOpen,
}

var headless bool

func init() {
	rootCmd.PersistentFlags().BoolVar(&headless, "headless", false, "don't attach to the tmux session after opening")
	rootCmd.PersistentFlags().BoolVar(&listAll, "all", false, "show all projects regardless of age")
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
