// Package projects manages project directories under base_dir.
//
// Each direct child of base_dir is a project. A project's tags live in a
// single global registry file outside any project tree (see registry.go), so
// projects don't carry proj-specific files in their checkout.
package projects

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tieo/proj/internal/tmux"
)

type Project struct {
	Name      string
	Dir       string
	Tags      []string // sorted
	SessionTS int64    // tmux activity unix-time, 0 if no live session
	DirMTime  int64
}

// SessionName builds a tmux-safe session name from a project's tags and name.
// Tags are sorted alphabetically and joined with the name by '_'. Untagged
// projects produce just the name. tmux rejects '.' and ':' in session targets,
// so those (and '/' and ' ') are normalised.
func SessionName(name string, tags []string) string {
	sorted := append([]string{}, tags...)
	sort.Strings(sorted)
	joined := strings.Join(append(sorted, name), "_")
	return strings.NewReplacer(".", "_", ":", "_", "/", "-", " ", "_").Replace(joined)
}

// ValidateName rejects only the names that would break the flat one-dir-per-
// project layout or the registry keying: empty, path separators, and the
// directory aliases "." / "..". Spaces and shell metacharacters are allowed;
// they are quoted at the point a command line is built (see shellout.Quote).
func ValidateName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("name required")
	case name == "." || name == "..":
		return fmt.Errorf("%q is not a valid project name", name)
	case strings.ContainsAny(name, `/\`):
		return fmt.Errorf("name %q may not contain a path separator", name)
	}
	return nil
}

func normalize(tags []string) []string {
	seen := make(map[string]struct{}, len(tags))
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// FindByName returns the project at baseDir/name, with tags drawn from the
// registry. Returns an error if no such directory exists.
func FindByName(baseDir, name string) (Project, error) {
	dir := filepath.Join(baseDir, name)
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return Project{}, fmt.Errorf("%q not found under %s", name, baseDir)
	}
	reg, _ := LoadRegistry()
	return Project{
		Name:     name,
		Dir:      dir,
		Tags:     reg.Tags(name),
		DirMTime: info.ModTime().Unix(),
	}, nil
}

// All returns every project directly under baseDir, with session status filled
// in and tags joined in from the registry.
func All(baseDir string) []Project {
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		return nil
	}
	reg, _ := LoadRegistry()
	sessionByPath := make(map[string]int64)
	for _, s := range tmux.ListSessions() {
		sessionByPath[s.Path] = s.Activity
	}
	var out []Project
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(baseDir, e.Name())
		p := Project{
			Name: e.Name(),
			Dir:  dir,
			Tags: reg.Tags(e.Name()),
		}
		if ts, ok := sessionByPath[dir]; ok {
			p.SessionTS = ts
		} else if info, err := os.Stat(dir); err == nil {
			p.DirMTime = info.ModTime().Unix()
		}
		out = append(out, p)
	}
	return out
}

// OrphanSessions returns tmux sessions whose paths don't correspond to any
// project under baseDir.
func OrphanSessions(baseDir string) []tmux.Session {
	known := make(map[string]struct{})
	for _, p := range All(baseDir) {
		known[p.Dir] = struct{}{}
	}
	var orphans []tmux.Session
	for _, s := range tmux.ListSessions() {
		if _, ok := known[s.Path]; !ok {
			orphans = append(orphans, s)
		}
	}
	return orphans
}

// HasHistory reports whether Claude Code has a prior transcript for `dir`.
// Claude encodes the project path by replacing every non-alphanumeric rune
// with '-' (so '/', '.', '_', ' ', '+' all become '-', and adjacent specials
// like '/.' produce '--'). Stored under ~/.claude/projects/<encoded>/.
func HasHistory(dir string) bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	encoded := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		default:
			return '-'
		}
	}, dir)
	histDir := filepath.Join(home, ".claude", "projects", encoded)
	entries, err := os.ReadDir(histDir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
			return true
		}
	}
	return false
}

func Reltime(ts, now int64) string {
	if ts <= 0 {
		return "-"
	}
	d := time.Duration(now-ts) * time.Second
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
