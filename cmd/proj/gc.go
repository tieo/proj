package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tieo/proj/internal/claudeapi"
	"github.com/tieo/proj/internal/config"
	"github.com/tieo/proj/internal/daemon"
)

var (
	gcRemote bool
	gcDryRun bool
)

var gcCmd = &cobra.Command{
	Use:   "gc",
	Short: "garbage-collect stale state",
	Long: `Garbage-collect stale state.

--remote purges disconnected Remote Control sessions from the Claude account
(claude.ai/code). Only the cloud entry is deleted; Claude Code's local
transcripts under ~/.claude are never touched. A disconnected session is a dead
bridge - its local process has exited or its RC dropped - so the entry only
clutters the account and the phone app. --dry-run lists what would go without
deleting.`,
	Args: cobra.NoArgs,
	RunE: runGC,
}

func init() {
	gcCmd.Flags().BoolVar(&gcRemote, "remote", false, "purge disconnected Remote Control sessions from the Claude account")
	gcCmd.Flags().BoolVar(&gcDryRun, "dry-run", false, "list what would be deleted without deleting")
	rootCmd.AddCommand(gcCmd)
}

func runGC(cmd *cobra.Command, args []string) error {
	if !gcRemote {
		return fmt.Errorf("nothing to do; the only mode is --remote (purge disconnected Claude Remote Control sessions)")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	token, err := claudeapi.Token(daemon.ClaudeRoot(cfg.Claude.Home))
	if err != nil {
		return err
	}
	sessions, tooMany, err := claudeapi.ListSessions(token)
	if err != nil {
		return err
	}

	var dead []claudeapi.Session
	for _, s := range sessions {
		if s.ConnectionStatus == "disconnected" {
			dead = append(dead, s)
		}
	}
	if len(dead) == 0 {
		fmt.Println("no disconnected Remote Control sessions")
		return nil
	}

	if gcDryRun {
		fmt.Printf("would delete %d disconnected session(s):\n", len(dead))
		printByHost(dead)
		if tooMany {
			fmt.Println("note: the account has 100+ sessions; only the first page was read")
		}
		return nil
	}

	deleted, failed := 0, 0
	for _, s := range dead {
		if err := claudeapi.DeleteSession(token, s.ID); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "  failed: %s (%s): %v\n", s.Title, s.ID, err)
			failed++
			continue
		}
		deleted++
	}
	fmt.Printf("deleted %d disconnected session(s)", deleted)
	if failed > 0 {
		fmt.Printf(", %d failed", failed)
	}
	fmt.Println()
	if tooMany {
		fmt.Println("note: the account has 100+ sessions; re-run to sweep the rest")
	}
	return nil
}

// printByHost groups sessions by the host in their title ("name @host [tags]")
// so a cross-machine sweep is legible before it runs.
func printByHost(sessions []claudeapi.Session) {
	byHost := map[string][]string{}
	for _, s := range sessions {
		byHost[titleHost(s.Title)] = append(byHost[titleHost(s.Title)], s.Title)
	}
	hosts := make([]string, 0, len(byHost))
	for h := range byHost {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	for _, h := range hosts {
		names := byHost[h]
		sort.Strings(names)
		fmt.Printf("  %-14s %d\n", h+":", len(names))
		for _, n := range names {
			fmt.Printf("      %s\n", n)
		}
	}
}

// titleHost extracts the host from a session title "name @host [tags]".
func titleHost(title string) string {
	at := strings.LastIndexByte(title, '@')
	if at < 0 {
		return "?"
	}
	rest := title[at+1:]
	if sp := strings.IndexByte(rest, ' '); sp >= 0 {
		rest = rest[:sp]
	}
	return rest
}
