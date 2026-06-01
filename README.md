# proj

A tmux-first project session manager with a built-in daemon (`proj unreset`)
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
proj list            # show projects with session status
proj go myapi        # create / open ~/projects/code/go/myapi in tmux
proj cd myapi        # cd into the project's directory
proj unreset         # show daemon status + pending resumes
```

## Layout

`proj` keeps projects on disk under one base directory (default
`~/projects/code/`), organised as `<base>/<lang>/<name>/`. The tmux session
name is derived from the project name (`.` and `:` become `_`).

Opening a project creates the session detached, runs Claude Code in it
(`claude --dangerously-skip-permissions --remote-control …`), and attaches.
Re-running `proj <name>` later just attaches.

## Commands

| Command | What it does |
| --- | --- |
| `proj <name>` | open the named project (must already exist) |
| `proj <lang> <name>` | create the dir if absent, then open |
| `proj new` | interactive wizard (asks lang, name, optional description) |
| `proj list` | active projects first, then idle, then orphan tmux sessions |
| `proj cd <name>` | cd the current shell into the project (needs the shim) |
| `proj path <name>` | print the project's absolute path |
| `proj kill <name>` | kill the project's tmux session |
| `proj rm <name>` | delete the project directory (asks first) |
| `proj rename <old> <new>` | rename dir + session |
| `proj clean [--days N]` | kill tmux sessions idle longer than N days (default 7) |

### `proj unreset` (the daemon)

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

| Command | What it does |
| --- | --- |
| `proj unreset` | status: service state, tracked sessions, next resume time |
| `proj unreset run` | run the daemon in foreground (what the service unit calls) |
| `proj unreset start` / `stop` / `restart` | manage the systemd user service |
| `proj unreset enable` / `disable` | enable+start / stop+disable |
| `proj unreset logs` | `journalctl --user -u proj-unreset -f` |

On macOS, `enable`/`disable`/`start`/`stop` are not wired up; use `launchctl`
on `gui/$UID/com.proj.unreset` directly.

## Config

Optional. Defaults are usable. `~/.config/proj/config.toml`:

```toml
base_dir     = "~/projects/code"
default_lang = "polyglot"      # what `proj new` uses if you skip the language prompt

[claude]
command     = "claude --dangerously-skip-permissions --remote-control --remote-control-session-name-prefix {name} -n {name}"
resume_flag = "-c"

[unreset]
poll_interval = "60s"
max_wait      = "5h"   # upper bound between retry attempts on the same pane
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
- Go 1.22+ (build-time only)
- Linux (systemd user instance) or macOS (launchd) or Windows via WSL2
  (see above). The binary itself runs on any unix; only the
  service-management subcommands are OS-specific.

## License

MIT, see [LICENSE](LICENSE).
