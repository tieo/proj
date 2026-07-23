package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tieo/proj/internal/config"
	"github.com/tieo/proj/internal/daemon"
	"github.com/tieo/proj/internal/projects"
	"github.com/tieo/proj/internal/tmux"
)

// Doner keeps a tagged session working until it reports done. It is a Claude
// Code Stop hook, not a daemon behaviour: when a session would end its turn, the
// hook runs, and for a doner-tagged project it either lets the session stop
// (the last reply reads as "done") or blocks the stop and tells the model to
// continue. The whole control surface is the "doner" tag; the hook is installed
// once and reads the tag live, so tagging a project is all a user does.
//
// The hook handler is this same proj binary (`proj doner-hook`), so the logic is
// one Go implementation that runs identically on Linux, macOS, WSL and Windows -
// no bash/jq/python in the hook. Only the command string Claude Code invokes
// differs per platform, and donerHookCommand builds the right one at install.

// DonerTag opts a project into the doner backstop.
const DonerTag = "doner"

// donerReason is injected into the model when the hook blocks a stop. It asks
// for a bare "Yes" on completion so the done-check stays a keyword match.
const donerReason = "done? if not, continue. If you are truly finished, reply with exactly: Yes"

var donerCmd = &cobra.Command{
	Use:   "doner [on|off]",
	Short: "keep doner-tagged sessions working until they report done (Stop hook)",
	Long: `Doner keeps a session working until it says it is finished. Tag a project
with "doner" (` + "`proj tag add <name> doner`" + `) and, whenever that session would
stop, a Claude Code Stop hook checks its last message: an affirmative ("Yes",
"done", ...) lets it stop, anything else makes it continue.

With no argument, shows whether doner is installed and on, and which projects
carry the tag. "on"/"off" toggle it globally without untouching the tags.
"install"/"uninstall" add or remove the Stop hook from Claude Code's settings.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runDoner,
}

var (
	donerInstallCmd   = &cobra.Command{Use: "install", Args: cobra.NoArgs, Short: "add the doner Stop hook to Claude Code settings", RunE: func(*cobra.Command, []string) error { return donerInstall(true) }}
	donerUninstallCmd = &cobra.Command{Use: "uninstall", Args: cobra.NoArgs, Short: "remove the doner Stop hook from Claude Code settings", RunE: func(*cobra.Command, []string) error { return donerInstall(false) }}
	donerHookCmd      = &cobra.Command{Use: "doner-hook", Hidden: true, Args: cobra.NoArgs, Short: "Stop-hook handler (reads hook JSON on stdin)", RunE: runDonerHook}
	donerToggleCmd    = &cobra.Command{
		Use:   "toggle",
		Short: "turn doner on or off for the project in the current directory",
		Long: `Add or remove the doner tag on the project the working directory belongs to.
This is what the /doner slash command runs, so a session can put itself on the
doner leash (or take itself off) without leaving the session.`,
		Args: cobra.NoArgs,
		RunE: runDonerToggle,
	}
)

func init() {
	donerCmd.AddCommand(donerInstallCmd, donerUninstallCmd, donerToggleCmd)
	rootCmd.AddCommand(donerCmd, donerHookCmd)
}

// runDonerToggle flips the doner tag on the project owning the working
// directory. Going through mutateTags rather than the registry directly keeps a
// toggle identical to `proj tag add/rm`: the tmux session is renamed to match
// its new tags and the Remote Control title follows.
func runDonerToggle(cmd *cobra.Command, args []string) error {
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	name := lastPathSegment(wd)
	if name == "" {
		return fmt.Errorf("cannot tell which project %s belongs to", wd)
	}
	on := false
	if err := mutateTags(name, func(current []string) []string {
		for i, t := range current {
			if t == DonerTag {
				return append(current[:i], current[i+1:]...)
			}
		}
		on = true
		return append(current, DonerTag)
	}); err != nil {
		return err
	}
	if on {
		fmt.Printf("doner on for %s: this session now keeps working until it reports done\n", name)
	} else {
		fmt.Printf("doner off for %s\n", name)
	}
	return nil
}

func runDoner(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if len(args) == 0 {
		printDonerStatus(cfg)
		return nil
	}
	switch args[0] {
	case "on":
		cfg.Daemon.Doner.Enabled = true
	case "off":
		cfg.Daemon.Doner.Enabled = false
	default:
		return fmt.Errorf("expected on or off, got %q", args[0])
	}
	if err := config.Write(cfg); err != nil {
		return err
	}
	fmt.Printf("doner: %s\n", args[0])
	if cfg.Daemon.Doner.Enabled && !donerHookInstalled(cfg) {
		fmt.Println("note: the Stop hook is not installed yet — run `proj doner install`")
	}
	return nil
}

func printDonerStatus(cfg config.Config) {
	on := cfg.Daemon.Doner.Active()
	installed := donerHookInstalled(cfg)
	fmt.Printf("doner: %s, hook %s\n", onOff(on), map[bool]string{true: "installed", false: "not installed"}[installed])
	tagged := donerProjects()
	if len(tagged) == 0 {
		fmt.Println("no projects tagged (tag one with `proj tag add <name> doner`)")
		return
	}
	fmt.Printf("tagged: %s\n", strings.Join(tagged, ", "))
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// donerProjects returns the names of projects carrying the doner tag, sorted.
func donerProjects() []string {
	reg, err := projects.LoadRegistry()
	if err != nil {
		return nil
	}
	var out []string
	for name := range reg.Projects {
		for _, t := range reg.Tags(name) {
			if t == DonerTag {
				out = append(out, name)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// runDonerHook is the Stop-hook handler. It reads Claude Code's hook JSON on
// stdin and, for a doner-tagged session that has not reported done, prints a
// block decision so the session keeps going; otherwise it exits silently to let
// the session stop.
func runDonerHook(cmd *cobra.Command, args []string) error {
	raw, _ := io.ReadAll(os.Stdin)
	var in struct {
		StopHookActive       bool   `json:"stop_hook_active"`
		Cwd                  string `json:"cwd"`
		LastAssistantMessage string `json:"last_assistant_message"`
	}
	// A hook must never fail loudly: an unreadable payload just means "let it
	// stop", never a blocked session or a visible error.
	if json.Unmarshal(raw, &in) != nil {
		return nil
	}
	if in.StopHookActive {
		return nil // loop guard: Claude Code already re-drove on our block
	}
	cfg, err := config.Load()
	if err != nil || !cfg.Daemon.Doner.Active() {
		return nil
	}
	if !projectHasDonerTag(in.Cwd) {
		return nil
	}
	if isDone(in.LastAssistantMessage) {
		return nil // the session reported finished
	}
	out, _ := json.Marshal(map[string]string{"decision": "block", "reason": donerReason})
	fmt.Println(string(out))
	return nil
}

// projectHasDonerTag reports whether the project a hook fired for carries the
// doner tag. The project name is the last path segment of the session's cwd,
// whatever form it takes (a \\wsl.localhost UNC path, a Windows drive path, or a
// plain Linux path), so no filesystem mapping is needed - the registry is keyed
// by that name.
func projectHasDonerTag(cwd string) bool {
	name := lastPathSegment(cwd)
	if name == "" {
		return false
	}
	reg, err := projects.LoadRegistry()
	if err != nil {
		return false
	}
	for _, t := range reg.Tags(name) {
		if t == DonerTag {
			return true
		}
	}
	return false
}

func lastPathSegment(p string) string {
	p = strings.TrimRight(p, `/\`)
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

// doneReplies are the affirmatives a session sends when it reports finished. The
// hook asks for "Yes"; the rest cover the phrasings a cooperating session still
// tends to use.
var doneReplies = map[string]bool{
	"yes": true, "yep": true, "yeah": true, "yup": true, "y": true,
	"done": true, "complete": true, "completed": true, "finished": true,
	"all done": true, "yes done": true, "task complete": true,
	"task completed": true, "yes complete": true, "yes finished": true,
	"already done": true, "its done": true,
}

// isDone reports whether an assistant reply reads as "finished". It is a whole-
// message match against a small affirmative set, never a substring test: a long
// message that merely ends in "yes" is work, not a completion report, so the
// text is reduced to lowercase letters and single spaces and matched whole.
func isDone(text string) bool {
	var b strings.Builder
	prevSpace := false
	for _, r := range strings.ToLower(text) {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
			prevSpace = false
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			if !prevSpace && b.Len() > 0 {
				b.WriteByte(' ')
			}
			prevSpace = true
		}
	}
	return doneReplies[strings.TrimSpace(b.String())]
}

// ----- hook installation -----

// donerSettingsPath is Claude Code's settings.json: under WSL this is the
// Windows-side .claude that claude.exe actually reads, which daemon.ClaudeRoot
// resolves.
func donerSettingsPath(cfg config.Config) string {
	return filepath.Join(daemon.ClaudeRoot(cfg.Claude.Home), "settings.json")
}

// donerHookCommand is the command string Claude Code runs for the Stop hook. The
// handler is always this proj binary; only how it is reached differs. Under WSL,
// claude.exe runs hooks through Git Bash, which both needs wsl.exe to cross into
// the distro and mangles Linux paths unless MSYS_NO_PATHCONV is set.
func donerHookCommand() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if tmux.IsWSL() {
		return "MSYS_NO_PATHCONV=1 wsl.exe " + exe + " doner-hook", nil
	}
	return quoteIfSpace(exe) + " doner-hook", nil
}

func quoteIfSpace(s string) string {
	if strings.ContainsAny(s, " \t") {
		return `"` + s + `"`
	}
	return s
}

// isDonerHookEntry reports whether a Stop-matcher entry is ours, by the
// doner-hook subcommand its command carries.
func isDonerHookEntry(entry any) bool {
	m, ok := entry.(map[string]any)
	if !ok {
		return false
	}
	hooks, ok := m["hooks"].([]any)
	if !ok {
		return false
	}
	for _, h := range hooks {
		hm, ok := h.(map[string]any)
		if !ok {
			continue
		}
		if cmd, _ := hm["command"].(string); strings.Contains(cmd, "doner-hook") {
			return true
		}
	}
	return false
}

func donerHookInstalled(cfg config.Config) bool {
	root, _ := readSettings(donerSettingsPath(cfg))
	for _, e := range stopMatchers(root) {
		if isDonerHookEntry(e) {
			return true
		}
	}
	return false
}

// donerInstall adds (install=true) or removes the doner Stop hook from Claude
// Code's settings.json, leaving every other setting and hook untouched.
func donerInstall(install bool) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	path := donerSettingsPath(cfg)
	root, err := readSettings(path)
	if err != nil {
		return err
	}

	matchers := stopMatchers(root)
	kept := make([]any, 0, len(matchers))
	for _, e := range matchers {
		if !isDonerHookEntry(e) {
			kept = append(kept, e)
		}
	}
	if install {
		cmdStr, err := donerHookCommand()
		if err != nil {
			return err
		}
		kept = append(kept, map[string]any{
			"hooks": []any{map[string]any{"type": "command", "command": cmdStr}},
		})
	}
	setStopMatchers(root, kept)

	if err := writeSettings(path, root); err != nil {
		return err
	}
	if err := writeDonerSlashCommand(cfg, install); err != nil {
		// The hook is the feature; the slash command is a convenience, so a
		// failure to write it must not fail the install.
		fmt.Fprintf(os.Stderr, "warning: /doner slash command: %v\n", err)
	}
	if install {
		fmt.Printf("doner Stop hook installed in %s\n", path)
		if !cfg.Daemon.Doner.Active() {
			fmt.Println("note: doner is off — turn it on with `proj doner on`")
		}
	} else {
		fmt.Printf("doner Stop hook removed from %s\n", path)
	}
	return nil
}

// donerSlashCommand is the /doner command body. It tells the session to run the
// toggle rather than executing it inline, so it does not depend on a particular
// Claude Code version's command-substitution syntax.
const donerSlashCommand = `---
description: Toggle doner for this project (keep working until you report done)
---
Run ` + "`proj doner toggle`" + ` and report its output in one short line. Do nothing else.

Doner is a Stop hook: while it is on, ending a turn is intercepted and you are
told to keep going unless your last message says you are finished. Reply with
exactly "Yes" when the work is genuinely complete.
`

// writeDonerSlashCommand installs or removes the /doner command in the commands
// directory Claude Code reads (the Windows-side one under WSL, alongside
// settings.json).
func writeDonerSlashCommand(cfg config.Config, install bool) error {
	path := filepath.Join(daemon.ClaudeRoot(cfg.Claude.Home), "commands", "doner.md")
	if !install {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(donerSlashCommand), 0o644)
}

func readSettings(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	root := map[string]any{}
	if len(strings.TrimSpace(string(data))) == 0 {
		return root, nil
	}
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return root, nil
}

func writeSettings(path string, root map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".proj-tmp"
	if err := os.WriteFile(tmp, append(out, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// stopMatchers returns the hooks.Stop array as a slice, or nil.
func stopMatchers(root map[string]any) []any {
	hooks, ok := root["hooks"].(map[string]any)
	if !ok {
		return nil
	}
	stop, _ := hooks["Stop"].([]any)
	return stop
}

// setStopMatchers writes the hooks.Stop array back, pruning empty containers so
// removing the last hook leaves clean settings rather than empty scaffolding.
func setStopMatchers(root map[string]any, matchers []any) {
	hooks, _ := root["hooks"].(map[string]any)
	if hooks == nil {
		if len(matchers) == 0 {
			return
		}
		hooks = map[string]any{}
		root["hooks"] = hooks
	}
	if len(matchers) == 0 {
		delete(hooks, "Stop")
		if len(hooks) == 0 {
			delete(root, "hooks")
		}
		return
	}
	hooks["Stop"] = matchers
}
