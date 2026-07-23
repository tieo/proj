package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

func writeSessionFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBridgeSessionID(t *testing.T) {
	t.Setenv("WSL_DISTRO_NAME", "")
	home := t.TempDir()
	sessDir := filepath.Join(home, "sessions")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// An older run of the same project, a newer one, another project, and a
	// session whose Remote Control never bound.
	writeSessionFile(t, sessDir, "100.json", `{"cwd":"/p/one","bridgeSessionId":"session_old","updatedAt":100}`)
	writeSessionFile(t, sessDir, "200.json", `{"cwd":"/p/one","bridgeSessionId":"session_new","updatedAt":200}`)
	writeSessionFile(t, sessDir, "300.json", `{"cwd":"/p/two","bridgeSessionId":"session_other","updatedAt":300}`)
	writeSessionFile(t, sessDir, "400.json", `{"cwd":"/p/one","updatedAt":400}`)

	if got := BridgeSessionID(home, "/p/one"); got != "session_new" {
		t.Errorf("BridgeSessionID = %q, want the newest bound session", got)
	}
	if got := BridgeSessionID(home, "/p/three"); got != "" {
		t.Errorf("BridgeSessionID for an unknown project = %q, want empty", got)
	}
}

// Under WSL Claude Code records the cwd as a \\wsl.localhost UNC path, so a
// lookup by the Linux path has to match that spelling too.
func TestBridgeSessionIDMatchesTheUNCSpelling(t *testing.T) {
	t.Setenv("WSL_DISTRO_NAME", "Ubuntu-24.04")
	home := t.TempDir()
	sessDir := filepath.Join(home, "sessions")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSessionFile(t, sessDir, "1.json",
		`{"cwd":"\\\\wsl.localhost\\Ubuntu-24.04\\home\\u\\projects\\code\\29","bridgeSessionId":"session_29","updatedAt":1}`)

	if got := BridgeSessionID(home, "/home/u/projects/code/29"); got != "session_29" {
		t.Errorf("BridgeSessionID = %q, want session_29", got)
	}
}

// A renamed or re-tagged project keeps the RC title it launched with, so the
// bridge must be found by directory: the title lookup that used to back the
// drop check would miss and report a live bridge as dropped.
func TestRCBridgeForDirIgnoresTheTitle(t *testing.T) {
	t.Setenv("WSL_DISTRO_NAME", "")
	home := t.TempDir()
	sessDir := filepath.Join(home, "sessions")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Launched as "simulator @host"; the project has since been tagged, so proj
	// would now derive "simulator @host [doner]" and a title lookup would miss.
	writeSessionFile(t, sessDir, "1.json",
		`{"cwd":"/p/simulator","name":"simulator @host","bridgeSessionId":"session_live","updatedAt":5}`)

	bound, known := RCBridgeForDir(home, "/p/simulator")
	if !known || !bound {
		t.Errorf("RCBridgeForDir = (bound=%v, known=%v), want a live bridge", bound, known)
	}
}

func TestRCBridgeForDirReportsADrop(t *testing.T) {
	t.Setenv("WSL_DISTRO_NAME", "")
	home := t.TempDir()
	sessDir := filepath.Join(home, "sessions")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSessionFile(t, sessDir, "1.json",
		`{"cwd":"/p/x","name":"x @host","bridgeSessionId":null,"updatedAt":5}`)

	if bound, known := RCBridgeForDir(home, "/p/x"); !known || bound {
		t.Errorf("a null bridge should read as a known drop, got (bound=%v, known=%v)", bound, known)
	}
	// Nothing recorded for the directory: unknown, so the watchdog holds off.
	if bound, known := RCBridgeForDir(home, "/p/other"); known || bound {
		t.Errorf("an unknown directory should report unknown, got (bound=%v, known=%v)", bound, known)
	}
}
