// Package projects manages the on-disk project directories under base_dir
// and reconciles them with their tmux session state.
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
	Lang      string
	Dir       string
	SessionTS int64 // tmux activity unix-time, 0 if no live session
	DirMTime  int64
}

// SessionName builds a tmux-safe session name from a project's lang directory
// and base name. The lang prefix disambiguates same-named projects living in
// different lang dirs (e.g. nix/.dotfiles.nix vs Nix/.dotfiles.nix); without
// it they collapse to a single tmux session and clobber each other's state.
// tmux rejects '.' and ':' in session targets, and we fold '/' so the
// lang/name join stays a single token.
func SessionName(lang, name string) string {
	return strings.NewReplacer("/", "-", ".", "_", ":", "_").Replace(lang + "/" + name)
}

// SessionNameForDir derives the session name from a full project directory of
// the form <baseDir>/<lang>/<name>.
func SessionNameForDir(dir string) string {
	return SessionName(filepath.Base(filepath.Dir(dir)), filepath.Base(dir))
}

// FindAll returns every project directory matching baseDir/*/name, one per
// lang dir that contains it, sorted by lang name. A name can legitimately
// exist under multiple langs, so callers must decide how to disambiguate.
func FindAll(baseDir, name string) []string {
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		return nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		candidate := filepath.Join(baseDir, e.Name(), name)
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			out = append(out, candidate)
		}
	}
	return out
}

// All returns every project at baseDir/<lang>/<name>, with session status filled in.
func All(baseDir string) []Project {
	langs, err := os.ReadDir(baseDir)
	if err != nil {
		return nil
	}
	sessionByPath := make(map[string]int64)
	for _, s := range tmux.ListSessions() {
		sessionByPath[s.Path] = s.Activity
	}
	var out []Project
	for _, l := range langs {
		if !l.IsDir() {
			continue
		}
		langDir := filepath.Join(baseDir, l.Name())
		names, err := os.ReadDir(langDir)
		if err != nil {
			continue
		}
		for _, n := range names {
			if !n.IsDir() {
				continue
			}
			dir := filepath.Join(langDir, n.Name())
			p := Project{Name: n.Name(), Lang: l.Name(), Dir: dir}
			if ts, ok := sessionByPath[dir]; ok {
				p.SessionTS = ts
			} else if info, err := os.Stat(dir); err == nil {
				p.DirMTime = info.ModTime().Unix()
			}
			out = append(out, p)
		}
	}
	return out
}

// OrphanSessions returns tmux sessions whose paths don't correspond to any
// project under baseDir (useful for the list view).
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
