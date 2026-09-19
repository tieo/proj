package paseo

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func fakeCLI(t *testing.T, stdout string) {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "paseo-stub")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\ncat <<'JSON'\n"+stdout+"\nJSON\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := Bin
	Bin = bin
	t.Cleanup(func() { Bin = old })
}

func TestAgentForMatchesTildePath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	fakeCLI(t, `[{"id":"a1","name":"proj","status":"idle","cwd":"~/projects/code/proj"}]`)
	got := AgentFor(filepath.Join(home, "projects", "code", "proj"))
	if got == nil || got.ID != "a1" {
		t.Fatalf("got %+v, want the agent whose cwd is that directory written with a tilde", got)
	}
}

func TestAgentsOnBrokenCLI(t *testing.T) {
	old := Bin
	Bin = filepath.Join(t.TempDir(), "absent")
	t.Cleanup(func() { Bin = old })
	if got := Agents(); got != nil {
		t.Fatalf("got %v, want nil when the CLI cannot run", got)
	}
}

func TestIdle(t *testing.T) {
	if !(Agent{Status: "idle"}).Idle() {
		t.Error("an idle agent is the only kind that can be stuck")
	}
	for _, s := range []string{"running", "error", "sent"} {
		if (Agent{Status: s}).Idle() {
			t.Errorf("%q is not idle", s)
		}
	}
}

func TestSessionIDReadsTheAgentRecord(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "agents", "home-marius-projects-code-phonetix")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	rec := map[string]any{"runtimeInfo": map[string]string{"sessionId": "sess-1"}}
	data, _ := json.Marshal(rec)
	if err := os.WriteFile(filepath.Join(dir, "agent-1.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := SessionID(home, "agent-1", "/home/marius/projects/code/phonetix"); got != "sess-1" {
		t.Errorf("got %q, want sess-1", got)
	}
}

func TestSessionIDFallsBackToPersistence(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "agents", "tmp-x")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]any{"persistence": map[string]string{"sessionId": "sess-2"}})
	if err := os.WriteFile(filepath.Join(dir, "a.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := SessionID(home, "a", "/tmp/x"); got != "sess-2" {
		t.Errorf("got %q, want sess-2", got)
	}
}

func TestSessionIDMissingRecord(t *testing.T) {
	if got := SessionID(t.TempDir(), "nope", "/tmp/x"); got != "" {
		t.Errorf("got %q, want empty for an agent with no record", got)
	}
}
