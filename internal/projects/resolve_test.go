package projects

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolve(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // isolate the tag registry
	base := t.TempDir()
	for _, n := range []string{"api-web", "api-svc", "konf_header", "proj"} {
		if err := os.Mkdir(filepath.Join(base, n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A non-directory entry that Resolve must ignore.
	if err := os.WriteFile(filepath.Join(base, "notes.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	mustResolve := func(query, want string) {
		t.Helper()
		p, err := Resolve(base, query)
		if err != nil {
			t.Errorf("Resolve(%q) error: %v", query, err)
		} else if p.Name != want {
			t.Errorf("Resolve(%q) = %q, want %q", query, p.Name, want)
		}
	}
	mustResolve("proj", "proj")       // exact name
	mustResolve("kon", "konf_header") // unique prefix

	// Ambiguous prefix, no match, and empty query all error.
	for _, query := range []string{"api", "zzz", ""} {
		if _, err := Resolve(base, query); err == nil {
			t.Errorf("Resolve(%q) = nil error; want an error", query)
		}
	}
}
