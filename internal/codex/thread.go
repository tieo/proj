// Package codex creates native Codex threads through its app-server protocol.
package codex

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"

	"github.com/tieo/proj/internal/handoff"
)

// ImportClaudeSession uses Codex's native Claude Code importer. Unlike raw
// item injection, imported sessions are rendered by the Codex terminal UI.
func ImportClaudeSession(path, dir string) (string, error) {
	cmd := exec.Command("codex", "app-server", "--stdio")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	defer func() { _ = stdin.Close(); _ = cmd.Wait() }()
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	send := func(value any) error {
		b, err := json.Marshal(value)
		if err != nil {
			return err
		}
		_, err = stdin.Write(append(b, '\n'))
		return err
	}
	if err := send(request{Method: "initialize", ID: 1, Params: map[string]any{"clientInfo": map[string]string{"name": "proj", "title": "proj", "version": "0.1.0"}, "capabilities": map[string]bool{"experimentalApi": true}}}); err != nil {
		return "", err
	}
	if err := send(request{Method: "initialized", Params: map[string]any{}}); err != nil {
		return "", err
	}
	item := map[string]any{"itemType": "SESSIONS", "description": "Import Claude Code session", "cwd": nil, "details": map[string]any{"sessions": []map[string]any{{"path": path, "cwd": dir}}}}
	if err := send(request{Method: "externalAgentConfig/import", ID: 2, Params: map[string]any{"migrationSource": "claude-code", "source": "proj", "migrationItems": []any{item}}}); err != nil {
		return "", err
	}
	for scanner.Scan() {
		var event struct {
			Method string `json:"method"`
			Params struct {
				ItemTypeResults []struct {
					Successes []struct {
						Target string `json:"target"`
					} `json:"successes"`
				} `json:"itemTypeResults"`
			} `json:"params"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			continue
		}
		if event.Method == "externalAgentConfig/import/completed" {
			for _, result := range event.Params.ItemTypeResults {
				if len(result.Successes) > 0 && result.Successes[0].Target != "" {
					return result.Successes[0].Target, nil
				}
			}
			return "", fmt.Errorf("Claude session import completed without a thread")
		}
		if event.Error != nil {
			return "", fmt.Errorf("Claude session import: %s", event.Error.Message)
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", io.EOF
}

type request struct {
	Method string `json:"method"`
	ID     int    `json:"id,omitempty"`
	Params any    `json:"params"`
}

type response struct {
	ID     int             `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// WriteThread creates a native Codex thread and appends the translated turns
// through the app-server's thread/inject_items API.
func WriteThread(transcript *handoff.Transcript, dir, artifactPath string) (string, error) {
	cmd := exec.Command("codex", "app-server", "--stdio")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	defer func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	}()

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	call := func(id int, method string, params any) (json.RawMessage, error) {
		line, err := json.Marshal(request{Method: method, ID: id, Params: params})
		if err != nil {
			return nil, err
		}
		if _, err := stdin.Write(append(line, '\n')); err != nil {
			return nil, err
		}
		for scanner.Scan() {
			var reply response
			if err := json.Unmarshal(scanner.Bytes(), &reply); err != nil || reply.ID != id {
				continue
			}
			if reply.Error != nil {
				return nil, fmt.Errorf("%s: %s", method, reply.Error.Message)
			}
			return reply.Result, nil
		}
		if err := scanner.Err(); err != nil {
			return nil, err
		}
		return nil, io.EOF
	}

	if _, err := call(1, "initialize", map[string]any{
		"clientInfo":   map[string]string{"name": "proj", "title": "proj", "version": "0.1.0"},
		"capabilities": map[string]bool{"experimentalApi": true},
	}); err != nil {
		return "", err
	}
	initialized, _ := json.Marshal(request{Method: "initialized", Params: map[string]any{}})
	if _, err := stdin.Write(append(initialized, '\n')); err != nil {
		return "", err
	}
	started, err := call(2, "thread/start", map[string]any{
		"cwd": dir, "approvalPolicy": "never", "sandbox": "danger-full-access", "threadSource": "cli",
	})
	if err != nil {
		return "", err
	}
	var thread struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(started, &thread); err != nil || thread.Thread.ID == "" {
		return "", fmt.Errorf("thread/start returned no thread id: %w", err)
	}
	if _, err := call(3, "thread/inject_items", map[string]any{"threadId": thread.Thread.ID, "items": items(transcript, artifactPath)}); err != nil {
		return "", err
	}
	return thread.Thread.ID, nil
}

func items(transcript *handoff.Transcript, artifactPath string) []map[string]any {
	message := func(role, contentType, text string) map[string]any {
		item := map[string]any{"type": "message", "role": role, "content": []map[string]string{{"type": contentType, "text": text}}}
		if role == "assistant" {
			item["phase"] = "final_answer"
		}
		return item
	}
	result := []map[string]any{message("user", "input_text", transcript.HandoffNote(artifactPath))}
	for _, turn := range transcript.TargetTurns() {
		switch turn.Role {
		case "user":
			result = append(result, message("user", "input_text", turn.Text))
		case "assistant":
			result = append(result, message("assistant", "output_text", turn.Text))
		case "tool":
			result = append(result, message("assistant", "output_text", fmt.Sprintf("[ran %s: %s]", turn.Name, turn.Text)))
		}
	}
	return result
}
