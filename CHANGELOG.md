# Changelog

## [Unreleased]

### Added
- Per-project coding tools: `proj tool <name> [tool]` and `proj new
  --tool` select which CLI a session runs (built-ins: claude, codex, agy;
  more via `[tools.<name>]` in the config). Sessions resume through the
  tool's own resume command (`claude -c`, `codex resume --last`), gated on
  real prior history per tool. The daemon's Claude-specific automation
  (banner resume, /compact recovery, RC watchdog) skips panes running other
  tools; keep-alive and pinned recreation cover every tool.
- Initial Go rewrite of `proj` (previously a zsh function).
- `proj daemon`: watches tmux panes for Claude Code's usage-limit banner
  and resumes them once the reset time passes.
- Cross-shell shims (zsh, bash, fish).
- Service units for systemd (Linux) and launchd (macOS).
- Optional TOML config at `~/.config/proj/config.toml`.
