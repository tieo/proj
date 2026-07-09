# proj

A tmux-first project session manager with a built-in daemon (`proj daemon`)
that auto-resumes Claude Code sessions when their usage-limit cooldown ends.

One tmux session per project, one binary, one process per shell.

## Quick start

```sh
git clone https://github.com/tieo/proj
cd proj
./install.sh                # builds, installs the shim, enables the service
```

`./install.sh` needs Go on PATH. On NixOS:

```sh
nix develop          # drops you into a shell with go, gopls, tmux
./install.sh
```

Open a new shell, then:

```sh
proj list                  # show projects with session status
proj myapi work go         # create ~/projects/code/myapi/ with tags [work, go]
proj myapi                 # open it later
proj cd myapi              # cd into the project's directory
proj daemon               # show daemon status + pending resumes
```

## Layout

`proj` keeps projects on disk under one base directory (default
`~/projects/code/`), organised flat as `<base>/<name>/`. Tags live in a
single global registry at `~/.config/proj/projects.toml`, not in the
project directories themselves (so projects stay free of proj-specific
files and don't need to gitignore anything):

```toml
[projects.myapi]
tags = ["work", "go"]

[projects.cli]
tags = ["oss"]
agent = "codex"
```

Any direct child directory of `base_dir` counts as a project; an entry
in the registry is optional and a project without one is just untagged.
The tmux session name is the sorted tags joined to the name by `_`, so
the same project always resolves to the same session: `myapi` with tags
`[work, go]` becomes session `go_work_myapi`. Untagged projects use just
`<name>`.

Opening a project creates the session detached, runs the project's coding
agent in it, and attaches. Re-running `proj <name>` later just attaches.
The default agent is Claude Code
(`claude --dangerously-skip-permissions --remote-control …`); per project
another agent can be selected with `proj agent <name> codex` (built-ins:
`claude`, `codex`, `agy`; more via `[agents.<name>]` in the config). When a
project has prior history for its agent, the session launches the agent's
resume command instead (`claude … -c`, `codex resume --last …`), so a
recreated session continues where it left off.

## Commands

| Command | What it does |
| --- | --- |
| `proj <name-or-prefix>` | open an existing project by name or unique prefix (names are unique) |
| `proj new <name> <tag>...` | create a project: first arg is the name, the rest are tags. Quote a multi-word name. `--agent codex` selects the coding agent. |
| `proj agent <name> [agent]` | show or set the project's coding agent (`claude`, `codex`, `agy`, or a `[agents.*]` entry); applies on the next launch |
| `proj list [--tag <t>]` | active projects first, then idle, then orphan tmux sessions; `--tag` filters |
| `proj cd <name-or-prefix>` | cd the current shell into the project (needs the shim) |
| `proj path <name-or-prefix>` | print the project's absolute path |
| `proj tag add <name> <tag>...` | add tags to an existing project |
| `proj tag rm <name> <tag>...` | remove tags from a project |
| `proj tag set <name> [<tag>...]` | replace the project's tags |
| `proj pin <name>` / `proj unpin <name>` | pin a project so the daemon always recreates its session (or remove the pin) |
| `proj close [name] [--force]` | kill a project's session and mark it intentionally closed; `--force` also unpins. No arg = current session |
| `proj rm <name-or-prefix>` | delete the project directory (asks first) |
| `proj rename <old> <new>` | rename dir + session (also moves Claude's history folder when resolvable) |
| `proj clean [--days N]` | kill tmux sessions idle longer than N days (default 7) |

### `proj daemon`

Watches every tmux pane (~60s default) for Claude Code's blocking banner:

```
  ⎿  You're out of extra usage · resets 3am (Europe/Berlin)
```

When it sees a banner, it acts:

1. If Claude is showing the `/rate-limit-options` selector ("What do you
   want to do?"), the daemon sends `Escape` first to dismiss it.
2. Then sends `continue<Enter>`.

**Try then verify, don't pre-guess the reset time.** The first attempt is
always immediate. On the next poll, if the banner is gone, the resume
worked. If it's still there, the daemon schedules the next attempt at the
banner's parsed clock time (advanced to the next future occurrence),
capped at `max_wait` (5h default).

**False-positive resistance.** A banner only counts if the matching line
starts with Claude's tool-output continuation marker (`⎿`). Prose
mentioning the phrase, user-typed text containing it, code-block quotes,
and so on are all rejected; only real TUI tool output triggers an action.

Sessions without the banner are never touched.

**Claude-only automation.** Everything above speaks Claude Code's TUI, so
the daemon applies it only to panes whose project runs the claude agent.
Sessions on another agent (codex, agy) still get the session-level care:
pinned and keep-alive recreation, with the agent's own resume command.

| Command | What it does |
| --- | --- |
| `proj daemon` | status: service state, tracked sessions, next resume time |
| `proj daemon run` | run the daemon in foreground (what the service unit calls) |
| `proj daemon start` / `stop` / `restart` | manage the systemd user service |
| `proj daemon enable` / `disable` | enable+start / stop+disable |
| `proj daemon logs` | `journalctl --user -u proj-daemon -f` |

On macOS, `enable`/`disable`/`start`/`stop` are not wired up; use `launchctl`
on `gui/$UID/com.proj.daemon` directly.

## Config

Optional. Defaults are usable. `~/.config/proj/config.toml`:

```toml
base_dir = "~/projects/code"

[claude]
command     = "claude --dangerously-skip-permissions --remote-control --remote-control-session-name-prefix {name} -n {name}"
resume_flag = "-c"

# Other coding agents, selectable per project with `proj agent <name> <agent>`.
# codex and agy ship as built-ins with these defaults; an [agents.<name>]
# entry replaces the whole built-in recipe for that name, and new names
# define new agents. resume_command is used instead of command when the
# project has prior history for the agent.
[agents.codex]
command        = "codex --dangerously-bypass-approvals-and-sandbox"
resume_command = "codex resume --last --dangerously-bypass-approvals-and-sandbox"

[agents.agy]
command = "agy"

[daemon]
poll_interval = "60s"
max_wait      = "5h"   # fallback retry interval when the banner has no parseable time
jitter        = "1s"   # added to the scheduled retry time
resume_text   = "continue"
capture_lines = 300
```

`{name}` and `{dir}` are substituted at session-creation time.

## Uninstall

```sh
./install.sh --uninstall
```

Leaves the `source …/proj.{zsh,bash,fish}` line in your shell rc; remove it
manually.

## Windows (WSL)

proj runs inside WSL on Windows. tmux, the daemon, and Claude Code all live
on the Linux side; PowerShell calls into it through a thin shim. Project
files stay on WSL's ext4 (fast for git/go/node) and are reachable from
Windows via the `\\wsl.localhost\<distro>\...` UNC path when needed.

1. Inside WSL: install proj as usual (see Quick start above).

2. On the PowerShell side: dot-source `shells/proj.ps1` from your
   `$PROFILE`, or paste its function in directly:

   ```powershell
   . \\wsl.localhost\Ubuntu-24.04\home\<user>\projects\go\proj\shells\proj.ps1
   ```

   Then in PowerShell, `proj list` / `proj go myapi` / `proj cd myapi` work
   as on Linux. Difference: `proj cd <name>` on Windows drops you into an
   interactive WSL shell at the project directory (exit returns to
   PowerShell), instead of changing the PowerShell pwd. PowerShell can't
   run Linux tools at a UNC path, so a real WSL shell at the dir is what's
   actually useful. From there you can `code .`, `explorer.exe .`, run
   git/go, or just stay there to work.

Caveat: corporate VPNs (Cisco AnyConnect, GlobalProtect) often block WSL2's
NAT egress. If the daemon can't reach the network with the VPN on,
disconnect VPN for WSL work, or enable `networkingMode=mirrored` in
`%USERPROFILE%\.wslconfig` (requires the full Hyper-V Platform Windows
feature, which corp policy may restrict).

## Requirements

- `tmux`
- `claude` (Claude Code CLI), runs inside each session; configurable via `[claude] command`
- optionally `codex` / `agy` (or any other agent CLI) for projects switched to another agent
- Go 1.22+ (build-time only)
- Linux (systemd user instance) or macOS (launchd) or Windows via WSL2
  (see above). The binary itself runs on any unix; only the
  service-management subcommands are OS-specific.

## License

MIT, see [LICENSE](LICENSE).
