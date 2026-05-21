package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tieo/proj/internal/config"
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

		desc, _ := ask("What are you building? ")

		fmt.Printf("Language? [%s] (existing: %s) ",
			cfg.DefaultLang, strings.Join(existingLangs(cfg.BaseDir), " "))
		lang, _ := r.ReadString('\n')
		lang = strings.TrimSpace(lang)
		if lang == "" {
			lang = cfg.DefaultLang
		}

		name, _ := ask("Project name? ")
		if name == "" {
			return fmt.Errorf("name required")
		}

		dir := filepath.Join(cfg.BaseDir, lang, name)
		if _, err := os.Stat(dir); err == nil {
			return fmt.Errorf("%s already exists", dir)
		}

		ans, _ := ask(fmt.Sprintf("\nCreate %s? [Y/n] ", dir))
		ans = strings.ToLower(ans)
		if ans != "" && ans != "y" && ans != "yes" {
			return fmt.Errorf("aborted")
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if desc != "" {
			_ = os.WriteFile(filepath.Join(dir, "README.md"),
				[]byte("# "+name+"\n\n"+desc+"\n"), 0o644)
		}
		return openInTmux(cfg, name, dir)
	},
}

func existingLangs(base string) []string {
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out
}

func init() {
	rootCmd.AddCommand(newCmd)
}
