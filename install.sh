#!/usr/bin/env bash
# proj — install script. Builds the binary, installs the shell shim, and
# wires up the unreset service unit for the current OS.
#
# Usage:
#   ./install.sh                # full install
#   ./install.sh --no-service   # skip enabling the service
#   ./install.sh --uninstall    # remove what install put in place
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
BIN_DIR="${XDG_BIN_HOME:-$HOME/.local/bin}"
INSTALL_SERVICE=1
UNINSTALL=0

for arg in "$@"; do
    case "$arg" in
        --no-service) INSTALL_SERVICE=0 ;;
        --uninstall)  UNINSTALL=1 ;;
        -h|--help)
            sed -n '2,9p' "$0"
            exit 0
            ;;
        *) echo "unknown flag: $arg" >&2; exit 2 ;;
    esac
done

uninstall() {
    case "$(uname -s)" in
        Linux)
            systemctl --user disable --now proj-unreset 2>/dev/null || true
            rm -f "$HOME/.config/systemd/user/proj-unreset.service"
            systemctl --user daemon-reload || true
            ;;
        Darwin)
            launchctl bootout "gui/$UID/com.proj.unreset" 2>/dev/null || true
            rm -f "$HOME/Library/LaunchAgents/com.proj.unreset.plist"
            ;;
    esac
    rm -f "$BIN_DIR/proj"
    echo "uninstalled. shell shim source line in your rc file is yours to remove."
}

if (( UNINSTALL )); then
    uninstall
    exit 0
fi

if ! command -v go >/dev/null; then
    echo "error: 'go' not found in PATH" >&2
    echo "  on NixOS: nix-shell -p go --run './install.sh' (or 'nix develop')" >&2
    exit 1
fi

mkdir -p "$BIN_DIR"
echo "→ building proj"
(cd "$HERE" && go build -o "$BIN_DIR/proj" ./cmd/proj)
echo "  installed $BIN_DIR/proj"

shell_name=$(basename "${SHELL:-/bin/bash}")
case "$shell_name" in
    zsh)  SHIM="$HERE/shells/proj.zsh";  RC="$HOME/.zshrc" ;;
    bash) SHIM="$HERE/shells/proj.bash"; RC="$HOME/.bashrc" ;;
    fish) SHIM="$HERE/shells/proj.fish"; RC="$HOME/.config/fish/config.fish" ;;
    *)    SHIM=""; RC="" ;;
esac
if [[ -n "$SHIM" && -n "$RC" ]] && ! grep -Fq "$SHIM" "$RC" 2>/dev/null; then
    if [[ -e "$RC" && ! -w "$RC" ]]; then
        echo "⚠ $RC is not writable (managed by home-manager / nix?)"
        echo "  Add this line to your shell config manually:"
        echo "      source $SHIM"
    else
        echo "→ adding shim source line to $RC"
        {
            echo
            echo "# proj — shell shim (for 'proj cd')"
            echo "source $SHIM"
        } >> "$RC"
    fi
fi

if (( INSTALL_SERVICE )); then
    case "$(uname -s)" in
        Linux)
            UNIT_DIR="$HOME/.config/systemd/user"
            mkdir -p "$UNIT_DIR"
            install -m 0644 "$HERE/service/systemd/proj-unreset.service" \
                "$UNIT_DIR/proj-unreset.service"
            
            echo "→ reloading and restarting systemd service"
            systemctl --user daemon-reload
            systemctl --user enable proj-unreset.service
            systemctl --user restart proj-unreset.service
            
            echo "  enabled and restarted proj-unreset.service — tail with: journalctl --user -u proj-unreset -f"
            ;;
        Darwin)
            AGENT_DIR="$HOME/Library/LaunchAgents"
            mkdir -p "$AGENT_DIR"
            sed "s|__HOME__|$HOME|g" "$HERE/service/launchd/com.proj.unreset.plist" \
                > "$AGENT_DIR/com.proj.unreset.plist"
            launchctl bootstrap "gui/$UID" "$AGENT_DIR/com.proj.unreset.plist" 2>/dev/null \
                || launchctl load -w "$AGENT_DIR/com.proj.unreset.plist"
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
