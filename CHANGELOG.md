# Changelog

## [Unreleased]

### Added
- Initial Go rewrite of `proj` (previously a zsh function).
- `proj unreset`: watches tmux panes for Claude Code's usage-limit banner
  and resumes them once the reset time passes.
- Cross-shell shims (zsh, bash, fish).
- Service units for systemd (Linux) and launchd (macOS).
- Optional TOML config at `~/.config/proj/config.toml`.
