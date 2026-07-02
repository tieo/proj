package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mattn/go-runewidth"
	"github.com/spf13/cobra"

	"github.com/tieo/proj/internal/config"
	"github.com/tieo/proj/internal/projects"
	"github.com/tieo/proj/internal/sessions"
	"github.com/tieo/proj/internal/tmux"
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

	// Piped or redirected: print the static table, no interaction.
	if !stdinIsTTY() {
		return printSessionsTable(cfg, home)
	}

	// Interactive: a moving cursor over the sessions, acted on by key. After an
	// action that mutates the list (adopt/stop/rm) the loop redraws the fresh
	// state; resume hands off to claude and ends the loop.
	for {
		all, err := sessions.List(home)
		if err != nil {
			return err
		}
		header, lines, shown, hidden := sessionLines(cfg, all)
		if len(lines) == 0 {
			if hidden > 0 {
				fmt.Printf("no recent Claude sessions (%d older; --all to show)\n", hidden)
			} else {
				fmt.Println("no Claude sessions found")
			}
			return nil
		}
		footer := "↑/↓ move · enter resume · a adopt · s stop · r rm · esc quit"
		idx, act := selectAction(header, lines, footer, "asr")
		if idx < 0 {
			return nil
		}
		s := shown[idx]
		switch act {
		case '\r':
			return resumeSession(s)
		case 'a':
			if err := adoptSessionInteractive(cfg, home, all, s); err != nil {
				fmt.Fprintf(os.Stderr, "adopt: %v\n", err)
			}
		case 's':
			if err := stopSessionInteractive(cfg, all, s); err != nil {
				fmt.Fprintf(os.Stderr, "stop: %v\n", err)
			}
		case 'r':
			if err := rmSessionInteractive(cfg, home, all, s); err != nil {
				fmt.Fprintf(os.Stderr, "rm: %v\n", err)
			}
		}
	}
}

// stdinIsTTY reports whether stdin is an interactive terminal (not a pipe or
// redirect), so the sessions list only goes interactive when a user can drive it.
func stdinIsTTY() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// printSessionsTable renders the static, non-interactive session table.
func printSessionsTable(cfg config.Config, home string) error {
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

// projectForSession returns the proj project a session's working directory
// belongs to, if any.
func projectForSession(cfg config.Config, all []sessions.Session, s sessions.Session) (projects.Project, bool) {
	for _, p := range projects.All(cfg.BaseDir) {
		if sessions.CwdForDir(p.Dir, all) == s.Cwd || p.Dir == sessions.LocalDir(s.Cwd) {
			return p, true
		}
	}
	return projects.Project{}, false
}

// adoptSessionInteractive adopts s into a project chosen interactively, using
// the same Adopt the `proj sessions adopt` command runs.
func adoptSessionInteractive(cfg config.Config, home string, all []sessions.Session, s sessions.Session) error {
	p, err := pickProject(cfg, dirBase(s.Cwd))
	if err != nil {
		return err
	}
	targetCwd := sessions.CwdForDir(p.Dir, all)
	if s.Cwd == targetCwd {
		return fmt.Errorf("session already belongs to %s", p.Name)
	}
	newID, report, err := sessions.Adopt(home, s, targetCwd, true)
	if err != nil {
		if newID == "" {
			return err
		}
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
	}
	fmt.Printf("adopted %s into %s as new session %s\n", s.ID[:8], p.Name, newID[:8])
	for _, line := range report {
		fmt.Printf("  %s\n", line)
	}
	return nil
}

// stopSessionInteractive stops (closes) the live tmux session for s, using the
// same closeSession the `proj close` command runs. It keeps all files.
func stopSessionInteractive(cfg config.Config, all []sessions.Session, s sessions.Session) error {
	name := liveSessionNameForCwd(cfg, all, s)
	if name == "" {
		fmt.Println("no live session to stop")
		return nil
	}
	if err := closeSession(name, false); err != nil {
		return err
	}
	fmt.Printf("stopped %s\n", name)
	return nil
}

// liveSessionNameForCwd returns the name of the live tmux session running in a
// session's working directory, or "".
func liveSessionNameForCwd(cfg config.Config, all []sessions.Session, s sessions.Session) string {
	dir := sessions.LocalDir(s.Cwd)
	if dir == "" {
		dir = s.Cwd
	}
	if p, ok := projectForSession(cfg, all, s); ok {
		want := projects.SessionName(p.Name, p.Tags)
		for _, ts := range tmux.ListSessions() {
			if ts.Name == want {
				return want
			}
		}
	}
	for _, ts := range tmux.ListSessions() {
		if ts.Path == dir || ts.Path == s.Cwd {
			return ts.Name
		}
	}
	return ""
}

// rmSessionInteractive removes s. When its cwd is a proj project, it deletes
// the whole project via the same removeProject the `proj rm` command runs
// (files included), after a confirm that names the directory. Otherwise it
// removes just this session's transcript and per-session sidecars. Both paths
// confirm first.
func rmSessionInteractive(cfg config.Config, home string, all []sessions.Session, s sessions.Session) error {
	if p, ok := projectForSession(cfg, all, s); ok {
		if !confirm(fmt.Sprintf("rm project %s and delete its files at %s? [y/N] ", p.Name, p.Dir)) {
			return nil
		}
		if err := removeProject(p); err != nil {
			return err
		}
		fmt.Printf("removed project %s\n", p.Name)
		return nil
	}
	if !confirm(fmt.Sprintf("rm session %s (transcript %s), keeping any project? [y/N] ", s.ID[:8], s.Path)) {
		return nil
	}
	if err := os.Remove(s.Path); err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, kind := range []string{"tasks", "file-history"} {
		_ = os.RemoveAll(filepath.Join(home, kind, s.ID))
	}
	fmt.Printf("removed session %s\n", s.ID[:8])
	return nil
}

// confirm prints a yes/no prompt and reports whether the user answered yes.
func confirm(prompt string) bool {
	fmt.Print(prompt)
	ans, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	a := strings.ToLower(strings.TrimSpace(ans))
	return a == "y" || a == "yes"
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
	return resumeSession(s)
}

// resumeSession hands off to `claude --resume` for s, launched from the
// session's working directory (recreated empty if a temp-dir session outlived
// it, since Claude locates a session by its cwd).
func resumeSession(s sessions.Session) error {
	dir := sessions.LocalDir(s.Cwd)
	if dir == "" {
		dir = s.Cwd
	}
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
