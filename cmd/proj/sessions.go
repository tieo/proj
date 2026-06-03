package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/mattn/go-runewidth"
	"github.com/spf13/cobra"

	"github.com/tieo/proj/internal/config"
	"github.com/tieo/proj/internal/projects"
	"github.com/tieo/proj/internal/sessions"
)

var sessionsCmd = &cobra.Command{
	Use:   "sessions",
	Short: "list existing Claude sessions; resume one with `proj sessions resume <id>`",
	Long: `List the Claude Code sessions on disk, newest first. proj only indexes them;
viewing is handed off to Claude itself.`,
	Args: cobra.NoArgs,
	RunE: runSessions,
}

var sessionsResumeCmd = &cobra.Command{
	Use:   "resume [id]",
	Short: "reopen a Claude session by id or prefix, or pick one interactively (hands off to `claude --resume`)",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runSessionsResume,
}

func init() {
	sessionsCmd.AddCommand(sessionsResumeCmd)
	sessionsCmd.AddCommand(newAdoptCmd())
	rootCmd.AddCommand(sessionsCmd)
}

func runSessions(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	home := sessions.Home(cfg.Claude.Home)
	all, err := sessions.List(home)
	if err != nil {
		return err
	}

	header, lines, _, hidden := sessionLines(cfg, all)
	if len(lines) == 0 {
		if hidden > 0 {
			fmt.Printf("no recent Claude sessions (%d older; --all to show)\n", hidden)
		} else {
			fmt.Println("no Claude sessions found")
		}
		return nil
	}
	fmt.Printf("  %s\n", header)
	for _, ln := range lines {
		fmt.Printf("  %s\n", ln)
	}
	if hidden > 0 {
		fmt.Printf("\n  \033[90m%d older session(s) hidden; --all to show\033[0m\n", hidden)
	}
	return nil
}

// termWidth reports the controlling terminal's column count, defaulting to 120
// when stdout is not a terminal (piped or redirected).
func termWidth() int {
	c := exec.Command("stty", "size")
	c.Stdin = os.Stdin
	out, err := c.Output()
	if err != nil {
		return 120
	}
	if f := strings.Fields(string(out)); len(f) == 2 {
		if n, err := strconv.Atoi(f[1]); err == nil && n > 20 {
			return n
		}
	}
	return 120
}

// sessionTextCols splits the space left after the fixed columns (and the 2-col
// indent/cursor) evenly between the last-message and last-answer columns.
func sessionTextCols() (msgW, ansW int) {
	avail := termWidth() - 47
	if avail < 30 {
		avail = 30
	}
	msgW = avail / 2
	ansW = avail - msgW
	return
}

// sessionHeader is the shared column header for both the table and the picker.
func sessionHeader(msgW, ansW int) string {
	return fmt.Sprintf("\033[90m%-8s %9s %6s  %-16s %s %s\033[0m",
		"ID", "AGE", "MSGS", "PROJECT", truncPadRight("LAST MESSAGE", msgW), truncPad("LAST ANSWER", ansW))
}

// sessionRow renders one session line: the project cell is green for the current
// session of a proj project and grey otherwise, then the last user message and
// (dimmed) the last assistant answer.
func sessionRow(s sessions.Session, name string, green bool, now time.Time, msgW, ansW int) string {
	cell := truncPad(name, projNameCol)
	if green {
		cell = "\033[32m" + cell + "\033[0m"
	} else {
		cell = "\033[90m" + cell + "\033[0m"
	}
	msgCell := truncPadRight(s.Title, msgW)
	if s.Title == "" || s.Title == "(no prompt)" {
		msgCell = "\033[90m" + truncPadRight("(no messages)", msgW) + "\033[0m"
	}
	ansCell := "\033[90m" + truncPad(s.Answer, ansW) + "\033[0m" // dim: the assistant's reply
	return fmt.Sprintf("%-8s %9s %6d  %s %s %s", s.ID[:8], formatAgo(now.Sub(s.Modified)), s.Messages, cell, msgCell, ansCell)
}

// sessionLines builds the header and rendered rows (recency-filtered unless
// --all), returning the sessions parallel to lines and the hidden count.
func sessionLines(cfg config.Config, all []sessions.Session) (header string, lines []string, shown []sessions.Session, hidden int) {
	nameByCwd := map[string]string{}
	for _, p := range projects.All(cfg.BaseDir) {
		nameByCwd[sessions.CwdForDir(p.Dir, all)] = p.Name
	}
	var cutoff time.Time
	if !listAll && cfg.List.MaxAgeDays > 0 {
		cutoff = time.Now().AddDate(0, 0, -cfg.List.MaxAgeDays)
	}
	now := time.Now()
	msgW, ansW := sessionTextCols()
	greened := map[string]bool{} // only the newest session of a managed project is green
	for _, s := range all {
		if !cutoff.IsZero() && s.Modified.Before(cutoff) {
			hidden++
			continue
		}
		name, isManaged := nameByCwd[s.Cwd]
		green := false
		if isManaged {
			// `all` is newest-first, so the first session seen for a managed
			// project is its current one; older ones stay grey.
			green = !greened[s.Cwd]
			greened[s.Cwd] = true
		} else {
			name = dirBase(s.Cwd)
		}
		lines = append(lines, sessionRow(s, name, green, now, msgW, ansW))
		shown = append(shown, s)
	}
	return sessionHeader(msgW, ansW), lines, shown, hidden
}

// dirBase is filepath.Base for both / and \ separated paths (the cwd may be a
// Windows or UNC path even though proj runs on Linux).
func dirBase(p string) string {
	p = strings.TrimRight(p, `/\`)
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

// truncPad fits s to exactly w terminal columns, padding with spaces or
// truncating with an ellipsis. Uses display width (not rune count) so wide
// runes - CJK, emoji, box-drawing - don't overflow the column and wrap onto
// the next line, which would shove later cells out of alignment.
func truncPad(s string, w int) string {
	if runewidth.StringWidth(s) > w {
		return runewidth.Truncate(s, w, "…")
	}
	return s + strings.Repeat(" ", w-runewidth.StringWidth(s))
}

// truncPadRight is truncPad but right-aligned: a short string is padded on the
// left so its text sits flush against the next column.
func truncPadRight(s string, w int) string {
	if runewidth.StringWidth(s) > w {
		return runewidth.Truncate(s, w, "…")
	}
	return strings.Repeat(" ", w-runewidth.StringWidth(s)) + s
}

func runSessionsResume(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	home := sessions.Home(cfg.Claude.Home)
	all, err := sessions.List(home)
	if err != nil {
		return err
	}
	var s sessions.Session
	if len(args) == 1 {
		if s, err = sessions.FindIn(all, args[0]); err != nil {
			return err
		}
	} else if s, err = pickSession(cfg, all); err != nil {
		return err
	}
	dir := sessions.LocalDir(s.Cwd)
	if dir == "" {
		dir = s.Cwd
	}
	// Claude finds a session by its working directory, so it has to launch from
	// that path. Temp-dir sessions outlive their directory; recreate it (empty)
	// so the resume can still chdir there and locate the transcript.
	if _, statErr := os.Stat(dir); os.IsNotExist(statErr) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("session directory %s is gone and could not be recreated: %w", dir, err)
		}
		fmt.Fprintf(os.Stderr, "note: recreated missing session directory %s\n", dir)
	}
	c := exec.Command("claude", "--resume", s.ID, "--dangerously-skip-permissions")
	c.Dir = dir
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("could not launch claude (run manually: cd %q && claude --resume %s): %w", dir, s.ID, err)
	}
	return nil
}
