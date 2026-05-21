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
