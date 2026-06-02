// Package sessions reads and relocates Claude Code session transcripts.
//
// Claude stores one JSONL transcript per session under
// <home>/projects/<encoded-cwd>/<session-id>.jsonl, where <encoded-cwd> is the
// session's working directory with every non-alphanumeric rune replaced by '-'.
// Each record also carries the real `cwd`, so this package reads paths straight
// from the transcript rather than reversing the lossy folder encoding.
package sessions

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
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

// List returns every session under home, newest first. Transcripts are read
// concurrently: they live on a slow filesystem (the Windows .claude over the WSL
// 9p mount), so overlapping the reads is what dominates wall-clock.
func List(home string) ([]Session, error) {
	files, err := filepath.Glob(filepath.Join(home, "projects", "*", "*.jsonl"))
	if err != nil {
		return nil, err
	}
	out := make([]Session, len(files))
	sem := make(chan struct{}, 16)
	var wg sync.WaitGroup
	for i, f := range files {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, f string) {
			defer wg.Done()
			defer func() { <-sem }()
			info, err := os.Stat(f)
			if err != nil {
				return
			}
			cwd, title, n := readMeta(f)
			out[i] = Session{
				ID:       strings.TrimSuffix(filepath.Base(f), ".jsonl"),
				Cwd:      cwd,
				Path:     f,
				Modified: info.ModTime(),
				Messages: n,
				Title:    title,
			}
		}(i, f)
	}
	wg.Wait()
	res := make([]Session, 0, len(out))
	for _, s := range out {
		if s.ID != "" {
			res = append(res, s)
		}
	}
	sort.Slice(res, func(i, j int) bool { return res[i].Modified.After(res[j].Modified) })
	return res, nil
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

var (
	userTok = []byte(`"type":"user"`)
	asstTok = []byte(`"type":"assistant"`)
	cwdTok  = []byte(`"cwd"`)
	trTok   = []byte(`"tool_result"`)
)

// readMeta extracts a transcript's cwd, message count, and title with as little
// JSON parsing as possible: the count is a substring scan, and only the cwd line
// and the last few genuine user prompts are unmarshalled. Tool-result lines
// (which carry no prompt text and are often the largest) are skipped.
func readMeta(path string) (cwd, title string, messages int) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", 0
	}
	messages = bytes.Count(data, userTok) + bytes.Count(data, asstTok)
	var lastPrompts [][]byte
	for rest := data; len(rest) > 0; {
		var line []byte
		if nl := bytes.IndexByte(rest, '\n'); nl >= 0 {
			line, rest = rest[:nl], rest[nl+1:]
		} else {
			line, rest = rest, nil
		}
		if len(line) == 0 {
			continue
		}
		if cwd == "" && bytes.Contains(line, cwdTok) {
			var rec record
			if json.Unmarshal(line, &rec) == nil {
				cwd = rec.Cwd
			}
		}
		if bytes.Contains(line, userTok) && !bytes.Contains(line, trTok) {
			lastPrompts = append(lastPrompts, line)
			if len(lastPrompts) > 8 {
				lastPrompts = lastPrompts[1:]
			}
		}
	}
	return cwd, oneLine(lastPromptTitle(lastPrompts), 56), messages
}

// lastPromptTitle parses the kept user lines newest-first and returns the most
// recent prose prompt, falling back to the most recent non-empty text.
func lastPromptTitle(lines [][]byte) string {
	var fallback string
	for i := len(lines) - 1; i >= 0; i-- {
		var rec record
		if json.Unmarshal(lines[i], &rec) != nil || rec.Message.Role != "user" {
			continue
		}
		t := cleanText(firstText(rec.Message.Content))
		if t == "" {
			continue
		}
		if letterRatio(t) > 0.5 {
			return t
		}
		if fallback == "" {
			fallback = t
		}
	}
	if fallback != "" {
		return fallback
	}
	return "(no prompt)"
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
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}))
	// Claude injects these as user-role messages; they are not real prompts.
	for _, noise := range []string{"This session is being continued", "Conversation compacted", "Caveat:"} {
		if strings.HasPrefix(s, noise) {
			return ""
		}
	}
	return s
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
func Adopt(home string, sess Session, targetCwd string) (newID string, err error) {
	targetDir := filepath.Join(home, "projects", EncodeCwd(targetCwd))
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return "", err
	}
	data, err := os.ReadFile(sess.Path)
	if err != nil {
		return "", err
	}
	// The copy is an independent session: rewrite the cwd to the target project
	// and give it a fresh id so it does not collide with the original (which
	// stays put). Claude resumes by the filename id, and rewriting every
	// internal sessionId to match keeps the transcript self-consistent.
	if sess.Cwd != "" && sess.Cwd != targetCwd {
		data = bytes.ReplaceAll(data, []byte(jsonInner(sess.Cwd)), []byte(jsonInner(targetCwd)))
	}
	newID = newSessionID()
	data = bytes.ReplaceAll(data,
		[]byte(`"sessionId":"`+sess.ID+`"`),
		[]byte(`"sessionId":"`+newID+`"`))
	dst := filepath.Join(targetDir, newID+".jsonl")
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return "", err
	}
	if err := pointLastSession(home, targetCwd, newID); err != nil {
		return newID, fmt.Errorf("copied transcript but could not update the continue pointer: %w", err)
	}
	return newID, nil
}

// newSessionID returns a random UUIDv4, matching the id format Claude gives
// natively created sessions.
func newSessionID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// MigrateHistory moves the Claude transcript folder for a project being renamed
// from oldDir to newDir, rewriting each transcript's cwd to the new path. It is
// best-effort and a no-op when there is nothing under the old location (so it
// safely does nothing on setups where the project's history lives elsewhere).
func MigrateHistory(home, oldDir, newDir string) {
	all, _ := List(home)
	oldCwd := CwdForDir(oldDir, all)
	newCwd := CwdForDir(newDir, all)
	if oldCwd == newCwd {
		return
	}
	oldFolder := filepath.Join(home, "projects", EncodeCwd(oldCwd))
	newFolder := filepath.Join(home, "projects", EncodeCwd(newCwd))
	entries, err := os.ReadDir(oldFolder)
	if err != nil {
		return
	}
	if err := os.MkdirAll(newFolder, 0o755); err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(oldFolder, e.Name()))
		if err != nil {
			continue
		}
		data = bytes.ReplaceAll(data, []byte(jsonInner(oldCwd)), []byte(jsonInner(newCwd)))
		if os.WriteFile(filepath.Join(newFolder, e.Name()), data, 0o644) == nil {
			_ = os.Remove(filepath.Join(oldFolder, e.Name()))
		}
	}
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
