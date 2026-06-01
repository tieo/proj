// Package sessions reads and relocates Claude Code session transcripts.
//
// Claude stores one JSONL transcript per session under
// <home>/projects/<encoded-cwd>/<session-id>.jsonl, where <encoded-cwd> is the
// session's working directory with every non-alphanumeric rune replaced by '-'.
// Each record also carries the real `cwd`, so this package reads paths straight
// from the transcript rather than reversing the lossy folder encoding.
package sessions

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
)

// Home returns the Claude home (parent of projects/ and .claude.json). It
// honors an explicit override, then ~/.claude, then a Windows install reachable
// from WSL (claude.exe writes under the Windows home, not the Linux one).
func Home(override string) string {
	if override != "" {
		return override
	}
	if home, err := os.UserHomeDir(); err == nil {
		native := filepath.Join(home, ".claude")
		if hasProjects(native) {
			return native
		}
	}
	if w := windowsHome(); w != "" {
		return w
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

func hasProjects(home string) bool {
	info, err := os.Stat(filepath.Join(home, "projects"))
	return err == nil && info.IsDir()
}

// windowsHome locates the Windows .claude reachable from WSL (e.g.
// /mnt/c/Users/<you>/.claude), via %USERPROFILE% or a scan of /mnt/c/Users.
func windowsHome() string {
	if out, err := exec.Command("cmd.exe", "/c", "echo %USERPROFILE%").Output(); err == nil {
		if wsl := winPathToWSL(strings.TrimSpace(string(out))); wsl != "" {
			if cand := filepath.Join(wsl, ".claude"); hasProjects(cand) {
				return cand
			}
		}
	}
	if matches, _ := filepath.Glob("/mnt/*/Users/*/.claude/projects"); len(matches) > 0 {
		return filepath.Dir(matches[0])
	}
	return ""
}

// winPathToWSL maps a Windows path like C:\Users\x to /mnt/c/Users/x.
func winPathToWSL(p string) string {
	if len(p) < 2 || p[1] != ':' {
		return ""
	}
	return "/mnt/" + strings.ToLower(p[:1]) + strings.ReplaceAll(p[2:], `\`, "/")
}

// EncodeCwd maps a working directory to its Claude transcript folder name.
func EncodeCwd(cwd string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		default:
			return '-'
		}
	}, cwd)
}

// Session is the lightweight metadata for one transcript.
type Session struct {
	ID       string
	Cwd      string
	Path     string
	Modified time.Time
	Messages int
	Title    string
}

// List returns every session under home, newest first.
func List(home string) ([]Session, error) {
	files, err := filepath.Glob(filepath.Join(home, "projects", "*", "*.jsonl"))
	if err != nil {
		return nil, err
	}
	var out []Session
	for _, f := range files {
		info, err := os.Stat(f)
		if err != nil {
			continue
		}
		cwd, title, n := readMeta(f)
		out = append(out, Session{
			ID:       strings.TrimSuffix(filepath.Base(f), ".jsonl"),
			Cwd:      cwd,
			Path:     f,
			Modified: info.ModTime(),
			Messages: n,
			Title:    title,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Modified.After(out[j].Modified) })
	return out, nil
}

// Find returns the session whose id equals or uniquely prefixes the argument.
func Find(home, id string) (Session, error) {
	all, err := List(home)
	if err != nil {
		return Session{}, err
	}
	return FindIn(all, id)
}

// FindIn returns the session in list whose id equals or uniquely prefixes id.
func FindIn(list []Session, id string) (Session, error) {
	var matches []Session
	for _, s := range list {
		if s.ID == id {
			return s, nil
		}
		if strings.HasPrefix(s.ID, id) {
			matches = append(matches, s)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return Session{}, fmt.Errorf("no session matching %q", id)
	default:
		return Session{}, fmt.Errorf("%q is ambiguous (%d sessions match)", id, len(matches))
	}
}

type record struct {
	Type        string `json:"type"`
	Cwd         string `json:"cwd"`
	CustomTitle string `json:"customTitle"`
	Summary     string `json:"summary"`
	Message     struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// readMeta scans a transcript once for its cwd, a human title, and a message
// count, tolerating very long lines.
func readMeta(path string) (cwd, title string, messages int) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", 0
	}
	defer f.Close()
	r := bufio.NewReader(f)
	var summary, custom string
	var prompts []string
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			var rec record
			if json.Unmarshal(line, &rec) == nil {
				if cwd == "" && rec.Cwd != "" {
					cwd = rec.Cwd
				}
				if rec.Summary != "" {
					summary = rec.Summary
				}
				if rec.CustomTitle != "" {
					custom = rec.CustomTitle
				}
				if rec.Type == "user" || rec.Type == "assistant" {
					messages++
				}
				if rec.Type == "user" && rec.Message.Role == "user" && len(prompts) < 6 {
					if t := cleanText(firstText(rec.Message.Content)); t != "" {
						prompts = append(prompts, t)
					}
				}
			}
		}
		if err != nil {
			break
		}
	}
	best := bestPrompt(prompts)
	switch {
	case summary != "":
		title = summary
	case best != "":
		title = best
	case custom != "":
		title = custom
	default:
		title = "(no prompt)"
	}
	return cwd, oneLine(title, 56), messages
}

// firstText returns the plain text of a user message's content (string form or
// the first text block), or "" for tool results and structured-only content.
func firstText(content json.RawMessage) string {
	var s string
	if json.Unmarshal(content, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) == nil {
		for _, b := range blocks {
			if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
				return b.Text
			}
		}
	}
	return ""
}

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]`)

// cleanText normalizes a prompt to one tidy line: strips ANSI escapes, collapses
// whitespace, drops synthetic (<...>) messages, and trims leading glyph noise
// (box-drawing, bullets, prompt markers).
func cleanText(s string) string {
	s = ansiRE.ReplaceAllString(s, "")
	s = strings.Join(strings.Fields(s), " ")
	if s == "" || strings.HasPrefix(s, "<") {
		return ""
	}
	s = strings.TrimSpace(strings.TrimLeftFunc(s, func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '"' || r == '\'')
	}))
	// Claude injects these as user-role messages; they are not real prompts.
	for _, noise := range []string{"This session is being continued", "Conversation compacted", "Caveat:"} {
		if strings.HasPrefix(s, noise) {
			return ""
		}
	}
	return s
}

// bestPrompt picks the most title-like prompt: prefer prose (starts with a
// letter and is mostly letters), else the first non-empty candidate.
func bestPrompt(prompts []string) string {
	for _, p := range prompts {
		if r := []rune(p); len(r) > 0 && unicode.IsLetter(r[0]) && letterRatio(p) > 0.5 {
			return p
		}
	}
	for _, p := range prompts {
		if p != "" {
			return p
		}
	}
	return ""
}

func letterRatio(s string) float64 {
	letters, total := 0, 0
	for _, r := range s {
		if unicode.IsSpace(r) {
			continue
		}
		total++
		if unicode.IsLetter(r) {
			letters++
		}
	}
	if total == 0 {
		return 0
	}
	return float64(letters) / float64(total)
}

func oneLine(s string, max int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len([]rune(s)) > max {
		s = string([]rune(s)[:max-1]) + "…"
	}
	return s
}

// Adopt copies sess into the transcript folder for targetCwd, rewriting every
// occurrence of the old cwd to targetCwd, then points .claude.json's
// lastSessionId for targetCwd at the session so `claude -c` resumes it.
func Adopt(home string, sess Session, targetCwd string) (string, error) {
	targetDir := filepath.Join(home, "projects", EncodeCwd(targetCwd))
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(targetDir, sess.ID+".jsonl")
	if _, err := os.Stat(dst); err == nil {
		return "", fmt.Errorf("session %s already exists in the target project", sess.ID)
	}
	data, err := os.ReadFile(sess.Path)
	if err != nil {
		return "", err
	}
	if sess.Cwd != "" && sess.Cwd != targetCwd {
		data = bytes.ReplaceAll(data, []byte(jsonInner(sess.Cwd)), []byte(jsonInner(targetCwd)))
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return "", err
	}
	if err := pointLastSession(home, targetCwd, sess.ID); err != nil {
		return dst, fmt.Errorf("copied transcript but could not update the continue pointer: %w", err)
	}
	return dst, nil
}

// jsonInner returns the JSON encoding of s without the surrounding quotes, so it
// matches the escaped form a path takes inside a transcript.
func jsonInner(s string) string {
	b, _ := json.Marshal(s)
	return string(b[1 : len(b)-1])
}

// pointLastSession sets projects[cwd].lastSessionId in <home>/../.claude.json.
// It round-trips the file with json.Number so numeric fields are preserved; key
// order is not (the app rewrites this file itself, so that is harmless).
func pointLastSession(home, cwd, id string) error {
	path := filepath.Join(filepath.Dir(home), ".claude.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var root map[string]any
	if err := dec.Decode(&root); err != nil {
		return err
	}
	projects, ok := root["projects"].(map[string]any)
	if !ok {
		projects = map[string]any{}
		root["projects"] = projects
	}
	proj, ok := projects[cwd].(map[string]any)
	if !ok {
		proj = map[string]any{}
		projects[cwd] = proj
	}
	proj["lastSessionId"] = id
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o644)
}

// UNCToWSL converts a \\wsl.localhost\<distro>\... cwd (how claude.exe records a
// WSL path) to its Linux form, or "" if it is not such a path.
func UNCToWSL(p string) string {
	for _, pre := range []string{`\\wsl.localhost\`, `\\wsl$\`} {
		if strings.HasPrefix(p, pre) {
			rest := p[len(pre):]
			if i := strings.IndexByte(rest, '\\'); i >= 0 {
				return strings.ReplaceAll(rest[i:], `\`, "/")
			}
		}
	}
	return ""
}

// WSLToUNC converts a Linux path to the \\wsl.localhost\<distro>\... form when
// running under WSL; off WSL it returns the path unchanged.
func WSLToUNC(p string) string {
	distro := os.Getenv("WSL_DISTRO_NAME")
	if distro == "" {
		return p
	}
	return `\\wsl.localhost\` + distro + strings.ReplaceAll(p, "/", `\`)
}

// CwdForDir returns the cwd Claude uses for a proj project directory. It learns
// the exact form from existing sessions when possible (authoritative), and
// constructs the UNC form otherwise.
func CwdForDir(projectDir string, sessions []Session) string {
	for _, s := range sessions {
		if s.Cwd == projectDir || UNCToWSL(s.Cwd) == projectDir {
			return s.Cwd
		}
	}
	return WSLToUNC(projectDir)
}
