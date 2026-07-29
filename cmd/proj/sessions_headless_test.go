package main

import (
	"os"
	"path/filepath"
	"testing"
)

// headlessHome writes transcripts under a fake Claude home and returns it, so a
// test can resolve sessions the way the headless commands do.
func headlessHome(t *testing.T, ids ...string) string {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, "projects", "-x")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		body := `{"type":"user","cwd":"/x","sessionId":"` + id + `","message":{"role":"user","content":"hi"}}` + "\n"
		if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

func TestResolveSessionPrefix(t *testing.T) {
	home := headlessHome(t, "abc12345-1111", "abd99999-2222")

	s, err := resolveSession(home, "abc")
	if err != nil {
		t.Fatalf("unique prefix: %v", err)
	}
	if s.ID != "abc12345-1111" {
		t.Errorf("resolved %q, want abc12345-1111", s.ID)
	}

	if s, err := resolveSession(home, "abd99999-2222"); err != nil || s.ID != "abd99999-2222" {
		t.Errorf("full id: got %q, %v", s.ID, err)
	}

	if _, err := resolveSession(home, "ab"); err == nil {
		t.Error("ambiguous prefix should not resolve")
	}
	if _, err := resolveSession(home, "zz"); err == nil {
		t.Error("unknown prefix should not resolve")
	}
}

func TestForkBounds(t *testing.T) {
	cases := []struct {
		n, from, to    int
		wantFrom, want int
		wantErr        bool
	}{
		{n: 5, from: 0, to: 0, wantFrom: 1, want: 5}, // both defaulted
		{n: 5, from: 3, to: 0, wantFrom: 3, want: 5}, // --from alone runs to the end
		{n: 5, from: 0, to: 2, wantFrom: 1, want: 2}, // --to alone starts at the top
		{n: 5, from: 2, to: 4, wantFrom: 2, want: 4}, // explicit range
		{n: 5, from: 4, to: 2, wantErr: true},        // inverted
		{n: 5, from: 0, to: 9, wantErr: true},        // past the end
		{n: 5, from: -1, to: 3, wantErr: true},       // before the start
	}
	for _, c := range cases {
		from, to, err := forkBounds(c.n, c.from, c.to)
		if c.wantErr {
			if err == nil {
				t.Errorf("forkBounds(%d,%d,%d) = %d–%d, want error", c.n, c.from, c.to, from, to)
			}
			continue
		}
		if err != nil {
			t.Errorf("forkBounds(%d,%d,%d): %v", c.n, c.from, c.to, err)
			continue
		}
		if from != c.wantFrom || to != c.want {
			t.Errorf("forkBounds(%d,%d,%d) = %d–%d, want %d–%d", c.n, c.from, c.to, from, to, c.wantFrom, c.want)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	for _, c := range []struct {
		n    int
		want string
	}{{512, "512B"}, {2048, "2K"}, {3 << 20, "3.0M"}} {
		if got := humanBytes(c.n); got != c.want {
			t.Errorf("humanBytes(%d) = %s, want %s", c.n, got, c.want)
		}
	}
}
