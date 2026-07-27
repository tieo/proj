// Package viewbook serves a project's model: every view of its app, what each
// has to do, the states it can be in, and how each renders today.
//
// The model is plain files in the project's own working tree, so the agent
// working in that project edits exactly what the browser shows, and the browser
// is told the moment a file changes underneath it. Anything typed in the browser
// goes to the session as a message rather than into a channel of its own: the
// conversation has one home, and this is not it.
package viewbook

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

//go:embed all:web/dist
var site embed.FS

// name is what a sketch may be called, so a request cannot reach outside the
// project's own wireframes directory.
var name = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// Server serves one project's model.
type Server struct {
	// Root holds viewbook.json, model.json, img/ and wireframes/.
	Root string
	// Say delivers a message to whoever is working on this project. Nothing here
	// knows how: a tmux session, a log, a webhook are all the same to it.
	Say func(message string) error

	watchers sync.Map // chan struct{} per subscriber
	prefix   string
}

// Config is what a project says about its own book: what it is called, and
// which tables it carries. Everything app-specific lives here rather than in
// this package.
type Config struct {
	Title    string  `json:"title"`
	Subtitle string  `json:"subtitle"`
	Tables   []Table `json:"tables"`
}

// Table is a list a project already has as JSON, shown as a table. The tool
// never learns what the rows mean.
type Table struct {
	Name      string   `json:"name"`
	Title     string   `json:"title"`
	Source    string   `json:"source"`
	Rows      string   `json:"rows,omitempty"`
	SortBy    string   `json:"sortBy,omitempty"`
	Statement string   `json:"statement,omitempty"`
	Columns   []Column `json:"columns"`
}

type Column struct {
	Field string `json:"field"`
	Title string `json:"title"`
	Yes   string `json:"yes,omitempty"`
	No    string `json:"no,omitempty"`
	Empty string `json:"empty,omitempty"`
}

// Handler is the whole site: the interface, the project's files, and the
// stream that tells a browser one of them changed. Mounted under prefix, which
// is "/" when this book is alone and "/name/" when it is one of several.
func (s *Server) Handler(prefix string) http.Handler {
	if prefix == "" {
		prefix = "/"
	}
	mux := http.NewServeMux()
	mux.HandleFunc(prefix+"api/config", s.getConfig)
	mux.HandleFunc(prefix+"api/model", s.model)
	mux.HandleFunc(prefix+"api/table/", s.table)
	mux.HandleFunc(prefix+"api/sketch/", s.sketch)
	mux.HandleFunc(prefix+"api/events", s.events)
	mux.HandleFunc(prefix+"img/", s.image)
	s.prefix = prefix

	built, err := fs.Sub(site, "web/dist")
	if err != nil {
		panic(fmt.Sprintf("viewbook: built interface missing: %v", err))
	}
	files := http.StripPrefix(strings.TrimSuffix(prefix, "/"), http.FileServer(http.FS(built)))
	mux.HandleFunc(prefix, func(w http.ResponseWriter, r *http.Request) {
		// A book mounted at /name must be reached as /name/, or every relative
		// request under it resolves one level too high.
		if prefix != "/" && r.URL.Path == strings.TrimSuffix(prefix, "/") {
			http.Redirect(w, r, prefix, http.StatusMovedPermanently)
			return
		}
		// Anything that is not a file is the interface itself, so reloading
		// /name/#/view/results works rather than 404ing.
		within := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, strings.TrimSuffix(prefix, "/")), "/")
		if _, err := fs.Stat(built, within); err != nil || within == "" {
			r = r.Clone(r.Context())
			r.URL.Path = prefix
		}
		files.ServeHTTP(w, r)
	})
	return mux
}

func (s *Server) path(parts ...string) string {
	return filepath.Join(append([]string{s.Root}, parts...)...)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func (s *Server) serveFile(w http.ResponseWriter, path, contentType string) {
	body, err := os.ReadFile(path)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such file"})
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(body)
}

// Config reads the project's own description of its book, with defaults so a
// project that declares nothing still works.
func (s *Server) config() Config {
	cfg := Config{}
	if body, err := os.ReadFile(s.path("viewbook.json")); err == nil {
		_ = json.Unmarshal(body, &cfg)
	}
	if cfg.Title == "" {
		cfg.Title = filepath.Base(s.Root)
	}
	if cfg.Subtitle == "" {
		cfg.Subtitle = "the model"
	}
	if cfg.Tables == nil {
		cfg.Tables = []Table{}
	}
	return cfg
}

func (s *Server) getConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.config())
}

func (s *Server) model(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.serveFile(w, s.path("model.json"), "application/json")
	case http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unreadable"})
			return
		}
		var incoming map[string]any
		if err := json.Unmarshal(body, &incoming); err != nil || incoming["views"] == nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "not a model"})
			return
		}
		before := readModel(s.path("model.json"))
		if err := writeWhole(s.path("model.json"), body); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		s.tell(changes(before, readModel(s.path("model.json"))))
		s.changed()
		writeJSON(w, http.StatusOK, map[string]bool{"saved": true})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) table(w http.ResponseWriter, r *http.Request) {
	wanted := strings.TrimPrefix(r.URL.Path, s.prefix+"api/table/")
	for _, table := range s.config().Tables {
		if table.Name == wanted {
			s.serveFile(w, s.path(table.Source), "application/json")
			return
		}
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such table"})
}

func (s *Server) sketch(w http.ResponseWriter, r *http.Request) {
	sketch := strings.TrimPrefix(r.URL.Path, s.prefix+"api/sketch/")
	if !name.MatchString(sketch) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad name"})
		return
	}
	path := s.path("wireframes", sketch+".excalidraw")
	switch r.Method {
	case http.MethodGet:
		if _, err := os.Stat(path); err != nil {
			writeJSON(w, http.StatusOK, map[string]any{
				"type": "excalidraw", "version": 2, "elements": []any{}, "files": map[string]any{},
			})
			return
		}
		s.serveFile(w, path, "application/json")
	case http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unreadable"})
			return
		}
		var scene map[string]any
		if err := json.Unmarshal(body, &scene); err != nil || scene["type"] != "excalidraw" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "not a scene"})
			return
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if err := writeWhole(path, body); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"saved": true})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) image(w http.ResponseWriter, r *http.Request) {
	wanted := filepath.Clean(strings.TrimPrefix(r.URL.Path, s.prefix+"img/"))
	path := s.path("img", wanted)
	if !strings.HasPrefix(path, s.path("img")+string(os.PathSeparator)) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "outside"})
		return
	}
	s.serveFile(w, path, "image/png")
}

// tell hands each change to whoever is working on this project.
func (s *Server) tell(said []string) {
	if s.Say == nil || len(said) == 0 {
		return
	}
	message := "From the viewbook:\n" + strings.Join(said, "\n")
	if err := s.Say(message); err != nil {
		fmt.Fprintf(os.Stderr, "viewbook: could not pass on the change: %v\n", err)
	}
}

// writeWhole replaces a file in one move, so a reader never sees half of one.
func writeWhole(path string, body []byte) error {
	pretty := body
	var shaped any
	if err := json.Unmarshal(body, &shaped); err == nil {
		if indented, err := json.MarshalIndent(shaped, "", "  "); err == nil {
			pretty = append(indented, '\n')
		}
	}
	temporary := path + ".writing"
	if err := os.WriteFile(temporary, pretty, 0o644); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func readModel(path string) map[string]any {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var model map[string]any
	if err := json.Unmarshal(body, &model); err != nil {
		return nil
	}
	return model
}

// Watch tells every open browser when a file under the project changes, so a
// page shows what the agent just wrote without anyone reloading it.
func (s *Server) Watch(stop <-chan struct{}) {
	var last map[string]time.Time
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			now := s.stamps()
			if last != nil && !sameStamps(last, now) {
				s.changed()
			}
			last = now
		}
	}
}

func (s *Server) stamps() map[string]time.Time {
	stamps := map[string]time.Time{}
	for _, where := range []string{"model.json", "img", "wireframes"} {
		_ = filepath.WalkDir(s.path(where), func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return nil
			}
			if info, err := entry.Info(); err == nil {
				stamps[path] = info.ModTime()
			}
			return nil
		})
	}
	return stamps
}

func sameStamps(a, b map[string]time.Time) bool {
	if len(a) != len(b) {
		return false
	}
	for path, when := range a {
		if other, ok := b[path]; !ok || !other.Equal(when) {
			return false
		}
	}
	return true
}

func (s *Server) changed() {
	s.watchers.Range(func(key, _ any) bool {
		select {
		case key.(chan struct{}) <- struct{}{}:
		default: // a browser already has one waiting; one nudge is enough
		}
		return true
	})
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no streaming here", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	mine := make(chan struct{}, 1)
	s.watchers.Store(mine, true)
	defer s.watchers.Delete(mine)

	fmt.Fprint(w, "event: ready\ndata: {}\n\n")
	flusher.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-mine:
			fmt.Fprint(w, "event: changed\ndata: {}\n\n")
			flusher.Flush()
		case <-time.After(25 * time.Second):
			fmt.Fprint(w, ": still here\n\n")
			flusher.Flush()
		}
	}
}
