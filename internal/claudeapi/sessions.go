// Package claudeapi is a small client for the Claude Code cloud session API
// (the Remote Control sessions listed at claude.ai/code). It reads the oauth
// token Claude Code stores locally and is used only by explicit user commands,
// never the daemon's hot path.
package claudeapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const sessionsURL = "https://api.anthropic.com/v1/sessions"

// Session is one Remote Control session as the API reports it.
type Session struct {
	ID               string `json:"id"`
	Title            string `json:"title"`
	ConnectionStatus string `json:"connection_status"`
	SessionStatus    string `json:"session_status"`
}

// Token reads the ChatGPT-style oauth access token Claude Code writes to
// <claudeRoot>/.credentials.json. claudeRoot is the resolved .claude directory
// (daemon.ClaudeRoot handles the WSL-relocated home).
func Token(claudeRoot string) (string, error) {
	path := filepath.Join(claudeRoot, ".credentials.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read creds %s: %w", path, err)
	}
	var creds struct {
		ClaudeAiOauth struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(raw, &creds); err != nil {
		return "", fmt.Errorf("parse creds: %w", err)
	}
	if creds.ClaudeAiOauth.AccessToken == "" {
		return "", fmt.Errorf("no oauth access token in %s (logged in with an API key rather than a Claude account?)", path)
	}
	return creds.ClaudeAiOauth.AccessToken, nil
}

func request(method, url, token string) (*http.Request, error) {
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", "ccr-byoc-2025-07-29")
	return req, nil
}

// ListSessions returns the account's Remote Control sessions. The endpoint
// caps a page at 100; TooMany reports whether the account has more than one
// page (the caller can warn that a sweep saw only the first).
func ListSessions(token string) (sessions []Session, tooMany bool, err error) {
	req, err := request("GET", sessionsURL+"?limit=100", token)
	if err != nil {
		return nil, false, err
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("GET /v1/sessions: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("GET /v1/sessions: HTTP %d", resp.StatusCode)
	}
	var out struct {
		Data []Session `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, false, fmt.Errorf("decode /v1/sessions: %w", err)
	}
	return out.Data, len(out.Data) >= 100, nil
}

// DeleteSession removes one Remote Control session from the account (the cloud
// side only; Claude Code's local transcript under ~/.claude is untouched).
func DeleteSession(token, id string) error {
	req, err := request("DELETE", sessionsURL+"/"+id, token)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("DELETE %s: %w", id, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("DELETE %s: HTTP %d", id, resp.StatusCode)
	}
	return nil
}

// RenameSession re-titles one Remote Control session. The title is what
// claude.ai/code and the phone app show, and it is fixed when the bridge first
// registers: a session resumed under a new --remote-control name keeps the old
// title, so renaming a project has to say so here too.
func RenameSession(token, id, title string) error {
	body, err := json.Marshal(map[string]string{"title": title})
	if err != nil {
		return err
	}
	req, err := request("PATCH", sessionsURL+"/"+id, token)
	if err != nil {
		return err
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.Header.Set("content-type", "application/json")
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("PATCH %s: %w", id, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("PATCH %s: HTTP %d", id, resp.StatusCode)
	}
	return nil
}
