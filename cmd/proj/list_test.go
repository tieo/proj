package main

import (
	"testing"

	"github.com/tieo/proj/internal/projects"
)

func TestShortPath(t *testing.T) {
	long := "/tmp/claude-1000/-home-user-projects-code-proj/scratchpad/agytest"
	got := shortPath(long, 32)
	if got != ".../scratchpad/agytest" {
		t.Fatalf("shortPath = %q", got)
	}
	if got := shortPath("/tmp/a", 32); got != "/tmp/a" {
		t.Fatalf("short path changed to %q", got)
	}
}

func TestModelLabelFallsBackToDefaultTool(t *testing.T) {
	p := projects.Project{Tool: "", Dir: t.TempDir()}
	if got := modelLabel(p, t.TempDir()); got != "" {
		t.Fatalf("modelLabel = %q, want empty", got)
	}
}
