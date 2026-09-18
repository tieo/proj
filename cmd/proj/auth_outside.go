package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Claude Code holds the login it started with in memory and writes it back when
// it stops. Switching the login therefore has to stop every running Claude
// first, which claudeSessions does for the tmux panes proj started. A Claude
// running anywhere else is invisible to that: a daemon that supervises its own
// agents, such as paseo, spawns them as ordinary children rather than in a
// pane, and one of those still holding the old login will write it back over
// the new one after the switch.
//
// These cannot be stopped from here without guessing at another supervisor's
// lifecycle, so they are reported instead and the caller decides.

// procRoot is a variable so tests can point it at a fixture tree.
var procRoot = "/proc"

// claudeOutsideTmux returns a line per running Claude Code process that no tmux
// server is an ancestor of, ready to print. The command is matched by its
// executable name so a wrapper script around it does not count twice.
func claudeOutsideTmux(command string) []string {
	binary := filepath.Base(strings.Fields(command)[0])
	if binary == "" {
		return nil
	}

	parents := map[int]int{}
	names := map[int]string{}
	commands := map[int]string{}
	for _, pid := range processIDs() {
		name, parent, ok := processStat(pid)
		if !ok {
			continue
		}
		parents[pid] = parent
		names[pid] = name
		commands[pid] = processCommand(pid)
	}

	var out []string
	for pid, name := range names {
		if name != binary && filepath.Base(firstField(commands[pid])) != binary {
			continue
		}
		if underTmux(pid, parents, names) {
			continue
		}
		out = append(out, describeProcess(pid, commands[pid]))
	}
	return out
}

// underTmux walks the parent chain looking for a tmux server. The walk is
// bounded by the number of processes seen, so a cycle in a malformed /proc
// cannot hang it.
func underTmux(pid int, parents map[int]int, names map[int]string) bool {
	for steps := 0; steps < len(parents)+1; steps++ {
		parent, ok := parents[pid]
		if !ok || parent <= 1 {
			return false
		}
		if strings.HasPrefix(names[parent], "tmux") {
			return true
		}
		pid = parent
	}
	return false
}

func processIDs() []int {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil
	}
	var pids []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}

// processStat reads the name and parent of a process. The name in stat is
// wrapped in parentheses and may itself contain them, so the split is on the
// last ")" rather than the first.
func processStat(pid int) (name string, parent int, ok bool) {
	raw, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "stat"))
	if err != nil {
		return "", 0, false
	}
	line := string(raw)
	open := strings.IndexByte(line, '(')
	close := strings.LastIndexByte(line, ')')
	if open < 0 || close < open {
		return "", 0, false
	}
	name = line[open+1 : close]
	fields := strings.Fields(line[close+1:])
	if len(fields) < 2 {
		return "", 0, false
	}
	parent, err = strconv.Atoi(fields[1])
	if err != nil {
		return "", 0, false
	}
	return name, parent, true
}

func processCommand(pid int) string {
	raw, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.ReplaceAll(string(raw), "\x00", " "))
}

func firstField(command string) string {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// describeProcess names a process by its working directory where that can be
// read, since that is what identifies which agent it is; the command line is
// the fallback and is cut short because an agent's can run to hundreds of
// characters.
func describeProcess(pid int, command string) string {
	if dir, err := os.Readlink(filepath.Join(procRoot, strconv.Itoa(pid), "cwd")); err == nil && dir != "" {
		return fmt.Sprintf("pid %d in %s", pid, dir)
	}
	if len(command) > 60 {
		command = command[:60] + "…"
	}
	return fmt.Sprintf("pid %d (%s)", pid, command)
}
