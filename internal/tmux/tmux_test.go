package tmux

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestNewSessionArgs(t *testing.T) {
	tests := []struct {
		name        string
		command     string
		capturePane bool
		want        []string
	}{
		{
			name:        "plain shell, capture pane",
			command:     "",
			capturePane: true,
			want:        []string{"new-session", "-d", "-P", "-F", "#{pane_id}", "-s", "proj@x", "-c", "/home/u/x"},
		},
		{
			name:        "with command, capture pane",
			command:     "claude -c; exec bash",
			capturePane: true,
			want:        []string{"new-session", "-d", "-P", "-F", "#{pane_id}", "-s", "proj@x", "-c", "/home/u/x", "claude -c; exec bash"},
		},
		{
			name:        "with command, no capture (systemd path)",
			command:     "claude -c; exec bash",
			capturePane: false,
			want:        []string{"new-session", "-d", "-s", "proj@x", "-c", "/home/u/x", "claude -c; exec bash"},
		},
		{
			name:        "plain shell, no capture",
			command:     "",
			capturePane: false,
			want:        []string{"new-session", "-d", "-s", "proj@x", "-c", "/home/u/x"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := newSessionArgs("proj@x", "/home/u/x", tt.command, tt.capturePane)
			if !slices.Equal(got, tt.want) {
				t.Errorf("newSessionArgs() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSystemdRunArgs(t *testing.T) {
	// The wrapper must place its own flags, then a --setenv per forwarded env
	// var, then the literal "--" separator before "tmux", so the session args
	// that follow are handed to tmux, not parsed by systemd-run. Type=forking is
	// required for systemd to track the daemonized server as the unit's main
	// process; forwarding the environment keeps the pane's PATH intact.
	sessionArgs := []string{"new-session", "-d", "-s", "proj@x", "-c", "/home/u/x"}
	env := []string{"PATH=/home/u/.local/bin:/usr/bin", "HOME=/home/u"}
	got := systemdRunArgs(sessionArgs, env)
	want := []string{
		"--user", "--quiet", "--collect", "-p", "Type=forking",
		"--setenv=PATH=/home/u/.local/bin:/usr/bin", "--setenv=HOME=/home/u",
		"--", "tmux",
		"new-session", "-d", "-s", "proj@x", "-c", "/home/u/x",
	}
	if !slices.Equal(got, want) {
		t.Errorf("systemdRunArgs() = %v, want %v", got, want)
	}
}

func TestSystemdRunArgsNoEnv(t *testing.T) {
	// With no env to forward, the separator must still sit directly before tmux.
	got := systemdRunArgs([]string{"new-session", "-d"}, nil)
	want := []string{"--user", "--quiet", "--collect", "-p", "Type=forking", "--", "tmux", "new-session", "-d"}
	if !slices.Equal(got, want) {
		t.Errorf("systemdRunArgs() = %v, want %v", got, want)
	}
}

func TestDetectWSL(t *testing.T) {
	tests := []struct {
		name      string
		osRelease string
		want      bool
	}{
		{"WSL2", "5.15.153.1-microsoft-standard-WSL2", true},
		{"WSL1 capitalized", "4.4.0-19041-Microsoft", true},
		{"native linux", "6.8.0-40-generic", false},
		{"empty (read failed)", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := detectWSL(tt.osRelease); got != tt.want {
				t.Errorf("detectWSL(%q) = %v, want %v", tt.osRelease, got, tt.want)
			}
		})
	}
}

func TestFirstLine(t *testing.T) {
	cases := map[string]string{
		"%33\n%34\n": "%33", // list-panes on a multi-pane session
		"  %7  \n":   "%7",
		"%1":         "%1",
		"\n":         "",
		"":           "",
	}
	for in, want := range cases {
		if got := firstLine(in); got != want {
			t.Errorf("firstLine(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPaneProgram(t *testing.T) {
	cases := map[string]string{
		"claude --dangerously-skip-permissions -n rc": "claude",
		"codex resume --last":                         "codex",
		"":                                            "",
		"/usr/local/bin/claude -c":                    "", // a path resolves without PATH
		"FOO=bar claude":                              "", // env prefix is the shell's job
		"$TOOL --flag":                                "",
	}
	for in, want := range cases {
		if got := paneProgram(in); got != want {
			t.Errorf("paneProgram(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWithUserBinPath(t *testing.T) {
	home := t.TempDir()
	local := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(local, 0o755); err != nil {
		t.Fatal(err)
	}
	// ~/bin is deliberately absent: only dirs that exist are added.
	got := withUserBinPath([]string{"HOME=" + home, "PATH=/usr/bin:/bin"}, home)
	want := []string{"HOME=" + home, "PATH=" + local + ":/usr/bin:/bin"}
	if !slices.Equal(got, want) {
		t.Errorf("withUserBinPath = %v, want %v", got, want)
	}

	// Already listed: PATH is left untouched, no duplicate entry.
	in := []string{"PATH=" + local + ":/usr/bin"}
	if got := withUserBinPath(in, home); !slices.Equal(got, in) {
		t.Errorf("withUserBinPath (already present) = %v, want %v", got, in)
	}
}

func TestLookPathIn(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "claude")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !lookPathIn("claude", "/nonexistent:"+dir) {
		t.Error("lookPathIn missed an executable in a later PATH entry")
	}
	if lookPathIn("notes", dir) {
		t.Error("lookPathIn accepted a non-executable file")
	}
	if lookPathIn("claude", "") {
		t.Error("lookPathIn accepted an empty PATH")
	}
}

func TestChunkRunes(t *testing.T) {
	if got := chunkRunes("short", 500); !slices.Equal(got, []string{"short"}) {
		t.Errorf("chunkRunes short = %v, want one piece", got)
	}
	if got := chunkRunes("", 500); !slices.Equal(got, []string{""}) {
		t.Errorf("chunkRunes empty = %v, want one empty piece", got)
	}
	// Multi-byte runes must not be cut in half: a piece that ends mid-character
	// reaches the pane as a replacement character.
	got := chunkRunes("äöüßäöüß", 3)
	want := []string{"äöü", "ßäö", "üß"}
	if !slices.Equal(got, want) {
		t.Errorf("chunkRunes = %q, want %q", got, want)
	}
	var n int
	for _, p := range chunkRunes(strings.Repeat("x", 2000), literalChunk) {
		if len([]rune(p)) > literalChunk {
			t.Errorf("piece of %d runes exceeds the burst limit %d", len([]rune(p)), literalChunk)
		}
		n += len(p)
	}
	if n != 2000 {
		t.Errorf("pieces total %d runes, want 2000", n)
	}
}
