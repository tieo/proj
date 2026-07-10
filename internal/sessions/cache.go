package sessions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// metaCache memoizes the expensive part of List - the per-transcript metadata
// read - keyed by file path and invalidated by mtime or size change. Transcripts
// live on a slow mount and the large ones are read in full by readMeta, so a
// session list that re-read every file each run took many seconds; only the
// handful of files that actually changed since last run need re-reading.
type cacheEntry struct {
	Mod      int64  `json:"mod"`  // ModTime().Unix() when the meta was read
	Size     int64  `json:"size"` // file size when the meta was read
	Cwd      string `json:"cwd"`
	Title    string `json:"title"`
	Answer   string `json:"answer"`
	Messages int    `json:"messages"`
}

var cacheMu sync.Mutex

// cachePath is the on-disk location of the list metadata cache, under the user
// cache dir (honoring XDG_CACHE_HOME). A missing or unreadable cache is not an
// error: it just means every transcript is read this run and the cache is
// rebuilt.
func cachePath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "proj", "sessions.json")
}

func loadCache() map[string]cacheEntry {
	m := map[string]cacheEntry{}
	p := cachePath()
	if p == "" {
		return m
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return m
	}
	_ = json.Unmarshal(data, &m)
	return m
}

// saveCache writes the cache atomically. Failures are silent: the cache is an
// optimization, and a missed write only costs a slower next run.
func saveCache(m map[string]cacheEntry) {
	p := cachePath()
	if p == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	data, err := json.Marshal(m)
	if err != nil {
		return
	}
	tmp := p + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		_ = os.Rename(tmp, p)
	}
}
