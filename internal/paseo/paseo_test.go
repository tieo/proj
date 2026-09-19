package paseo

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeCLI installs a stub `paseo` that prints agents, so the lookup can be
// tested without a daemon.
func fakeCLI(t *testing.T, stdout string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "paseo-stub")
	script := "#!/bin/sh\ncat <<'JSON'\n" + stdout + "\nJSON\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
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
	fakeCLI(t, `[{"id":"a1","shortId":"a1","name":"proj","status":"idle","cwd":"~/projects/code/proj"}]`)

	got := AgentFor(filepath.Join(home, "projects", "code", "proj"))
	if got == nil {
		t.Fatal("expected the agent whose cwd is the same directory written with a tilde")
	}
	if got.ID != "a1" {
		t.Fatalf("id = %q, want a1", got.ID)
	}
}

func TestAgentForIgnoresOtherDirectories(t *testing.T) {
	fakeCLI(t, `[{"id":"a1","shortId":"a1","name":"other","status":"idle","cwd":"/tmp/somewhere-else"}]`)

	if got := AgentFor("/tmp/not-that-one"); got != nil {
		t.Fatalf("got %+v, want no match", got)
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

func TestRunningReadsStatus(t *testing.T) {
	if !(Agent{Status: "running"}).Running() {
		t.Error("a running agent should report Running")
	}
	if (Agent{Status: "idle"}).Running() {
		t.Error("an idle agent should not report Running")
	}
}
