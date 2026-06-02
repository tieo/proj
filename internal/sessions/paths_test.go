package sessions

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLocalDir(t *testing.T) {
	cases := []struct{ in, want string }{
		{`C:\Users\u\AppData\Local\Temp\cc-1`, "/mnt/c/Users/u/AppData/Local/Temp/cc-1"},
		{`D:\proj`, "/mnt/d/proj"},
		{`\\wsl.localhost\Ubuntu-24.04\home\u\p`, "/home/u/p"},
		{"/home/u/p", "/home/u/p"},
	}
	for _, c := range cases {
		if got := LocalDir(c.in); got != c.want {
			t.Errorf("LocalDir(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// On a Linux-only machine (no WSL) the path helpers must leave native paths
// untouched: no UNC rewriting, so a project's cwd is just its directory.
func TestPathsOnLinuxOnly(t *testing.T) {
	t.Setenv("WSL_DISTRO_NAME", "")
	if got := WSLToUNC("/home/u/projects/x"); got != "/home/u/projects/x" {
		t.Errorf("off WSL, WSLToUNC should pass through, got %q", got)
	}
	if got := CwdForDir("/home/u/projects/x", nil); got != "/home/u/projects/x" {
		t.Errorf("off WSL, CwdForDir should be the dir itself, got %q", got)
	}
	if got := LocalDir("/home/u/projects/x"); got != "/home/u/projects/x" {
		t.Errorf("LocalDir should pass a native path through, got %q", got)
	}
}

// On a Linux box that has used Claude, ~/.claude/projects exists, so Home picks
// the native home and never probes for a Windows install.
func TestHomePrefersNative(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	native := filepath.Join(home, ".claude")
	if err := os.MkdirAll(filepath.Join(native, "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := Home(""); got != native {
		t.Errorf("Home() = %q, want %q", got, native)
	}
}
