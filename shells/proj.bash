# proj; bash shim. Source from .bashrc:
#   source /path/to/proj/shells/proj.bash

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

# Keep-alive integration: mark the session as intentionally closed on shell exit.
if [[ -n "$TMUX" ]]; then
    _proj_on_shell_exit() {
        local _proj_sess
        # Resolve the *exiting* pane's session via $TMUX_PANE, not the active
        # client's session; `display-message -p '#S'` with no target returns
        # the latter and would mark-closed the wrong session.
        _proj_sess=$(tmux display-message -p -t "$TMUX_PANE" '#S' 2>/dev/null) || return
        command proj daemon mark-closed "$_proj_sess" 2>/dev/null || true
    }
    trap '_proj_on_shell_exit' EXIT
fi
