package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/tieo/proj/internal/config"
	"github.com/tieo/proj/internal/projects"
)

var newCmd = &cobra.Command{
	Use:   "new <name> <tags...>",
	Short: "create a project; the first argument is the name, any following ones are tags",
	Long: "Create a project. The first argument is always the project name; every\n" +
		"argument after it is a tag. Names must be unique: each project is its own\n" +
		"directory, and you open one by its name (or a unique prefix). Tags are just\n" +
		"labels for grouping and may be shared across projects. Quote a multi-word name:\n\n" +
		"  proj new webapp                 # untagged project \"webapp\"\n" +
		"  proj new webapp go oss          # name \"webapp\", tags [go oss]\n" +
		"  proj new \"client app\" work go   # name \"client app\", tags [work go]",
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		name := args[0]
		tags := args[1:]
		if err := projects.ValidateName(name); err != nil {
			return err
		}
		for _, t := range tags {
			if err := projects.ValidateTag(t); err != nil {
				return err
			}
		}
		if newAgentF != "" {
			if _, err := cfg.Agent(newAgentF); err != nil {
				return err
			}
		}

		exists, err := projects.CheckNewName(cfg.BaseDir, name)
		if err != nil {
			return err
		}
		if exists {
			return fmt.Errorf("%q already exists; use `proj tag add %s ...` to add tags", name, name)
		}
		dir := filepath.Join(cfg.BaseDir, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		reg, err := projects.LoadRegistry()
		if err != nil {
			return err
		}
		if err := reg.SetTags(name, tags); err != nil {
			return err
		}
		if newAgentF != "" {
			if err := reg.SetAgent(name, newAgentF); err != nil {
				return err
			}
		}
		p, err := projects.FindByName(cfg.BaseDir, name)
		if err != nil {
			return err
		}
		return openInTmux(cfg, p)
	},
}

var newAgentF string

func init() {
	newCmd.Flags().StringVar(&newAgentF, "agent", "", "coding agent for the project's sessions (claude, codex, agy, or a [agents.*] entry)")
	rootCmd.AddCommand(newCmd)
}
