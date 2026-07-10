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

// Session is the lightweight metadata for one transcript. Title is the last
// real user message and Answer is the last assistant reply.
type Session struct {
	ID       string
	Cwd      string
	Path     string
	Modified time.Time
	Messages int
	Title    string
	Answer   string
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
			cwd, title, answer, n := readMeta(f)
			out[i] = Session{
				ID:       strings.TrimSuffix(filepath.Base(f), ".jsonl"),
				Cwd:      cwd,
				Path:     f,
				Modified: info.ModTime(),
				Messages: n,
				Title:    title,
				Answer:   answer,
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
func readMeta(path string) (cwd, title, answer string, messages int) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", "", 0
	}
	messages = bytes.Count(data, userTok) + bytes.Count(data, asstTok)
	var lastPrompts, lastAnswers [][]byte
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
		if bytes.Contains(line, asstTok) {
			lastAnswers = append(lastAnswers, line)
			if len(lastAnswers) > 8 {
				lastAnswers = lastAnswers[1:]
			}
		}
	}
	return cwd, oneLine(lastPromptTitle(lastPrompts), 120), oneLine(lastAnswerText(lastAnswers), 120), messages
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

// lastAnswerText returns the most recent assistant text reply, skipping turns
// that are only tool calls or thinking (which carry no displayable text).
func lastAnswerText(lines [][]byte) string {
	for i := len(lines) - 1; i >= 0; i-- {
		var rec record
		if json.Unmarshal(lines[i], &rec) != nil || rec.Message.Role != "assistant" {
			continue
		}
		if t := cleanText(firstText(rec.Message.Content)); t != "" {
			return t
		}
	}
	return ""
}

// Prompt is one genuine user turn in a transcript, offered as a fork point.
type Prompt struct {
	Text  string // one-line cleaned preview of the user message
	CutAt int    // byte offset of the start of the next user turn (len(file) for
	// the last one); truncating the transcript at CutAt keeps this turn and the
	// assistant's reply to it
}

// Prompts returns the real user prompts in a transcript, oldest first, each with
// the byte offset at which to truncate to keep that prompt and its reply. It
// skips tool-result and synthetic (continued/compacted/caveat) user lines - the
// same ones the title extractor ignores - so the list matches what a reader
// recognises as the messages they sent.
func Prompts(path string) ([]Prompt, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var prompts []Prompt
	offset := 0
	for rest := data; len(rest) > 0; {
		lineStart := offset
		var line []byte
		if nl := bytes.IndexByte(rest, '\n'); nl >= 0 {
			line, rest = rest[:nl], rest[nl+1:]
			offset += nl + 1
		} else {
			line, rest = rest, nil
			offset = len(data)
		}
		if len(line) == 0 || !bytes.Contains(line, userTok) || bytes.Contains(line, trTok) {
			continue
		}
		var rec record
		if json.Unmarshal(line, &rec) != nil || rec.Message.Role != "user" {
			continue
		}
		text := cleanText(firstText(rec.Message.Content))
		if text == "" {
			continue
		}
		// This prompt's line begins where the previous prompt's reply ends.
		if len(prompts) > 0 {
			prompts[len(prompts)-1].CutAt = lineStart
		}
		prompts = append(prompts, Prompt{Text: text})
	}
	if len(prompts) > 0 {
		prompts[len(prompts)-1].CutAt = len(data)
	}
	return prompts, nil
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

// Adopt relocates sess into the transcript folder for targetCwd under a fresh
// id, rewriting every occurrence of the old cwd (and the internal sessionId) so
// the result is a self-consistent, independent session, then points
// .claude.json's lastSessionId for targetCwd at it so `claude -c` resumes it.
// The per-session sidecars Claude Code keys by id (the task list and the
// file-edit history) are carried over to the new id as well, so the adopted
// session keeps its tasks and history instead of resuming with an empty list.
// The source project's memory (projects/<cwd>/memory) is merged into the
// target project's memory too - always copied, never moved, since memory is
// shared by every session in a cwd and moving it would strand the others.
//
// Adoption is all-or-nothing. Every piece is copied and verified first, while
// nothing is deleted; only once the transcript, its sidecars, the project
// memory and the continue pointer are all in place does a move delete the
// originals. If any step fails, the copies made so far are rolled back and the
// originals are left exactly as they were, so a failed adopt never strands a
// duplicate transcript nor loses the source. On failure newID is "" and the
// error explains what went wrong; the caller reports it rather than claiming a
// move happened. When move is false nothing is deleted (the --copy-file
// behavior). The one non-fatal case is a move whose final removal of an
// original fails after everything else committed: newID is set and the error is
// a warning that a leftover original remains.
//
// report lists, one line each, everything the adoption did beyond the
// transcript itself - sidecars carried, memory files merged or left, source
// folders removed - so the caller can show the user exactly what happened
// instead of a bare "moved".
func Adopt(home string, sess Session, targetCwd string, move bool) (newID string, report []string, err error) {
	data, err := os.ReadFile(sess.Path)
	if err != nil {
		return "", nil, err
	}
	return transplant(home, sess, targetCwd, data, move)
}

// Fork writes the transcript of sess truncated to its first cutAt bytes as a new
// session under targetCwd, performing the same rewrite as Adopt (cwd, session
// id, sidecars, memory, continue pointer) but always as a copy: the source
// session is left intact. cutAt must fall on a record boundary, as the offsets
// from Prompts do; a cutAt of 0 or beyond the file keeps the whole transcript.
func Fork(home string, sess Session, targetCwd string, cutAt int) (newID string, report []string, err error) {
	data, err := os.ReadFile(sess.Path)
	if err != nil {
		return "", nil, err
	}
	if cutAt > 0 && cutAt < len(data) {
		data = data[:cutAt]
	}
	return transplant(home, sess, targetCwd, data, false)
}

// transplant writes data (a full or truncated transcript of sess) as a new
// session under targetCwd, rewriting the embedded cwd, session id, sidecars,
// memory, and continue pointer. With move set it deletes the source once the
// copy is verified; otherwise the source is left in place.
func transplant(home string, sess Session, targetCwd string, data []byte, move bool) (newID string, report []string, err error) {
	targetDir := filepath.Join(home, "projects", EncodeCwd(targetCwd))
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return "", nil, err
	}
	if sess.Cwd != "" && sess.Cwd != targetCwd {
		data = bytes.ReplaceAll(data, []byte(jsonInner(sess.Cwd)), []byte(jsonInner(targetCwd)))
		// The transcript also embeds the cwd in its ENCODED form: harness
		// reminders name the project memory directory
		// (.claude/projects/<EncodeCwd(cwd)>/memory/...). Left stale, a resumed
		// session keeps writing memory into the old project's folder - for a
		// temp-dir session that silently recreates the deleted temp folder.
		// Encoded names are [A-Za-z0-9-] only, so a plain byte replace is safe.
		data = bytes.ReplaceAll(data, []byte(EncodeCwd(sess.Cwd)), []byte(EncodeCwd(targetCwd)))
	}
	newID = NewSessionID()
	// Replace every occurrence of the id, not just the "sessionId" key: the
	// transcript also embeds it in sidecar paths (.claude/tasks/<id>/...) and
	// memory frontmatter (originSessionId), which would otherwise point a
	// resumed session at folders that no longer exist. A UUID cannot collide
	// with unrelated text.
	data = bytes.ReplaceAll(data, []byte(sess.ID), []byte(newID))
	dst := filepath.Join(targetDir, newID+".jsonl")
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return "", nil, err
	}
	// Prove the copy is actually on disk and intact before touching the original.
	if got, rerr := os.ReadFile(dst); rerr != nil || !bytes.Equal(got, data) {
		_ = os.Remove(dst)
		return "", nil, fmt.Errorf("could not verify the copied transcript landed at %s; left the original in place", dst)
	}

	// Everything below is non-destructive: copies only, each verified. Track the
	// artifacts created so any failure can undo them, leaving the originals as
	// they were. rollback removes the new transcript and any sidecar copies.
	var createdSidecars []string
	rollback := func() {
		_ = os.Remove(dst)
		for _, d := range createdSidecars {
			_ = os.RemoveAll(d)
		}
	}

	createdSidecars, err = copySidecars(home, sess.ID, newID)
	if err != nil {
		rollback()
		return "", nil, fmt.Errorf("could not carry over the session's tasks/history; left the original in place: %w", err)
	}
	for _, d := range createdSidecars {
		report = append(report, fmt.Sprintf("carried %s to the new session id", filepath.Base(filepath.Dir(d))))
	}
	memoryCarried, memNotes, err := copyProjectMemory(home, sess.Cwd, targetCwd, sess.ID, newID)
	if err != nil {
		rollback()
		return "", nil, fmt.Errorf("could not carry over project memory; left the original in place: %w", err)
	}
	report = append(report, memNotes...)
	if err := PointLastSession(home, targetCwd, newID); err != nil {
		rollback()
		return "", nil, fmt.Errorf("could not update the continue pointer; left the original in place: %w", err)
	}

	// Commit. Everything is copied and verified, so a move now deletes the
	// originals. A failure here has already published the new session, so it is a
	// warning about a leftover original, not a rollback.
	if move {
		removeSidecarSources(home, sess.ID)
		if err := os.Remove(sess.Path); err != nil {
			return newID, report, fmt.Errorf("adopted the session but could not remove the original transcript %s (a duplicate remains): %w", sess.ID[:8], err)
		}
		if sess.Cwd != "" && EncodeCwd(sess.Cwd) != EncodeCwd(targetCwd) {
			report = append(report, cleanupSourceProject(home, sess.Cwd, memoryCarried)...)
		}
	}
	return newID, report, nil
}

// copySidecars copies the per-session folders Claude Code keys by session id -
// the task list (tasks/<id>) and the file-edit history (file-history/<id>) -
// from oldID to newID, verifying each file byte-for-byte. It never deletes the
// source: the caller removes the originals only after the whole adoption
// commits, so a failure here can be rolled back. It returns the destination
// folders it created, so the caller can undo them on a later failure. Folders
// that do not exist are skipped. Without this an adopted session resumes with
// an empty task list because its tasks still sit under the old id.
func copySidecars(home, oldID, newID string) (created []string, err error) {
	for _, kind := range []string{"tasks", "file-history"} {
		src := filepath.Join(home, kind, oldID)
		if _, statErr := os.Stat(src); statErr != nil {
			continue // nothing to carry over for this kind
		}
		dst := filepath.Join(home, kind, newID)
		if cerr := copyDir(src, dst); cerr != nil {
			return created, fmt.Errorf("%s: %w", kind, cerr)
		}
		created = append(created, dst)
	}
	return created, nil
}

// removeSidecarSources deletes the old-id task and file-history folders. Called
// only after an adoption has fully committed, to complete a move.
func removeSidecarSources(home, oldID string) {
	for _, kind := range []string{"tasks", "file-history"} {
		_ = os.RemoveAll(filepath.Join(home, kind, oldID))
	}
}

// copyProjectMemory merges the source project's memory folder into the target
// project's, so an adopted session does not resume having forgotten everything
// the project taught it. Memory is keyed by cwd (projects/<cwd>/memory) and is
// shared by every session in that cwd, so this only ever copies; whether the
// source folder may then be deleted is the caller's decision, based on the
// carried result: true when every source memory file is now fully represented
// at the target, byte-for-byte (fact files verified by read-back, the MEMORY.md
// index by a successful merge). A fact file whose name exists at the target
// with different content is kept on both sides (the target's knowledge wins),
// reported in notes, and makes carried false so the source is never deleted
// while it still holds the only copy of something. A missing source memory
// folder is a no-op with carried true.
//
// Occurrences of oldID in copied files are rewritten to newID: memory
// frontmatter records the creating session (originSessionId), and after a move
// oldID names a session that no longer exists. Other sessions' ids are left
// alone.
func copyProjectMemory(home, oldCwd, newCwd, oldID, newID string) (carried bool, notes []string, err error) {
	if oldCwd == "" || oldCwd == newCwd {
		return true, nil, nil
	}
	srcMem := filepath.Join(home, "projects", EncodeCwd(oldCwd), "memory")
	entries, err := os.ReadDir(srcMem)
	if err != nil {
		return true, nil, nil // no memory folder to carry over
	}
	dstMem := filepath.Join(home, "projects", EncodeCwd(newCwd), "memory")
	if err := os.MkdirAll(dstMem, 0o755); err != nil {
		return false, nil, err
	}
	carried = true
	for _, e := range entries {
		if e.IsDir() {
			carried = false // memory is a flat folder of markdown files
			notes = append(notes, fmt.Sprintf("memory: left unexpected directory %s in the source", e.Name()))
			continue
		}
		src, dst := filepath.Join(srcMem, e.Name()), filepath.Join(dstMem, e.Name())
		if e.Name() == "MEMORY.md" {
			if err := mergeMemoryIndex(src, dst); err != nil {
				return false, notes, err
			}
			continue
		}
		data, err := os.ReadFile(src)
		if err != nil {
			return false, notes, err
		}
		data = bytes.ReplaceAll(data, []byte(oldID), []byte(newID))
		if existing, err := os.ReadFile(dst); err == nil {
			if !bytes.Equal(existing, data) {
				carried = false // target's knowledge wins; source keeps the only copy of its version
				notes = append(notes, fmt.Sprintf("memory: kept the target's %s (source version differs, left in the source)", e.Name()))
			}
			continue
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return false, notes, err
		}
		// Verify the copy landed intact; only a verified copy may later justify
		// deleting the source folder.
		if got, rerr := os.ReadFile(dst); rerr != nil || !bytes.Equal(got, data) {
			return false, notes, fmt.Errorf("could not verify copied memory file %s", dst)
		}
	}
	return carried, notes, nil
}

// cleanupSourceProject removes the source project's folder after a move, but
// only what is provably redundant: it acts only when no other session
// transcript remains there, deletes the memory folder only when memoryCarried
// says every memory file is fully represented at the target, and removes the
// folder itself only when empty. It returns what it did (or why it left things)
// so the caller can show the user; it never fails the adoption.
func cleanupSourceProject(home, oldCwd string, memoryCarried bool) (actions []string) {
	folder := filepath.Join(home, "projects", EncodeCwd(oldCwd))
	if left, _ := filepath.Glob(filepath.Join(folder, "*.jsonl")); len(left) > 0 {
		return nil // other sessions still live in this cwd; their folder stays
	}
	if memoryCarried {
		memDir := filepath.Join(folder, "memory")
		if _, err := os.Stat(memDir); err == nil {
			if err := os.RemoveAll(memDir); err == nil {
				actions = append(actions, "removed the source project's memory folder (all of it verified at the target)")
			}
		}
	} else {
		actions = append(actions, "left the source project's memory folder (not everything is at the target)")
	}
	if err := os.Remove(folder); err == nil {
		actions = append(actions, "removed the now-empty source project folder")
	}
	return actions
}

// mergeMemoryIndex appends the pointer lines from the source MEMORY.md to the
// target's, dropping blanks and exact-duplicate lines so re-adopting is
// idempotent and the target's existing entries are preserved.
func mergeMemoryIndex(src, dst string) error {
	srcData, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	dstData, _ := os.ReadFile(dst) // absent target is fine: start empty
	seen := make(map[string]bool)
	var lines []string
	for _, block := range [][]byte{dstData, srcData} {
		for _, ln := range strings.Split(string(block), "\n") {
			if strings.TrimSpace(ln) == "" || seen[ln] {
				continue
			}
			seen[ln] = true
			lines = append(lines, ln)
		}
	}
	return os.WriteFile(dst, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}

// copyDir recursively copies the directory tree at src into dst, preserving
// file permission bits and reading each file back to confirm it landed intact.
func copyDir(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	for _, e := range entries {
		s, d := filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())
		if e.IsDir() {
			if err := copyDir(s, d); err != nil {
				return err
			}
			continue
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(s)
		if err != nil {
			return err
		}
		if err := os.WriteFile(d, data, info.Mode().Perm()); err != nil {
			return err
		}
		// Verify the copy landed intact before any caller deletes the original.
		if got, rerr := os.ReadFile(d); rerr != nil || !bytes.Equal(got, data) {
			return fmt.Errorf("could not verify copied file %s", d)
		}
	}
	return nil
}

// NewSessionID returns a random UUIDv4, matching the id format Claude gives
// natively created sessions.
func NewSessionID() string {
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
		newPath := filepath.Join(newFolder, e.Name())
		if os.WriteFile(newPath, data, 0o644) != nil {
			continue
		}
		// Verify the rewritten copy landed intact before deleting the original:
		// the transcripts live on a flaky 9p mount, so a silent short write must
		// not cost the only copy.
		if got, err := os.ReadFile(newPath); err != nil || !bytes.Equal(got, data) {
			continue
		}
		_ = os.Remove(filepath.Join(oldFolder, e.Name()))
	}
}

// jsonInner returns the JSON encoding of s without the surrounding quotes, so it
// matches the escaped form a path takes inside a transcript.
func jsonInner(s string) string {
	b, _ := json.Marshal(s)
	return string(b[1 : len(b)-1])
}

// PointLastSession sets projects[cwd].lastSessionId in <home>/../.claude.json.
// It round-trips the file with json.Number so numeric fields are preserved; key
// order is not (the app rewrites this file itself, so that is harmless).
func PointLastSession(home, cwd, id string) error {
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

// LocalDir returns a path the Linux side can chdir into for a session's cwd: a
// \\wsl.localhost\... path becomes its native Linux path, a Windows drive path
// (C:\...) becomes its /mnt mount, and a plain Linux path is returned as-is.
func LocalDir(cwd string) string {
	if w := UNCToWSL(cwd); w != "" {
		return w
	}
	if len(cwd) >= 2 && cwd[1] == ':' {
		return winPathToWSL(cwd)
	}
	return cwd
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
