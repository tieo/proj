# Changelog

## [Unreleased]

### Changed
- The model column reports the model a Claude project is configured to run
  (`--model` on the launch command, then project and user settings files),
  falling back to the model of its last recorded turn. A project whose
  settings moved to a newer model no longer shows the old one until it
  answers again.

### Added
- `proj sessions list`, `proj sessions prompts <id>` and `proj sessions fork
  <id> --into <project>` do headless what the picker did a keystroke at a
  time. `list --json` and `prompts --json` are machine-readable; `prompts`
  numbers the turns the way `--from/--to` count them and prints the size of
  each turn, so a range can be picked to fit a context window. `fork` creates
  the target project when the name is new and takes `--no-open` to write the
  transcript without starting its session.
- `proj say --now` interrupts whatever a session is doing and delivers the
  message as the next turn. Without it, a session busy with a foreground
  command is offered the TUI's own way out first (ctrl+b ctrl+b), so the
  message is read at once and the command keeps running; a session busy with
  anything else keeps the message queued until its turn ends.
- `proj switch <project> <tool>` carries the conversation into the new
  tool. Claude receives a native session; Codex receives a native thread
  built through its app-server, and a Claude source goes through Codex's
  own session importer so the thread renders in its UI; every other tool
  starts from a handoff prompt read out of a file, which keeps a large
  transcript under the tmux command-length boundary. Rollouts written
  before Codex required a provider field in their metadata are repaired so
  they stay resumable.
- Per-project coding tools: `proj tool <name> [tool]` and `proj new
  --tool` select which CLI a session runs (built-ins: claude, codex, agy;
  more via `[tools.<name>]` in the config). Sessions resume through the
  tool's own resume command (`claude -c`, `codex resume --last`,
  `agy --continue`), gated on
  real prior history per tool. The daemon's Claude-specific automation
  (banner resume, /compact recovery, RC watchdog) skips panes running other
  tools; keep-alive and pinned recreation cover every tool.
- Initial Go rewrite of `proj` (previously a zsh function).
- `proj daemon`: watches tmux panes for Claude Code's usage-limit banner
  and resumes them once the reset time passes.
- Cross-shell shims (zsh, bash, fish).
- Service units for systemd (Linux) and launchd (macOS).
- Optional TOML config at `~/.config/proj/config.toml`.
