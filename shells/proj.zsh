# proj — zsh shim. Source from .zshrc:
#   source /path/to/proj/shells/proj.zsh
#
# The shim only exists so `proj cd <name>` can change the current shell's
# working directory. Every other subcommand passes straight through to the
# proj binary.

proj() {
    case "${1:-}" in
        cd)
            shift
            local dir
            dir=$(command proj path "$@") || return $?
            builtin cd -- "$dir"
            ;;
        *)
            command proj "$@"
            ;;
    esac
}

# Keep-alive integration: tell proj unreset that this session was closed
# intentionally when the shell exits cleanly (user typed exit or Ctrl-D).
# Without this, a keep-alive or pinned session would be automatically
# recreated by the daemon after it vanishes.
if [[ -n "$TMUX" ]]; then
    autoload -Uz add-zsh-hook
    _proj_on_shell_exit() {
        local _proj_sess
        # Resolve the *exiting* pane's session via $TMUX_PANE, not the active
        # client's session — `display-message -p '#S'` with no target returns
        # the latter and would mark-closed the wrong session.
        _proj_sess=$(tmux display-message -p -t "$TMUX_PANE" '#S' 2>/dev/null) || return
        command proj unreset mark-closed "$_proj_sess" 2>/dev/null || true
    }
    add-zsh-hook zshexit _proj_on_shell_exit
fi
