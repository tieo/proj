#!/usr/bin/env bash
# proj — install script. Builds the binary, installs the shell shim, and
# wires up the unreset service unit for the current OS.
#
# Usage:
#   ./install.sh                # full install
#   ./install.sh --no-service   # skip enabling the service unit
#   ./install.sh --uninstall    # remove what install put in place
#   ./install.sh --force        # don't prompt on conflicts (overwrite/proceed)
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
BIN_DIR="${XDG_BIN_HOME:-$HOME/.local/bin}"
UNIT_DIR="$HOME/.config/systemd/user"
AGENT_DIR="$HOME/Library/LaunchAgents"
UNIT_PATH="$UNIT_DIR/proj-unreset.service"
PLIST_PATH="$AGENT_DIR/com.proj.unreset.plist"

INSTALL_SERVICE=1
UNINSTALL=0
FORCE=0

for arg in "$@"; do
    case "$arg" in
        --no-service) INSTALL_SERVICE=0 ;;
        --uninstall)  UNINSTALL=1 ;;
        --force)      FORCE=1 ;;
        -h|--help)    sed -n '2,10p' "$0"; exit 0 ;;
        *) echo "unknown flag: $arg" >&2; exit 2 ;;
    esac
done

# ----- helpers -----

is_nix_path()   { [[ "$1" == /nix/store/* ]]; }
is_nix_symlink() { [[ -L "$1" && "$(readlink "$1" 2>/dev/null)" == /nix/store/* ]]; }

# yes-or-no prompt. With --force, always yes. Without a TTY, always no
# (caller decides whether to abort).
confirm() {
    local msg="$1"
    if (( FORCE )); then return 0; fi
    if [[ ! -t 0 ]]; then
        echo "  (non-interactive; pass --force to proceed)" >&2
        return 1
    fi
    read -r -p "$msg [y/N] " ans
    [[ "$ans" =~ ^[Yy] ]]
}

backup_if_modified() {
    local src="$1" dst="$2"
    [[ -f "$dst" && ! -L "$dst" ]] || return 0
    if ! cmp -s "$src" "$dst"; then
        local bak="$dst.bak.$(date +%Y%m%d-%H%M%S)"
        echo "  backing up modified $dst → $bak"
        cp -p "$dst" "$bak"
    fi
}

# ----- uninstall -----

uninstall() {
    if is_nix_symlink "$UNIT_PATH"; then
        echo "$UNIT_PATH is managed by nix (home-manager); uninstall via your nix config."
        exit 0
    fi
    case "$(uname -s)" in
        Linux)
            systemctl --user disable --now proj-unreset 2>/dev/null || true
            rm -f "$UNIT_PATH"
            systemctl --user daemon-reload || true
            ;;
        Darwin)
            launchctl bootout "gui/$UID/com.proj.unreset" 2>/dev/null \
                || launchctl unload "$PLIST_PATH" 2>/dev/null || true
            rm -f "$PLIST_PATH"
            ;;
    esac
    rm -f "$BIN_DIR/proj"
    echo "uninstalled. shell shim source line in your rc file is yours to remove."
}

if (( UNINSTALL )); then uninstall; exit 0; fi

# ----- pre-flight conflict detection -----

# 1) Existing proj on PATH that isn't our target?
existing=$(command -v proj 2>/dev/null || true)
if [[ -n "$existing" ]]; then
    resolved=$(readlink -f "$existing")
    target="$BIN_DIR/proj"
    if is_nix_path "$resolved"; then
        echo "proj is already installed via nix at: $resolved"
        echo "use that instead — this installer is for non-nix systems."
        echo "to uninstall the nix one, remove it from your home-manager config."
        exit 0
    elif [[ "$resolved" != "$(readlink -f "$target" 2>/dev/null || echo "$target")" ]]; then
        echo "⚠  found a different proj already on PATH:"
        echo "      $resolved"
        echo "  this installer would put one at:"
        echo "      $target"
        echo "  both will coexist; PATH ordering decides which 'proj' runs."
        confirm "  proceed anyway?" || { echo "aborted."; exit 1; }
    fi
fi

# 2) Service-unit conflict with home-manager?
if is_nix_symlink "$UNIT_PATH"; then
    echo "$UNIT_PATH is managed by nix (home-manager); refusing to overwrite."
    exit 0
fi

# ----- build -----

if ! command -v go >/dev/null; then
    echo "error: 'go' not found in PATH" >&2
    echo "  on NixOS: nix-shell -p go --run './install.sh' (or 'nix develop')" >&2
    exit 1
fi

mkdir -p "$BIN_DIR"
echo "→ building proj"
(cd "$HERE" && go build -o "$BIN_DIR/proj" ./cmd/proj)
echo "  installed $BIN_DIR/proj"

# ----- shell shim source line -----

shell_name=$(basename "${SHELL:-/bin/bash}")
case "$shell_name" in
    zsh)  SHIM="$HERE/shells/proj.zsh";  RC="$HOME/.zshrc" ;;
    bash) SHIM="$HERE/shells/proj.bash"; RC="$HOME/.bashrc" ;;
    fish) SHIM="$HERE/shells/proj.fish"; RC="$HOME/.config/fish/config.fish" ;;
    *)    SHIM=""; RC="" ;;
esac
if [[ -n "$SHIM" && -n "$RC" ]]; then
    shim_base=$(basename "$SHIM")
    # Grep by filename so an earlier install from a moved repo path still
    # counts as "already wired" and we don't append a duplicate.
    if grep -qF "$shim_base" "$RC" 2>/dev/null; then
        if ! grep -qF "$SHIM" "$RC" 2>/dev/null; then
            echo "⚠ $RC already sources a $shim_base from a different path — leave the"
            echo "  existing line alone, or update it to:  source $SHIM"
        fi
    elif [[ -e "$RC" && ! -w "$RC" ]]; then
        echo "⚠ $RC is not writable (managed by home-manager / nix?)"
        echo "  add this line to your shell config manually:"
        echo "      source $SHIM"
    else
        echo "→ adding shim source line to $RC"
        { echo; echo "# proj — shell shim (for 'proj cd')"; echo "source $SHIM"; } >> "$RC"
    fi
fi

# ----- service unit -----

if (( INSTALL_SERVICE )); then
    case "$(uname -s)" in
        Linux)
            mkdir -p "$UNIT_DIR"
            backup_if_modified "$HERE/service/systemd/proj-unreset.service" "$UNIT_PATH"
            install -m 0644 "$HERE/service/systemd/proj-unreset.service" "$UNIT_PATH"
            echo "→ reloading and restarting systemd service"
            systemctl --user daemon-reload
            systemctl --user enable proj-unreset.service
            systemctl --user restart proj-unreset.service
            echo "  enabled and restarted proj-unreset — tail with: journalctl --user -u proj-unreset -f"
            ;;
        Darwin)
            mkdir -p "$AGENT_DIR"
            tmp_plist=$(mktemp)
            sed "s|__HOME__|$HOME|g" "$HERE/service/launchd/com.proj.unreset.plist" > "$tmp_plist"
            backup_if_modified "$tmp_plist" "$PLIST_PATH"
            install -m 0644 "$tmp_plist" "$PLIST_PATH"
            rm -f "$tmp_plist"
            launchctl bootout "gui/$UID/com.proj.unreset" 2>/dev/null || true
            launchctl bootstrap "gui/$UID" "$PLIST_PATH" 2>/dev/null \
                || launchctl load -w "$PLIST_PATH"
            echo "  loaded com.proj.unreset — logs at ~/.local/state/proj/unreset.log"
            ;;
        *)
            echo "  (skipping service install — unsupported OS)"
            ;;
    esac
fi

echo
echo "done. open a new shell, then try:"
echo "  proj list"
echo "  proj unreset"
