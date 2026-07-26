package handoff

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// scanLines iterates a jsonl file line by line with a buffer large enough for
// transcript records (claude tool results run to hundreds of KB).
func scanLines(path string, fn func([]byte)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		fn(sc.Bytes())
	}
	return sc.Err()
}

func newTranscript(tool, cwd string, turns []Turn) *Transcript {
	return &Transcript{
		Version:     1,
		SourceTool:  tool,
		Cwd:         cwd,
		ExtractedAt: time.Now().Format(time.RFC3339),
		Turns:       turns,
	}
}

// ReadClaude parses a Claude Code session transcript (jsonl) into the IR.
// Kept: user text (except injected "<...>" system lines), assistant text
// blocks, and tool_use blocks flattened to name + compact input. Dropped:
// thinking blocks (opaque), tool results (bulky, the action line carries the
// intent), sidechains, meta records, and bookkeeping record types.
func ReadClaude(path, cwd string) (*Transcript, error) {
	var turns []Turn
	err := scanLines(path, func(line []byte) {
		var rec struct {
			Type        string `json:"type"`
			IsMeta      bool   `json:"isMeta"`
			IsSidechain bool   `json:"isSidechain"`
			Message     struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &rec) != nil || rec.IsMeta || rec.IsSidechain {
			return
		}
		switch rec.Type {
		case "user":
			var s string
			if json.Unmarshal(rec.Message.Content, &s) == nil {
				if s != "" && !strings.HasPrefix(strings.TrimSpace(s), "<") {
					turns = append(turns, Turn{Role: "user", Text: capText(s)})
				}
				return
			}
			var blocks []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal(rec.Message.Content, &blocks) == nil {
				for _, b := range blocks {
					if b.Type == "text" && b.Text != "" && !strings.HasPrefix(strings.TrimSpace(b.Text), "<") {
						turns = append(turns, Turn{Role: "user", Text: capText(b.Text)})
					}
				}
			}
		case "assistant":
			var blocks []struct {
				Type  string          `json:"type"`
				Text  string          `json:"text"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			}
			if json.Unmarshal(rec.Message.Content, &blocks) != nil {
				return
			}
			for _, b := range blocks {
				switch b.Type {
				case "text":
					if b.Text != "" {
						turns = append(turns, Turn{Role: "assistant", Text: capText(b.Text)})
					}
				case "tool_use":
					turns = append(turns, Turn{Role: "tool", Name: b.Name, Text: compactJSON(b.Input)})
				}
			}
		}
	})
	if err != nil {
		return nil, err
	}
	return newTranscript("claude", cwd, turns), nil
}

// agentsInstructionsHeading opens the block Codex builds from the AGENTS.md
// files in scope and sends as the first user message of a thread.
const agentsInstructionsHeading = "# AGENTS.md instructions"

// ReadCodex parses a Codex rollout into the IR. A rollout carries the
// conversation twice: as response_item messages, which also hold threads
// restored through app-server injection, and as the event_msg stream, which
// older rollouts have alone. Only one of the two feeds the IR, while tool
// calls, recorded once, join whichever wins.
func ReadCodex(path, cwd string) (*Transcript, error) {
	var messages []Turn
	var events []Turn
	responseMessages := false
	err := scanLines(path, func(line []byte) {
		var rec struct {
			Type    string `json:"type"`
			Payload struct {
				Type      string          `json:"type"`
				Role      string          `json:"role"`
				Message   string          `json:"message"`
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
				Input     string          `json:"input"`
				Content   []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &rec) != nil {
			return
		}
		switch {
		case rec.Type == "event_msg" && rec.Payload.Type == "user_message":
			events = append(events, Turn{Role: "user", Text: capText(rec.Payload.Message)})
		case rec.Type == "event_msg" && rec.Payload.Type == "agent_message":
			events = append(events, Turn{Role: "assistant", Text: capText(rec.Payload.Message)})
		case rec.Type == "response_item" && rec.Payload.Type == "message":
			for _, content := range rec.Payload.Content {
				if content.Text == "" || (content.Type != "input_text" && content.Type != "output_text") {
					continue
				}
				text := strings.TrimSpace(content.Text)
				// Codex prepends its own blocks to the user side of the
				// response stream: tagged ones (environment context, plugin
				// listings) and the AGENTS.md instructions it inlines under
				// that heading. Both are harness input, not conversation.
				if rec.Payload.Role == "user" && (strings.HasPrefix(text, "<") || strings.HasPrefix(text, agentsInstructionsHeading)) {
					continue
				}
				if rec.Payload.Role == "user" || rec.Payload.Role == "assistant" {
					messages = append(messages, Turn{Role: rec.Payload.Role, Text: capText(content.Text)})
					responseMessages = true
				}
			}
		case rec.Type == "response_item" && rec.Payload.Type == "function_call":
			tool := Turn{Role: "tool", Name: rec.Payload.Name, Text: compactJSON(rec.Payload.Arguments)}
			messages = append(messages, tool)
			events = append(events, tool)
		case rec.Type == "response_item" && rec.Payload.Type == "custom_tool_call":
			// Freeform tools (exec, apply_patch) carry their call as a raw
			// string rather than a JSON argument object.
			tool := Turn{Role: "tool", Name: rec.Payload.Name, Text: capText(rec.Payload.Input)}
			messages = append(messages, tool)
			events = append(events, tool)
		}
	})
	if err != nil {
		return nil, err
	}
	if responseMessages {
		return newTranscript("codex", cwd, messages), nil
	}
	return newTranscript("codex", cwd, events), nil
}

// ReadAgy builds an IR from agy's history.jsonl, which records one line per
// prompt with the workspace it ran in. Assistant replies live only in agy's
// protobuf conversation store and are not recoverable, so the transcript
// carries user prompts alone; the rendered prompt still orients the target tool.
func ReadAgy(historyPath, cwd string) (*Transcript, error) {
	var turns []Turn
	err := scanLines(historyPath, func(line []byte) {
		var rec struct {
			Display   string `json:"display"`
			Workspace string `json:"workspace"`
		}
		if json.Unmarshal(line, &rec) == nil && rec.Workspace == cwd && rec.Display != "" {
			turns = append(turns, Turn{Role: "user", Text: capText(rec.Display)})
		}
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return newTranscript("agy", cwd, turns), nil
}

// AgyHistoryPath returns agy's prompt history file.
func AgyHistoryPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".gemini", "antigravity-cli", "history.jsonl")
}

// CodexHome returns the Codex home directory ($CODEX_HOME, default ~/.codex).
func CodexHome() string {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return h
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex")
}

// RecentCodexRollout returns the newest rollout transcript recorded for cwd,
// or "". Codex keys rollouts by date; the cwd sits in each file's session_meta
// head line.
func RecentCodexRollout(cwd string) string {
	root := filepath.Join(CodexHome(), "sessions")
	var best string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		name := d.Name()
		if !strings.HasPrefix(name, "rollout-") || !strings.HasSuffix(name, ".jsonl") {
			return nil
		}
		// The rollout's date is in its path (sessions/YYYY/MM/DD/rollout-<ts>...),
		// which orders chronologically; comparing paths is stable where mtimes
		// tie (files written in the same instant on a fast filesystem).
		if path <= best || rolloutCwd(path) != cwd {
			return nil
		}
		best = path
		return nil
	})
	return best
}

// rolloutCwd extracts the cwd from a rollout's session_meta head line.
func rolloutCwd(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	if !sc.Scan() {
		return ""
	}
	var rec struct {
		Type    string `json:"type"`
		Payload struct {
			Cwd string `json:"cwd"`
		} `json:"payload"`
	}
	if json.Unmarshal(sc.Bytes(), &rec) != nil || rec.Type != "session_meta" {
		return ""
	}
	return rec.Payload.Cwd
}
