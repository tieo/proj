package tmux

import (
	"slices"
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
