package handoff

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tieo/proj/internal/sessions"
)

// WriteClaude emits the IR as a Claude Code session for dir, so the next
// `claude -c` there resumes it natively. claudeHome is the resolved Claude
// home (sessions.Home). Tool turns render as bracketed assistant text; claude
// cannot be handed foreign tool_use records (its API validates tool names and
// ids), and a text line preserves the information.
//
// Records carry the uuid/parentUuid chain and per-record sessionId/cwd claude
// writes itself; .claude.json's lastSessionId is pointed at the new session,
// which is what `claude -c` resumes.
func WriteClaude(t *Transcript, claudeHome, dir, artifactPath string) (string, error) {
	id := sessions.NewSessionID()
	folder := filepath.Join(claudeHome, "projects", sessions.EncodeCwd(dir))
	if err := os.MkdirAll(folder, 0o755); err != nil {
		return "", err
	}
	var b strings.Builder
	parent := ""
	now := time.Now().UTC()
	writeRec := func(role, text string) {
		uuid := sessions.NewSessionID()
		rec := map[string]any{
			"type":        role,
			"uuid":        uuid,
			"timestamp":   now.Format("2006-01-02T15:04:05.000Z"),
			"sessionId":   id,
			"cwd":         dir,
			"userType":    "external",
			"isSidechain": false,
			"message": map[string]any{
				"role":    role,
				"content": []map[string]any{{"type": "text", "text": text}},
			},
		}
		if parent == "" {
			rec["parentUuid"] = nil
		} else {
			rec["parentUuid"] = parent
		}
		parent = uuid
		line, _ := json.Marshal(rec)
		b.Write(line)
		b.WriteByte('\n')
		now = now.Add(time.Second)
	}
	writeRec("user", t.HandoffNote(artifactPath))
	for _, turn := range t.targetTurns() {
		switch turn.Role {
		case "user":
			writeRec("user", turn.Text)
		case "assistant":
			writeRec("assistant", turn.Text)
		case "tool":
			writeRec("assistant", fmt.Sprintf("[ran %s: %s]", turn.Name, turn.Text))
		}
	}
	path := filepath.Join(folder, id+".jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return "", err
	}
	if err := sessions.PointLastSession(claudeHome, dir, id); err != nil {
		return "", fmt.Errorf("session written but not registered in .claude.json: %w", err)
	}
	return id, nil
}

// WriteCodex emits the IR as a codex rollout for dir and registers it in the
// session index, so `codex resume --last` there (cwd-filtered, newest first)
// resumes it natively. Verified against codex 0.139.0: a rollout of
// session_meta plus message response_items loads and the model answers from
// the fabricated turns.
func WriteCodex(t *Transcript, codexHome, dir, artifactPath string) (string, error) {
	id := sessions.NewSessionID()
	now := time.Now().UTC()
	stamp := now.Format("2006-01-02T15:04:05.000Z")
	folder := filepath.Join(codexHome, "sessions", now.Format("2006"), now.Format("01"), now.Format("02"))
	if err := os.MkdirAll(folder, 0o755); err != nil {
		return "", err
	}
	var b strings.Builder
	writeItem := func(payload map[string]any) {
		line, _ := json.Marshal(map[string]any{"timestamp": stamp, "type": "response_item", "payload": payload})
		b.Write(line)
		b.WriteByte('\n')
	}
	writeEvent := func(kind, msg string) {
		line, _ := json.Marshal(map[string]any{"timestamp": stamp, "type": "event_msg", "payload": map[string]any{"type": kind, "message": msg}})
		b.Write(line)
		b.WriteByte('\n')
	}
	meta, _ := json.Marshal(map[string]any{"timestamp": stamp, "type": "session_meta", "payload": map[string]any{
		"id": id, "timestamp": stamp, "cwd": dir,
		"originator": "codex-tui", "cli_version": "0.139.0", "source": "cli", "thread_source": "user",
	}})
	b.Write(meta)
	b.WriteByte('\n')
	message := func(role, kind, text string) map[string]any {
		return map[string]any{"type": "message", "role": role, "content": []map[string]any{{"type": kind, "text": text}}}
	}
	handoffText := t.HandoffNote(artifactPath)
	writeItem(message("user", "input_text", handoffText))
	writeEvent("user_message", handoffText)
	for _, turn := range t.targetTurns() {
		switch turn.Role {
		case "user":
			writeItem(message("user", "input_text", turn.Text))
			writeEvent("user_message", turn.Text)
		case "assistant":
			writeItem(message("assistant", "output_text", turn.Text))
			writeEvent("agent_message", turn.Text)
		case "tool":
			text := fmt.Sprintf("[ran %s: %s]", turn.Name, turn.Text)
			writeItem(message("assistant", "output_text", text))
			writeEvent("agent_message", text)
		}
	}
	path := filepath.Join(folder, fmt.Sprintf("rollout-%s-%s.jsonl", now.Format("2006-01-02T15-04-05"), id))
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return "", err
	}
	idx, _ := json.Marshal(map[string]any{"id": id, "thread_name": "proj switch handoff", "updated_at": now.Format(time.RFC3339Nano)})
	f, err := os.OpenFile(filepath.Join(codexHome, "session_index.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return "", fmt.Errorf("rollout written but index not updated: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(idx, '\n')); err != nil {
		return "", fmt.Errorf("rollout written but index not updated: %w", err)
	}
	return id, nil
}
