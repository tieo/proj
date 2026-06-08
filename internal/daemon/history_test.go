package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEncodeClaudePath(t *testing.T) {
	cases := map[string]string{
		"/home/u/x":                    "-home-u-x",
		"/home/lwebom11/projects/code": "-home-lwebom11-projects-code",
		"Ubuntu-24.04":                 "Ubuntu-24-04",
		`\\wsl.localhost\Ubuntu-24.04`: "--wsl-localhost-Ubuntu-24-04",
	}
	for in, want := range cases {
		if got := encodeClaudePath(in); got != want {
			t.Errorf("encodeClaudePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLocateProjectDir(t *testing.T) {
	root := t.TempDir()
	projects := filepath.Join(root, "projects")
	const work = "/home/u/projects/code/virtmc"

	// Bare-Linux layout: directly-encoded name.
	linuxName := encodeClaudePath(work) // -home-u-projects-code-virtmc
	mustMkdir(t, filepath.Join(projects, linuxName))
	// WSL layout: claude.exe keys on the \\wsl.localhost\<distro>\... UNC path,
	// whose encoded name ends with the encoding of work.
	wslName := "--wsl-localhost-Ubuntu-24-04" + linuxName
	mustMkdir(t, filepath.Join(projects, wslName))
	// A decoy that must not match either probe.
	mustMkdir(t, filepath.Join(projects, "-home-u-projects-code-other"))

	if got := locateProjectDir(root, work, false); got != filepath.Join(projects, linuxName) {
		t.Errorf("linux locate = %q, want .../%s", got, linuxName)
	}
	if got := locateProjectDir(root, work, true); got != filepath.Join(projects, wslName) {
		t.Errorf("wsl locate = %q, want .../%s", got, wslName)
	}
	if got := locateProjectDir(root, "/home/u/projects/code/missing", false); got != "" {
		t.Errorf("missing linux locate = %q, want \"\"", got)
	}
	if got := locateProjectDir(root, "/home/u/projects/code/missing", true); got != "" {
		t.Errorf("missing wsl locate = %q, want \"\"", got)
	}
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}
