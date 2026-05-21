# proj — bash shim. Source from .bashrc:
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
