package projects

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// Registry stores per-project metadata (tags, ...) in a single TOML file
// outside any project directory, so projects don't carry proj-specific
// files in their checkout.
type Registry struct {
	Projects map[string]ProjectMeta `toml:"projects"`
}

type ProjectMeta struct {
	Tags []string `toml:"tags"`
}

// RegistryPath returns the location of the registry file.
//   $XDG_CONFIG_HOME/proj/projects.toml, or ~/.config/proj/projects.toml.
func RegistryPath() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "proj", "projects.toml")
}

// LoadRegistry reads the registry file. A missing file produces an empty
// registry without error.
func LoadRegistry() (Registry, error) {
	r := Registry{Projects: map[string]ProjectMeta{}}
	data, err := os.ReadFile(RegistryPath())
	if err != nil {
		if os.IsNotExist(err) {
			return r, nil
		}
		return r, err
	}
	if err := toml.Unmarshal(data, &r); err != nil {
		return r, fmt.Errorf("parse %s: %w", RegistryPath(), err)
	}
	if r.Projects == nil {
		r.Projects = map[string]ProjectMeta{}
	}
	return r, nil
}

// Save writes the registry atomically (write to tmp, rename). Empty
// registries are still persisted so the file's existence stays consistent.
func (r Registry) Save() error {
	path := RegistryPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(r); err != nil {
		return fmt.Errorf("encode registry: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Tags returns the tags for name, sorted and deduplicated. Returns nil if
// the name has no entry.
func (r Registry) Tags(name string) []string {
	m, ok := r.Projects[name]
	if !ok {
		return nil
	}
	return normalize(m.Tags)
}

// SetTags assigns tags to name, persisting the registry. An empty tag list
// removes the project's entry entirely.
func (r Registry) SetTags(name string, tags []string) error {
	clean := normalize(tags)
	if len(clean) == 0 {
		delete(r.Projects, name)
	} else {
		r.Projects[name] = ProjectMeta{Tags: clean}
	}
	return r.Save()
}

// Delete removes the registry entry for name. No error if it didn't exist.
func (r Registry) Delete(name string) error {
	if _, ok := r.Projects[name]; !ok {
		return nil
	}
	delete(r.Projects, name)
	return r.Save()
}

// Rename moves the entry from old to new. No-op if there was no entry.
func (r Registry) Rename(oldName, newName string) error {
	m, ok := r.Projects[oldName]
	if !ok {
		return nil
	}
	delete(r.Projects, oldName)
	r.Projects[newName] = m
	return r.Save()
}

// AllTags returns the union of tags across all entries, sorted.
func (r Registry) AllTags() []string {
	seen := make(map[string]struct{})
	for _, m := range r.Projects {
		for _, t := range m.Tags {
			t = strings.TrimSpace(t)
			if t != "" {
				seen[t] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}
