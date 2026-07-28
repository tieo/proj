package main

import "github.com/tieo/proj/internal/projstate"

// projStateDir is proj's state directory, with parts joined onto it. It is the
// package-local name for projstate.Dir, so callers here read as they did.
func projStateDir(parts ...string) string { return projstate.Dir(parts...) }
