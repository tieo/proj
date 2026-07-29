package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tieo/proj/internal/config"
	"github.com/tieo/proj/internal/projects"
	"github.com/tieo/proj/internal/sessions"
)

// The headless half of `proj sessions`: the same list, prompt index and fork the
// interactive picker drives, reachable as plain commands so a script (or an
// agent) can do in three calls what the TUI needs a keystroke at a time.

var sessionsListCmd = &cobra.Command{
	Use:   "list",
	Short: "print the session table without the interactive picker",
	Long: `Print every Claude session, newest first, as a table or as JSON.

The table is the one the picker shows. --json emits one object per session with
id, cwd, project, message count, modification time and transcript path, which is
what a caller feeds back into "sessions prompts" and "sessions fork".`,
	Args: cobra.NoArgs,
	RunE: runSessionsList,
}

var sessionsPromptsCmd = &cobra.Command{
	Use:   "prompts <session-id>",
	Short: "list a session's user prompts, numbered as fork ranges count them",
	Long: `List the real user prompts of a session, oldest first, numbered from 1.

The numbers are the ones "sessions fork --from/--to" takes: synthetic user lines
(tool results, compaction notices, interrupts) are skipped here exactly as they
are there, so what this prints is what a fork can cut on.

Each line also carries the size of the turn it starts - that prompt plus the
reply and tool output that follow it - so a caller can pick a range that fits a
context window instead of guessing. --json emits the byte offsets too.`,
	Args: cobra.ExactArgs(1),
	RunE: runSessionsPrompts,
}

var sessionsForkCmd = &cobra.Command{
	Use:   "fork <session-id>",
	Short: "branch a range of a session's history into another project",
	Long: `Copy messages --from..--to of a session into --into, as a new session there.

The source session is left untouched; the copy is rewritten to the target
project's working directory and gets a fresh session id, so resuming the target
project lands in the branched history. Prompt numbers come from
"sessions prompts"; --from defaults to the first prompt and --to to the last.

The target project is created when it does not exist yet. With --no-open the
fork only writes the transcript, leaving the tmux session to a later
"proj <name>".`,
	Args: cobra.ExactArgs(1),
	RunE: runSessionsFork,
}

var (
	sessionsJSON     bool
	sessionsForkFrom int
	sessionsForkTo   int
	sessionsForkInto string
	sessionsForkTags []string
	sessionsForkOpen bool
)

func init() {
	sessionsListCmd.Flags().BoolVar(&sessionsJSON, "json", false, "emit JSON instead of a table")
	sessionsPromptsCmd.Flags().BoolVar(&sessionsJSON, "json", false, "emit JSON instead of a table")
	sessionsForkCmd.Flags().IntVar(&sessionsForkFrom, "from", 0, "first prompt to keep (default: the first prompt)")
	sessionsForkCmd.Flags().IntVar(&sessionsForkTo, "to", 0, "last prompt to keep (default: the last prompt)")
	sessionsForkCmd.Flags().StringVar(&sessionsForkInto, "into", "", "target project name (created if new)")
	sessionsForkCmd.Flags().StringSliceVar(&sessionsForkTags, "tags", nil, "tags for a newly created target project")
	sessionsForkCmd.Flags().BoolVar(&sessionsForkOpen, "open", true, "open the target project's session after forking")
	_ = sessionsForkCmd.MarkFlagRequired("into")
	sessionsCmd.AddCommand(sessionsListCmd, sessionsPromptsCmd, sessionsForkCmd)
}

// sessionJSON is the machine-readable form of a session row.
type sessionJSON struct {
	ID       string `json:"id"`
	Project  string `json:"project"`
	Cwd      string `json:"cwd"`
	Path     string `json:"path"`
	Messages int    `json:"messages"`
	Modified string `json:"modified"`
	Title    string `json:"title"`
}

func runSessionsList(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	home := sessions.Home(cfg.Claude.Home)
	if !sessionsJSON {
		return printSessionsTable(cfg, home)
	}
	all, err := sessions.List(home)
	if err != nil {
		return err
	}
	nameByCwd := map[string]string{}
	for _, p := range projects.All(cfg.BaseDir) {
		nameByCwd[sessions.CwdForDir(p.Dir, all)] = p.Name
	}
	out := make([]sessionJSON, 0, len(all))
	for _, s := range all {
		name, ok := nameByCwd[s.Cwd]
		if !ok {
			name = dirBase(s.Cwd)
		}
		out = append(out, sessionJSON{
			ID:       s.ID,
			Project:  name,
			Cwd:      s.Cwd,
			Path:     s.Path,
			Messages: s.Messages,
			Modified: s.Modified.Format("2006-01-02T15:04:05Z07:00"),
			Title:    s.Title,
		})
	}
	return writeJSON(out)
}

// promptJSON is one numbered prompt plus the byte span of the turn it starts.
type promptJSON struct {
	N     int    `json:"n"`
	Text  string `json:"text"`
	At    int    `json:"at"`
	CutAt int    `json:"cut_at"`
	Bytes int    `json:"bytes"`
}

func runSessionsPrompts(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	s, err := resolveSession(sessions.Home(cfg.Claude.Home), args[0])
	if err != nil {
		return err
	}
	prompts, err := sessions.Prompts(s.Path)
	if err != nil {
		return err
	}
	if sessionsJSON {
		out := make([]promptJSON, len(prompts))
		for i, p := range prompts {
			out[i] = promptJSON{N: i + 1, Text: p.Text, At: p.At, CutAt: p.CutAt, Bytes: p.CutAt - p.At}
		}
		return writeJSON(out)
	}
	if len(prompts) == 0 {
		fmt.Printf("session %s has no user prompts\n", s.ID[:8])
		return nil
	}
	w := termWidth() - 20
	if w < 40 {
		w = 40
	}
	for i, p := range prompts {
		fmt.Printf("%5d %8s  %s\n", i+1, humanBytes(p.CutAt-p.At), truncPad(p.Text, w))
	}
	return nil
}

func runSessionsFork(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	home := sessions.Home(cfg.Claude.Home)
	s, err := resolveSession(home, args[0])
	if err != nil {
		return err
	}
	prompts, err := sessions.Prompts(s.Path)
	if err != nil {
		return err
	}
	if len(prompts) == 0 {
		return fmt.Errorf("session %s has no user messages to fork from", s.ID[:8])
	}
	from, to, err := forkBounds(len(prompts), sessionsForkFrom, sessionsForkTo)
	if err != nil {
		return err
	}
	all, err := sessions.List(home)
	if err != nil {
		return err
	}
	p, err := ensureProject(cfg, sessionsForkInto, sessionsForkTags)
	if err != nil {
		return err
	}
	targetCwd := sessions.CwdForDir(p.Dir, all)
	if s.Cwd == targetCwd {
		return fmt.Errorf("cannot fork a session into its own project")
	}
	keepFrom := 0
	if from > 1 {
		keepFrom = prompts[from-1].At
	}
	newID, report, err := sessions.ForkRange(home, s, targetCwd, keepFrom, prompts[to-1].CutAt, prompts[0].At)
	if err != nil {
		return err
	}
	fmt.Printf("forked %s messages %d–%d into %s as new session %s\n", s.ID[:8], from, to, p.Name, newID[:8])
	for _, line := range report {
		fmt.Printf("  %s\n", line)
	}
	if !sessionsForkOpen {
		return nil
	}
	return openInTmux(cfg, p)
}

// forkBounds turns the --from/--to flags into a 1-based prompt range over a
// session with n prompts, where 0 means "the end you did not name": --from
// alone forks to the last prompt, --to alone from the first.
func forkBounds(n, from, to int) (int, int, error) {
	if from == 0 {
		from = 1
	}
	if to == 0 {
		to = n
	}
	if from < 1 || to > n || from > to {
		return 0, 0, fmt.Errorf("range %d–%d is outside this session's 1–%d prompts", from, to, n)
	}
	return from, to, nil
}

// resolveSession finds the session whose id starts with query. A full id or any
// prefix long enough to be unique works; an ambiguous prefix names the
// candidates rather than picking one.
func resolveSession(home, query string) (sessions.Session, error) {
	all, err := sessions.List(home)
	if err != nil {
		return sessions.Session{}, err
	}
	var hits []sessions.Session
	for _, s := range all {
		if s.ID == query {
			return s, nil
		}
		if strings.HasPrefix(s.ID, query) {
			hits = append(hits, s)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return sessions.Session{}, fmt.Errorf("no session matches %q", query)
	default:
		var ids []string
		for _, s := range hits {
			ids = append(ids, s.ID[:8])
		}
		return sessions.Session{}, fmt.Errorf("%q matches %d sessions: %s", query, len(hits), strings.Join(ids, " "))
	}
}

// ensureProject returns the named project, creating its directory (and
// recording tags) when the name is new. It is the non-interactive counterpart
// of what pickProject does once a name has been typed.
func ensureProject(cfg config.Config, name string, tags []string) (projects.Project, error) {
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
	if !exists {
		if err := os.MkdirAll(filepath.Join(cfg.BaseDir, name), 0o755); err != nil {
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

// writeJSON prints v as indented JSON on stdout.
func writeJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// humanBytes renders a turn's size in the largest unit that keeps it short.
func humanBytes(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1fM", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0fK", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}
