package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tieo/proj/internal/config"
	"github.com/tieo/proj/internal/projects"
)

var newCmd = &cobra.Command{
	Use:   "new",
	Short: "interactive new-project wizard",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		r := bufio.NewReader(os.Stdin)
		ask := func(prompt string) (string, error) {
			fmt.Print(prompt)
			line, err := r.ReadString('\n')
			return strings.TrimSpace(line), err
		}

		name, _ := ask("Project name? ")
		if name == "" {
			return fmt.Errorf("name required")
		}
		dir := filepath.Join(cfg.BaseDir, name)
		if _, err := os.Stat(dir); err == nil {
			return fmt.Errorf("%s already exists", dir)
		}

		existing := projects.ExistingTags(cfg.BaseDir)
		hint := ""
		if len(existing) > 0 {
			hint = fmt.Sprintf(" (existing: %s)", strings.Join(existing, " "))
		}
		raw, _ := ask(fmt.Sprintf("Tags?%s ", hint))
		tags := splitTags(raw)

		ans, _ := ask(fmt.Sprintf("Create %s? [Y/n] ", dir))
		ans = strings.ToLower(ans)
		if ans != "" && ans != "y" && ans != "yes" {
			return fmt.Errorf("aborted")
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := projects.SaveTags(dir, tags); err != nil {
			return err
		}
		p, err := projects.FindByName(cfg.BaseDir, name)
		if err != nil {
			return err
		}
		return openInTmux(cfg, p)
	},
}

// splitTags splits on any combination of commas, spaces, and tabs.
func splitTags(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return r == ' ' || r == '\t' || r == ','
	})
}

func init() {
	rootCmd.AddCommand(newCmd)
}
