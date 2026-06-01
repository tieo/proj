package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tieo/proj/internal/config"
	"github.com/tieo/proj/internal/projects"
	"github.com/tieo/proj/internal/tmux"
)

var tagCmd = &cobra.Command{
	Use:   "tag",
	Short: "manage a project's tags",
}

var tagAddCmd = &cobra.Command{
	Use:   "add <name> <tag>...",
	Short: "add tags to a project",
	Args:  cobra.MinimumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return mutateTags(args[0], func(current []string) []string {
			return append(current, args[1:]...)
		})
	},
}

var tagRmCmd = &cobra.Command{
	Use:   "rm <name> <tag>...",
	Short: "remove tags from a project",
	Args:  cobra.MinimumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		toRemove := make(map[string]struct{}, len(args)-1)
		for _, t := range args[1:] {
			toRemove[t] = struct{}{}
		}
		return mutateTags(args[0], func(current []string) []string {
			out := current[:0]
			for _, t := range current {
				if _, drop := toRemove[t]; !drop {
					out = append(out, t)
				}
			}
			return out
		})
	},
}

var tagSetCmd = &cobra.Command{
	Use:   "set <name> [<tag>...]",
	Short: "replace a project's tags",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return mutateTags(args[0], func(_ []string) []string {
			return args[1:]
		})
	},
}

// mutateTags loads the project, applies fn to its current tags, persists the
// result, and renames the tmux session (if any) so its name reflects the new
// tags.
func mutateTags(name string, fn func([]string) []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	p, err := projects.FindByName(cfg.BaseDir, name)
	if err != nil {
		return err
	}
	reg, err := projects.LoadRegistry()
	if err != nil {
		return err
	}
	oldSession := projects.SessionName(p.Name, p.Tags)
	newTags := fn(p.Tags)
	if err := reg.SetTags(p.Name, newTags); err != nil {
		return err
	}
	stored := reg.Tags(p.Name)
	newSession := projects.SessionName(p.Name, stored)
	if oldSession != newSession {
		_ = tmux.RenameSession(oldSession, newSession)
	}
	if len(stored) == 0 {
		fmt.Printf("%s: (no tags)\n", p.Name)
	} else {
		fmt.Printf("%s: %s\n", p.Name, strings.Join(stored, " "))
	}
	return nil
}

func init() {
	tagCmd.AddCommand(tagAddCmd, tagRmCmd, tagSetCmd)
	rootCmd.AddCommand(tagCmd)
}
