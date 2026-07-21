package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tieo/proj/internal/config"
	"github.com/tieo/proj/internal/daemon"
	"github.com/tieo/proj/internal/projects"
	"github.com/tieo/proj/internal/sessions"
)

// recordingOps stands in for tmux during a rename and snapshots the state of
// the filesystem at each step, so a test can assert the ORDER of the rename:
// park the tool before the directory moves, migrate the history before the
// tool comes back.
type recordingOps struct {
	live string // session name reported for the project's directory

	calls []string

	oldDirAtPark   bool   // the project directory still existed when the pane was parked
	historyAtStart string // where the transcript was when the tool was relaunched
	startedIn      string // directory the tool was relaunched in
	startedCmd     string // command the tool was relaunched with
	renamedTo      string

	oldFolder string // history folder of the pre-rename path
	newFolder string // history folder of the post-rename path
}

func (o *recordingOps) SessionForPath(string) string {
	o.calls = append(o.calls, "SessionForPath")
	return o.live
}

func (o *recordingOps) RenameSession(_, newName string) error {
	o.calls = append(o.calls, "RenameSession")
	o.renamedTo = newName
	return nil
}

func (o *recordingOps) RespawnShell(_, dir string) error {
	o.calls = append(o.calls, "RespawnShell")
	_, err := os.Stat(dir)
	o.oldDirAtPark = err == nil
	return nil
}

func (o *recordingOps) RespawnSession(_, dir, command string) error {
	o.calls = append(o.calls, "RespawnSession")
	o.startedIn = dir
	o.startedCmd = command
	switch {
	case hasTranscript(o.newFolder):
		o.historyAtStart = "new"
	case hasTranscript(o.oldFolder):
		o.historyAtStart = "old"
	default:
		o.historyAtStart = "none"
	}
	return nil
}

func hasTranscript(folder string) bool {
	m, _ := filepath.Glob(filepath.Join(folder, "*.jsonl"))
	return len(m) > 0
}

// renameFixture builds a project with one recorded conversation and the env
// (registry, daemon state, Claude home) pointed at throwaway directories.
func renameFixture(t *testing.T) (cfg config.Config, p projects.Project, oldFolder, newFolder string) {
	t.Helper()
	// WSLToUNC only rewrites paths when a distro name is set; clearing it keeps
	// the history folders named after the plain temp paths on every host.
	t.Setenv("WSL_DISTRO_NAME", "")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))

	base := t.TempDir()
	claudeHome := t.TempDir()
	oldDir := filepath.Join(base, "old")
	if err := os.MkdirAll(oldDir, 0o755); err != nil {
		t.Fatal(err)
	}

	oldFolder = filepath.Join(claudeHome, "projects", sessions.EncodeCwd(oldDir))
	newFolder = filepath.Join(claudeHome, "projects", sessions.EncodeCwd(filepath.Join(base, "new")))
	if err := os.MkdirAll(oldFolder, 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"user","cwd":"` + oldDir + `","sessionId":"s1","message":{"role":"user","content":"the conversation"}}` + "\n"
	if err := os.WriteFile(filepath.Join(oldFolder, "s1.jsonl"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg = config.Default()
	cfg.BaseDir = base
	cfg.Claude.Home = claudeHome
	p = projects.Project{Name: "old", Dir: oldDir}
	return cfg, p, oldFolder, newFolder
}

// A rename must not cost the running conversation: the session is kept (not
// killed), its history moves with the project, and the tool comes back on the
// resume command so it continues the same conversation in the new directory.
func TestRenameProjectCarriesLiveConversation(t *testing.T) {
	cfg, p, oldFolder, newFolder := renameFixture(t)
	newDir := filepath.Join(cfg.BaseDir, "new")

	// A pinned entry for the live session: the daemon's bookkeeping has to
	// follow the rename, or the pin is dropped with the vanished directory.
	if err := daemon.UpdateManagedState(daemonConfig().StatePath, func(m daemon.ManagedState) error {
		m["old"] = daemon.ManagedSession{Name: "old", Dir: p.Dir, Pinned: true}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	ops := &recordingOps{live: "old", oldFolder: oldFolder, newFolder: newFolder}
	if err := renameProject(cfg, p, "new", ops); err != nil {
		t.Fatalf("renameProject: %v", err)
	}

	want := []string{"SessionForPath", "RespawnShell", "RenameSession", "RespawnSession"}
	if strings.Join(ops.calls, ",") != strings.Join(want, ",") {
		t.Errorf("call order = %v, want %v", ops.calls, want)
	}
	if !ops.oldDirAtPark {
		t.Error("the pane was parked after the directory moved; the tool keeps writing to the old history until then")
	}
	if ops.historyAtStart != "new" {
		t.Errorf("history was %q when the tool relaunched, want it already migrated (%q)", ops.historyAtStart, "new")
	}
	if ops.startedIn != newDir {
		t.Errorf("tool relaunched in %q, want %q", ops.startedIn, newDir)
	}
	if !strings.Contains(ops.startedCmd, cfg.Claude.ResumeFlag) {
		t.Errorf("relaunch command %q does not resume; the conversation would start over", ops.startedCmd)
	}
	if ops.renamedTo != "new" {
		t.Errorf("session renamed to %q, want %q", ops.renamedTo, "new")
	}

	if hasTranscript(oldFolder) {
		t.Error("a transcript stayed behind under the old path")
	}
	if !hasTranscript(newFolder) {
		t.Error("the conversation did not move to the new path")
	}
	if _, err := os.Stat(newDir); err != nil {
		t.Errorf("project directory not moved: %v", err)
	}

	managed, err := daemon.LoadManagedState(daemonConfig().StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, stale := managed["old"]; stale {
		t.Error("the daemon still tracks the old session name, whose directory is gone")
	}
	if ms := managed["new"]; !ms.Pinned || ms.Dir != newDir {
		t.Errorf("managed entry after rename = %+v, want pinned at %q", ms, newDir)
	}
}

// With nothing running there is no conversation to carry, but the history
// still moves and a session left under the old name is renamed.
func TestRenameProjectWithoutLiveSession(t *testing.T) {
	cfg, p, oldFolder, newFolder := renameFixture(t)

	ops := &recordingOps{live: "", oldFolder: oldFolder, newFolder: newFolder}
	if err := renameProject(cfg, p, "new", ops); err != nil {
		t.Fatalf("renameProject: %v", err)
	}

	want := []string{"SessionForPath", "RenameSession"}
	if strings.Join(ops.calls, ",") != strings.Join(want, ",") {
		t.Errorf("call order = %v, want %v", ops.calls, want)
	}
	if !hasTranscript(newFolder) || hasTranscript(oldFolder) {
		t.Error("the conversation did not move to the new path")
	}
}
