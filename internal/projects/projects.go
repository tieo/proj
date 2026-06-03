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

// SessionName builds a tmux-safe session name from a project's name and tags,
// formatted name-first as "name@tag1+tag2" (tags sorted; an untagged project is
// just its name). '@' separates the name from the tag block and '+' joins tags;
// both are valid in Windows and Linux filenames and in tmux session names. The
// name keeps its own characters, so the '@' boundary stays unambiguous. tmux
// rejects '.' and ':' in session targets, so those (and '/' and ' ') are
// normalised.
func SessionName(name string, tags []string) string {
	s := name
	if len(tags) > 0 {
		sorted := append([]string{}, tags...)
		sort.Strings(sorted)
		s += "@" + strings.Join(sorted, "+")
	}
	return strings.NewReplacer(".", "_", ":", "_", "/", "-", " ", "_").Replace(s)
}

// reservedNames are Windows device names that can't be used as a path
// component. Project dirs live on Linux but are opened by claude.exe over the
// \\wsl.localhost UNC path, so we avoid them even though Linux would allow them.
var reservedNames = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
	"com1": true, "com2": true, "com3": true, "com4": true, "com5": true,
	"com6": true, "com7": true, "com8": true, "com9": true,
	"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true, "lpt5": true,
	"lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
}

// ValidateName rejects names that would break the flat one-dir-per-project
// layout, the registry keying, the "name@tag+tag" session format, or the
// Windows path namespace (these dirs are opened by claude.exe over UNC). Spaces
// mid-name are fine; they're quoted when a command line is built (shellout.Quote)
// and normalised in session names.
func ValidateName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("name required")
	case name == "." || name == "..":
		return fmt.Errorf("%q is not a valid project name", name)
	case strings.ContainsAny(name, `/\`):
		return fmt.Errorf("name %q may not contain a path separator", name)
	case strings.ContainsAny(name, "@+"):
		return fmt.Errorf("name %q may not contain '@' or '+' (reserved for the name@tag session format)", name)
	case strings.ContainsAny(name, "<>:\"|?*"):
		return fmt.Errorf(`name %q may not contain any of < > : " | ? * (not allowed in Windows paths)`, name)
	case strings.HasSuffix(name, ".") || strings.HasSuffix(name, " "):
		return fmt.Errorf("name %q may not end with a space or '.'", name)
	case reservedNames[strings.ToLower(name)]:
		return fmt.Errorf("%q is a reserved device name on Windows", name)
	}
	return nil
}

// ValidateTag rejects tags that would break the "name@tag+tag" session format.
// Tags never become directories, so they aren't held to the full path rules;
// only the structural separators and path separators are forbidden.
func ValidateTag(tag string) error {
	switch {
	case strings.TrimSpace(tag) == "":
		return fmt.Errorf("empty tag")
	case strings.ContainsAny(tag, "@+"):
		return fmt.Errorf("tag %q may not contain '@' or '+'", tag)
	case strings.ContainsAny(tag, `/\`):
		return fmt.Errorf("tag %q may not contain a path separator", tag)
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

// Resolve maps a query to exactly one existing project under baseDir. An exact
// name match wins first; then a unique case-insensitive name match; then a
// unique case-insensitive prefix. Ambiguity errors name the candidates.
// EnsureUniqueName keeps case-only collisions out at creation time, so the
// case-insensitive match is normally unique in practice.
func Resolve(baseDir, query string) (Project, error) {
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		return Project{}, err
	}
	ql := strings.ToLower(query)
	var caseEq, prefixes []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		n := e.Name()
		switch {
		case n == query:
			return FindByName(baseDir, n) // exact match always wins
		case strings.EqualFold(n, query):
			caseEq = append(caseEq, n)
		case strings.HasPrefix(strings.ToLower(n), ql):
			prefixes = append(prefixes, n)
		}
	}
	if len(caseEq) == 1 {
		return FindByName(baseDir, caseEq[0])
	}
	if len(caseEq) > 1 {
		sort.Strings(caseEq)
		return Project{}, fmt.Errorf("%q is ambiguous: matches %s", query, strings.Join(caseEq, ", "))
	}
	switch len(prefixes) {
	case 1:
		return FindByName(baseDir, prefixes[0])
	case 0:
		return Project{}, fmt.Errorf("no project matching %q (use `proj new %s` to create it)", query, query)
	default:
		sort.Strings(prefixes)
		return Project{}, fmt.Errorf("%q is ambiguous: matches %s", query, strings.Join(prefixes, ", "))
	}
}

// CheckNewName reports whether name is available to use for a new project
// under baseDir. exists is true when a project with the exact name already
// lives there (the caller decides whether that's an error, e.g. `proj new`,
// or a merge target, e.g. `proj adopt`). err is non-nil when a case-only
// sibling exists, or when baseDir can't be read. Names are case-sensitive on
// Linux but not on Windows, macOS, or WSL's UNC bridge, so allowing case-only
// siblings would break the cross-OS layout and also turn Resolve's
// case-insensitive lookup into a permanent ambiguity. This is the single
// existence-and-case-collision check; callers should not duplicate it with
// their own os.Stat probes.
func CheckNewName(baseDir, name string) (exists bool, err error) {
	entries, e := os.ReadDir(baseDir)
	if e != nil {
		if os.IsNotExist(e) {
			return false, nil
		}
		return false, e
	}
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		switch n := ent.Name(); {
		case n == name:
			exists = true
		case strings.EqualFold(n, name):
			return exists, fmt.Errorf("%q conflicts with existing %q (case-insensitive)", name, n)
		}
	}
	return exists, nil
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
