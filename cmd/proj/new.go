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
	Use:   "new <tags...> <name>",
	Short: "create a project; the last argument is the name, any preceding ones are tags",
	Long: "Create a project. The final argument is always the project name; every\n" +
		"argument before it is a tag. Quote a multi-word name:\n\n" +
		"  proj new webapp                 # untagged project \"webapp\"\n" +
		"  proj new go oss webapp          # tags [go oss], name \"webapp\"\n" +
		"  proj new work go \"client app\"   # tags [work go], name \"client app\"",
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		name := args[len(args)-1]
		tags := args[:len(args)-1]
		if err := projects.ValidateName(name); err != nil {
			return err
		}

		dir := filepath.Join(cfg.BaseDir, name)
		if _, err := os.Stat(dir); err == nil {
			return fmt.Errorf("%q already exists; use `proj tag add %s ...` to add tags", name, name)
		}
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
		p, err := projects.FindByName(cfg.BaseDir, name)
		if err != nil {
			return err
		}
		return openInTmux(cfg, p)
	},
}

func init() {
	rootCmd.AddCommand(newCmd)
}
