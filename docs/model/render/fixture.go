package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A fixture is a whole machine as proj sees one: a config, a directory of
// projects, a registry, daemon state, a Claude home, a Codex home, and stub
// tmux and stty commands standing in for the terminal the picture does not
// have. Every render runs against one of these rather than the real machine,
// so no picture can leak a session or change with the day's work.

type fixtureProject struct {
	name      string
	tags      []string
	tool      string
	model     string // written as that project's Claude settings model
	turnModel string // model recorded on its last turn, when it differs
	ageDays   float64
	alive     bool
	pinned    bool
	sessions  []fixtureSession
}

type fixtureSession struct {
	title    string
	prompt   string
	answer   string
	ageHours float64
}

var fleet = []fixtureProject{
	{name: "Arbay", tags: []string{"kotlin", "doner"}, ageDays: 0.02, alive: true, turnModel: "claude-sonnet-5",
		sessions: []fixtureSession{{
			title:    "Arbay @book [kotlin,doner]",
			prompt:   "the price band should filter the list, not just colour it",
			answer:   "Filtering now happens in the query, so the count under the band matches what the list shows.",
			ageHours: 0.3,
		}}},
	{name: "papercut", tags: []string{"python"}, tool: "codex", ageDays: 0.1, alive: true},
	{name: "proj", tags: []string{"go"}, ageDays: 0.005, alive: true,
		sessions: []fixtureSession{{
			title:    "proj @book [go]",
			prompt:   "make the model column say what a project will run, not what it ran",
			answer:   "The column reads the configured model now and falls back to the last turn.",
			ageHours: 0.05,
		}}},
	{name: "tldr", tags: []string{"python"}, ageDays: 2, pinned: true, alive: true,
		sessions: []fixtureSession{{
			title:    "tldr @book [python]",
			prompt:   "summarise the release notes into three lines",
			answer:   "Three lines, and the fourth one you asked me to drop is gone.",
			ageHours: 41,
		}}},
	{name: "nasx", tags: []string{"nix"}, ageDays: 0.6,
		sessions: []fixtureSession{{
			title:    "nasx @book [nix]",
			prompt:   "the backup job is silent, is it running at all?",
			answer:   "It ran; the unit logs to a file nothing tails. Timer output goes to the journal now.",
			ageHours: 14,
		}}},
	{name: "tv", tags: []string{"nix"}, model: "claude-sonnet-5", ageDays: 7},
	{name: "claudemd", ageDays: 8},
	{name: "phonetix", tags: []string{"extension"}, ageDays: 9},
	{name: "kicad-parts", ageDays: 40},
}

// sessionIDs give each fixture project's transcript the shape of a real
// Claude session id, since the sessions view shows the id.
var sessionIDs = map[string]string{
	"Arbay": "6f1c2c4a",
	"proj":  "b28d0e71",
	"tldr":  "3a7c91d0",
	"nasx":  "e41b58aa",
}

func buildFixture(kind, home string) error {
	projectsDir := filepath.Join(home, "projects")
	for _, dir := range []string{
		filepath.Join(home, ".config", "proj"),
		filepath.Join(home, ".local", "state", "proj"),
		filepath.Join(home, ".claude", "projects"),
		filepath.Join(home, ".codex", "sessions", "2026", "07", "24"),
		filepath.Join(home, "bin"),
		projectsDir,
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	if err := writeStubs(home); err != nil {
		return err
	}

	if kind == "broken-config" {
		// A config that does not parse is the failure every command shares:
		// nothing proj does can start without it.
		return os.WriteFile(filepath.Join(home, ".config", "proj", "config.toml"),
			[]byte("base_dir = [\"one\"\ntools = broken\n"), 0o644)
	}

	config := fmt.Sprintf(`base_dir = "%s"

[claude]
  command = "claude --dangerously-skip-permissions"
  resume_flag = "-c"

[list]
  max_age_days = 14
`, projectsDir)
	if err := os.WriteFile(filepath.Join(home, ".config", "proj", "config.toml"), []byte(config), 0o644); err != nil {
		return err
	}

	settings := map[string]any{"model": "claude-opus-5"}
	if kind != "no-doner" {
		settings["hooks"] = map[string]any{"Stop": []any{map[string]any{
			"hooks": []any{map[string]any{"type": "command", "command": "proj doner-hook"}},
		}}}
	}
	if err := writeJSON(filepath.Join(home, ".claude", "settings.json"), settings); err != nil {
		return err
	}

	if kind == "no-projects" {
		return nil
	}

	projects := fleet
	if kind == "no-doner" {
		projects = withoutTag(projects, "doner")
	}
	registry := map[string]map[string]any{}
	managed := map[string]any{}
	for _, p := range projects {
		dir := filepath.Join(projectsDir, p.name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		meta := map[string]any{}
		if len(p.tags) > 0 {
			meta["tags"] = p.tags
		}
		if p.tool != "" {
			meta["tool"] = p.tool
		}
		if len(meta) > 0 {
			registry[p.name] = meta
		}
		if p.model != "" {
			if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0o755); err != nil {
				return err
			}
			if err := writeJSON(filepath.Join(dir, ".claude", "settings.json"), map[string]any{"model": p.model}); err != nil {
				return err
			}
		}
		if p.pinned {
			managed[sessionName(p)] = map[string]any{"name": sessionName(p), "dir": dir, "pinned": true}
		}
		if kind != "no-sessions" {
			if err := writeTranscripts(home, dir, p); err != nil {
				return err
			}
		}
		if p.tool == "codex" {
			if err := writeCodexRollout(home, dir); err != nil {
				return err
			}
		}
	}

	if err := writeRegistry(filepath.Join(home, ".config", "proj", "projects.toml"), registry); err != nil {
		return err
	}
	if len(managed) > 0 {
		if err := writeJSON(filepath.Join(home, ".local", "state", "proj", "daemon-sessions.json"), managed); err != nil {
			return err
		}
	}
	if err := writeTmuxState(home, projects, projectsDir); err != nil {
		return err
	}
	// A project's age is its directory's mtime, so the ages are stamped after
	// everything inside the directories has been written.
	for _, p := range projects {
		stamp := time.Now().Add(-time.Duration(p.ageDays * float64(24*time.Hour)))
		if err := os.Chtimes(filepath.Join(projectsDir, p.name), stamp, stamp); err != nil {
			return err
		}
	}
	return nil
}

func withoutTag(projects []fixtureProject, tag string) []fixtureProject {
	out := make([]fixtureProject, 0, len(projects))
	for _, p := range projects {
		kept := p
		kept.tags = nil
		for _, t := range p.tags {
			if t != tag {
				kept.tags = append(kept.tags, t)
			}
		}
		out = append(out, kept)
	}
	return out
}

func sessionName(p fixtureProject) string {
	if len(p.tags) == 0 {
		return p.name
	}
	return p.name + "@" + strings.Join(p.tags, "+")
}

// writeStubs writes the two commands proj shells out to that a picture cannot
// have: a tmux holding the fixture's sessions, and an stty reporting the shape
// the render is being taken in.
func writeStubs(home string) error {
	// display-message answers the one question proj asks of a pane it did not
	// list: where that pane is. Without it a live session has no directory, and
	// everything keyed on the directory (the model a session is running, above
	// all) silently finds nothing.
	tmux := `#!/bin/sh
state="$HOME/.local/state/proj/tmux"
case "$1" in
  list-sessions) cat "$state/sessions" 2>/dev/null ;;
  list-panes)    cat "$state/panes" 2>/dev/null ;;
  capture-pane)  ;;
  display-message)
    for arg in "$@"; do
      case "$prev" in -t) pane="$arg" ;; esac
      prev="$arg"
    done
    grep "^$pane	" "$state/paths" 2>/dev/null | cut -f2
    ;;
  has-session)   exit 1 ;;
esac
exit 0
`
	stty := `#!/bin/sh
[ "$1" = "size" ] && echo "$PROJ_RENDER_SIZE"
exit 0
`
	for name, body := range map[string]string{"tmux": tmux, "stty": stty} {
		if err := os.WriteFile(filepath.Join(home, "bin", name), []byte(body), 0o755); err != nil {
			return err
		}
	}
	return nil
}

func writeTmuxState(home string, projects []fixtureProject, projectsDir string) error {
	dir := filepath.Join(home, ".local", "state", "proj", "tmux")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var sessions, panes, paths []string
	for i, p := range projects {
		if !p.alive {
			continue
		}
		name := sessionName(p)
		projectDir := filepath.Join(projectsDir, p.name)
		activity := time.Now().Add(-time.Duration(p.ageDays * float64(24*time.Hour))).Unix()
		sessions = append(sessions, fmt.Sprintf("%s\t%s\t%d", name, projectDir, activity))
		panes = append(panes, fmt.Sprintf("%s\t%%%d", name, i+1))
		paths = append(paths, fmt.Sprintf("%%%d\t%s", i+1, projectDir))
	}
	for file, lines := range map[string][]string{"sessions": sessions, "panes": panes, "paths": paths} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// writeTranscripts lays down the Claude session files a project's history is
// read from: what was asked, what came back, and when.
func writeTranscripts(home, dir string, p fixtureProject) error {
	if len(p.sessions) == 0 {
		return nil
	}
	encoded := strings.ReplaceAll(dir, "/", "-")
	root := filepath.Join(home, ".claude", "projects", encoded)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	for i, s := range p.sessions {
		stamp := time.Now().Add(-time.Duration(s.ageHours * float64(time.Hour)))
		iso := stamp.UTC().Format(time.RFC3339Nano)
		lines := []string{
			jsonLine(map[string]any{"type": "custom-title", "customTitle": s.title}),
			jsonLine(map[string]any{
				"type": "user", "cwd": dir, "timestamp": iso,
				"message": map[string]any{"role": "user", "content": s.prompt},
			}),
			jsonLine(map[string]any{
				"type": "assistant", "cwd": dir, "timestamp": iso,
				"message": map[string]any{
					"role": "assistant", "model": turnModel(p),
					"content": []any{map[string]any{"type": "text", "text": s.answer}},
				},
			}),
		}
		id := sessionIDs[p.name]
		if id == "" {
			id = "0a1b2c3d"
		}
		path := filepath.Join(root, fmt.Sprintf("%s-4f%02d-a1b2-9c3d-7e5f10a2b3c4.jsonl", id, i))
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			return err
		}
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			return err
		}
	}
	return nil
}

// turnModel is the model a project's last turn was answered by, which is what
// a running session is still on until it restarts.
func turnModel(p fixtureProject) string {
	if p.turnModel != "" {
		return p.turnModel
	}
	return "claude-opus-5"
}

func writeCodexRollout(home, dir string) error {
	stamp := time.Now().Add(-90 * time.Minute)
	path := filepath.Join(home, ".codex", "sessions", "2026", "07", "24", "rollout-fixture.jsonl")
	lines := []string{
		jsonLine(map[string]any{"type": "session_meta", "payload": map[string]any{
			"id": "fixture", "cwd": dir, "model_provider": "openai", "cli_version": "0.139.0",
		}}),
		jsonLine(map[string]any{"type": "turn_context", "payload": map[string]any{"model": "gpt-5.6-terra", "cwd": dir}}),
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		return err
	}
	return os.Chtimes(path, stamp, stamp)
}

func writeRegistry(path string, registry map[string]map[string]any) error {
	var b strings.Builder
	for name, meta := range registry {
		fmt.Fprintf(&b, "[projects.%q]\n", name)
		if tags, ok := meta["tags"].([]string); ok {
			fmt.Fprintf(&b, "  tags = [%s]\n", quotedList(tags))
		}
		if tool, ok := meta["tool"].(string); ok {
			fmt.Fprintf(&b, "  tool = %q\n", tool)
		}
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func quotedList(items []string) string {
	quoted := make([]string, len(items))
	for i, s := range items {
		quoted[i] = fmt.Sprintf("%q", s)
	}
	return strings.Join(quoted, ", ")
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func jsonLine(value any) string {
	data, _ := json.Marshal(value)
	return string(data)
}
