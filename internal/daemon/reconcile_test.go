package daemon

import (
	"testing"

	"github.com/tieo/proj/internal/tmux"
)

func TestMergeRenamedAliases(t *testing.T) {
	const projDir = "/home/u/projects/code/proj"

	managed := ManagedState{
		// Stale alias of the proj session under its old name; carries the pin.
		"go_tools_proj": {Name: "go_tools_proj", Dir: projDir, Pinned: true},
		// The live proj session under its current name; unpinned.
		"proj@go+tools": {Name: "proj@go+tools", Dir: projDir},
		// Unrelated stale entry whose dir has no live session; must be kept.
		"old_keepalive": {Name: "old_keepalive", Dir: "/home/u/projects/code/gone", KeepAlive: true},
	}
	liveSessionMap := map[string]tmux.Session{
		"proj@go+tools": {Name: "proj@go+tools", Path: projDir},
	}
	liveNameByDir := map[string]string{projDir: "proj@go+tools"}

	mergeRenamedAliases(managed, liveSessionMap, liveNameByDir)

	if _, ok := managed["go_tools_proj"]; ok {
		t.Error("stale alias go_tools_proj should have been removed")
	}
	if !managed["proj@go+tools"].Pinned {
		t.Error("pin should have migrated onto the live session proj@go+tools")
	}
	if _, ok := managed["old_keepalive"]; !ok {
		t.Error("stale entry with no live session in its dir should be kept (may need recreating)")
	}
}
