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
