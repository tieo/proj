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
	Short: "interactive list of Claude sessions: enter resume, a adopt, f fork, s stop, r rm",
	Long: `List the Claude Code sessions on disk, newest first, as an interactive list:
move with the arrows, then enter to resume, a to adopt into a project, f to fork
(branch the history at a chosen message into a new project), s to stop (close,
keeping files), r to rm (delete the project and its files), esc to quit. Piped or
redirected, it prints the static table instead.`,
	Args: cobra.NoArgs,
	RunE: runSessions,
}

func init() {
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
		footer := "↑/↓ move · enter resume · a adopt · f fork · s stop · r rm · esc quit"
		idx, act := selectAction(header, lines, footer, "afsr")
		if idx < 0 {
			return nil
		}
		s := shown[idx]
		switch act {
		case '\r':
			// A successful resume hands off to claude and exits the list; a failed
			// one (e.g. a session with no cwd) stays in the list so the user can
			// pick another instead of the whole command erroring out.
			if err := resumeSession(s); err != nil {
				fmt.Fprintf(os.Stderr, "resume: %v\n", err)
			} else {
				return nil
			}
		case 'a':
			if err := adoptSessionInteractive(cfg, home, all, s); err != nil {
				fmt.Fprintf(os.Stderr, "adopt: %v\n", err)
			}
		case 'f':
			if err := forkSessionInteractive(cfg, home, all, s); err != nil {
				fmt.Fprintf(os.Stderr, "fork: %v\n", err)
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

// forkSessionInteractive branches s at a user-chosen message into a new project.
// It lists s's prompts, cuts after the selected one (keeping that turn and its
// reply), creates the chosen project, and copies the truncated history in via
// sessions.Fork. The source session is left untouched.
func forkSessionInteractive(cfg config.Config, home string, all []sessions.Session, s sessions.Session) error {
	prompts, err := sessions.Prompts(s.Path)
	if err != nil {
		return err
	}
	if len(prompts) == 0 {
		return fmt.Errorf("session %s has no user messages to fork from", s.ID[:8])
	}
	rows, cols := termSize()
	w := cols - 8
	lines := make([]string, len(prompts))
	for i, p := range prompts {
		lines[i] = fmt.Sprintf("\033[90m%4d\033[0m  %s", i+1, truncPad(p.Text, w))
	}
	h := rows - 6
	if h < 8 {
		h = 8
	}
	// Cursor starts on the newest message (bottom); arrow up walks back through
	// the history to the branch point.
	sel := selectFromEnd("fork after which message? (keeps it and its reply)",
		lines, "↑/↓ move · pgup/pgdn · enter fork here · esc cancel", h)
	if sel < 0 {
		return nil
	}
	p, err := pickProject(cfg, "")
	if err != nil {
		return err
	}
	targetCwd := sessions.CwdForDir(p.Dir, all)
	if s.Cwd == targetCwd {
		return fmt.Errorf("cannot fork a session into its own project")
	}
	newID, report, err := sessions.Fork(home, s, targetCwd, prompts[sel].CutAt)
	if err != nil {
		return err
	}
	fmt.Printf("forked %s after message %d into %s as new session %s\n", s.ID[:8], sel+1, p.Name, newID[:8])
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
// (files included). Otherwise it removes the whole session: transcript,
// per-session sidecars, and the transcript's project folder (memory included)
// once no other session remains in it. Both paths confirm first.
func rmSessionInteractive(cfg config.Config, home string, all []sessions.Session, s sessions.Session) error {
	if p, ok := projectForSession(cfg, all, s); ok {
		if !confirm(fmt.Sprintf("rm project %s and all its files? [y/N] ", p.Name)) {
			return nil
		}
		if err := removeProject(p); err != nil {
			return err
		}
		fmt.Printf("removed project %s\n", p.Name)
		return nil
	}
	// Not a proj project (e.g. a temp-dir session): rm the whole session -
	// transcript, per-session sidecars, and the transcript's project folder
	// (memory included) once no other session remains in it.
	if !confirm(fmt.Sprintf("rm session %s and all its data? [y/N] ", s.ID[:8])) {
		return nil
	}
	if err := os.Remove(s.Path); err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, kind := range []string{"tasks", "file-history"} {
		_ = os.RemoveAll(filepath.Join(home, kind, s.ID))
	}
	folder := filepath.Dir(s.Path)
	if left, _ := filepath.Glob(filepath.Join(folder, "*.jsonl")); len(left) == 0 {
		_ = os.RemoveAll(folder)
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

// pickProject defaults to creating a new project (type a name) and lets the
// user arrow down to adopt into an existing one instead.
func pickProject(cfg config.Config, defaultName string) (projects.Project, error) {
	all := projects.All(cfg.BaseDir)
	lines := make([]string, len(all))
	for i, p := range all {
		line := fmt.Sprintf("%-*s", projNameCol, p.Name)
		if len(p.Tags) > 0 {
			line += "  \033[90m" + strings.Join(p.Tags, " ") + "\033[0m"
		}
		lines[i] = line
	}
	name, tags, idx, ok := selectOrCreate(defaultName, lines)
	if !ok {
		return projects.Project{}, fmt.Errorf("cancelled")
	}
	if idx >= 0 {
		return projects.FindByName(cfg.BaseDir, all[idx].Name)
	}
	if err := projects.ValidateName(name); err != nil {
		return projects.Project{}, err
	}
	for _, t := range tags {
		if err := projects.ValidateTag(t); err != nil {
			return projects.Project{}, err
		}
	}
	exists, err := projects.CheckNewName(cfg.BaseDir, name)
	if err != nil {
		return projects.Project{}, err
	}
	dir := filepath.Join(cfg.BaseDir, name)
	if !exists {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return projects.Project{}, err
		}
	}
	if len(tags) > 0 {
		if reg, err := projects.LoadRegistry(); err == nil {
			_ = reg.SetTags(name, tags)
		}
	}
	return projects.FindByName(cfg.BaseDir, name)
}

// termSize reports the controlling terminal's row and column count, defaulting
// to 24x120 when stdin is not a terminal (piped or redirected).
func termSize() (rows, cols int) {
	rows, cols = 24, 120
	c := exec.Command("stty", "size")
	c.Stdin = os.Stdin
	out, err := c.Output()
	if err != nil {
		return
	}
	if f := strings.Fields(string(out)); len(f) == 2 {
		if r, err := strconv.Atoi(f[0]); err == nil && r > 4 {
			rows = r
		}
		if n, err := strconv.Atoi(f[1]); err == nil && n > 20 {
			cols = n
		}
	}
	return
}

// termWidth reports the controlling terminal's column count.
func termWidth() int {
	_, cols := termSize()
	return cols
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

// resumeSession hands off to `claude --resume` for s, launched from the
// session's working directory (recreated empty if a temp-dir session outlived
// it, since Claude locates a session by its cwd).
func resumeSession(s sessions.Session) error {
	dir := sessions.LocalDir(s.Cwd)
	if dir == "" {
		dir = s.Cwd
	}
	if dir == "" {
		return fmt.Errorf("session %s has no recorded working directory (nothing to resume)", s.ID[:8])
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
