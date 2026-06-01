# proj - PowerShell shim. Source from $PROFILE:
#   . \\wsl.localhost\<distro>\<path>\proj.ps1
# or paste the function below directly into $PROFILE.
#
# proj lives inside WSL; this shim forwards every subcommand via
# `wsl.exe -e bash -lc 'proj "$@"' bash ...`. The login shell (-lc) is
# required so ~/.local/bin (where install.sh puts proj) is on PATH; the
# `-e` flag is required so wsl.exe passes trailing args to bash instead
# of dropping them.
#
# Special case: `proj cd <name>` resolves the project's Linux path and
# drops you into an interactive WSL shell at that path. Exit returns to
# PowerShell. This diverges from the Linux shims (which `cd` the current
# shell) because a PowerShell session sitting at a UNC path can't run
# Linux tools; a real WSL shell at the dir is what's actually useful.

function proj {
    if ($args.Count -ge 1 -and $args[0] -eq 'cd') {
        $rest = @($args | Select-Object -Skip 1)
        $p = (& wsl.exe -e bash -lc 'proj path "$@"' bash @rest | Out-String).Trim()
        if ($LASTEXITCODE -eq 0 -and $p) {
            & wsl.exe --cd $p
        }
    } else {
        & wsl.exe -e bash -lc 'proj "$@"' bash @args
    }
}
