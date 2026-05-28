# proj — fish shim. Source from config.fish:
#   source /path/to/proj/shells/proj.fish

function proj
    if test (count $argv) -ge 1; and test $argv[1] = "cd"
        set -l dir (command proj path $argv[2..-1])
        or return $status
        builtin cd $dir
    else
        command proj $argv
    end
end

# Keep-alive integration: mark the session as intentionally closed on shell exit.
if set -q TMUX
    function _proj_on_shell_exit --on-event fish_exit
        set -l _proj_sess (tmux display-message -p '#S' 2>/dev/null)
        and command proj unreset mark-closed "$_proj_sess" 2>/dev/null
        or true
    end
end
