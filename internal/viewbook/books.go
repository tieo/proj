package viewbook

import (
	"fmt"
	"html"
	"net/http"
	"sort"
	"strings"
)

// Book is one project's model, and where it is mounted.
type Book struct {
	Name   string // the project's name, and its path segment
	Title  string
	Server *Server
}

// Serve mounts every book under its own path, with a list of them at the root.
// One host, one address per project, so a link to a view stays a link to that
// view rather than to whatever the server happened to be showing.
func Serve(books []Book) http.Handler {
	mux := http.NewServeMux()
	sort.Slice(books, func(i, j int) bool { return books[i].Name < books[j].Name })
	for _, book := range books {
		prefix := "/" + book.Name + "/"
		mux.Handle(prefix, book.Server.Handler(prefix))
		mux.Handle(strings.TrimSuffix(prefix, "/"), book.Server.Handler(prefix))
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, index(books))
	})
	return mux
}

// index is the list of books, written plainly rather than through the
// interface: it exists to get out of the way.
func index(books []Book) string {
	var rows strings.Builder
	for _, book := range books {
		rows.WriteString(fmt.Sprintf(
			`<li><a href="/%s/"><strong>%s</strong><span>%s</span></a></li>`,
			html.EscapeString(book.Name), html.EscapeString(book.Title),
			html.EscapeString(book.Server.config().Subtitle)))
	}
	if len(books) == 0 {
		rows.WriteString(`<li class="none">No project here has a model yet.</li>`)
	}
	return `<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Viewbook</title><style>
:root{color-scheme:light dark;--bg:#fbfbf9;--panel:#fff;--ink:#14181a;--quiet:#6b7478;--line:#e4e6e3;--accent:#2f9e44}
@media(prefers-color-scheme:dark){:root{--bg:#14171a;--panel:#1b1f22;--ink:#e9ecef;--quiet:#99a2a8;--line:#2b3136}}
body{margin:0;background:var(--bg);color:var(--ink);font-family:-apple-system,"Segoe UI",system-ui,sans-serif;
display:flex;min-height:100vh;align-items:center;justify-content:center}
main{width:min(560px,90vw);padding:40px 0}
h1{font-size:26px;margin:0 0 4px}p.sub{margin:0 0 26px;color:var(--quiet);font-size:14px}
ul{list-style:none;margin:0;padding:0;display:flex;flex-direction:column;gap:10px}
a{display:flex;flex-direction:column;gap:2px;padding:14px 16px;border:1px solid var(--line);
border-radius:11px;background:var(--panel);color:inherit;text-decoration:none}
a:hover{border-color:var(--accent)}
a span{font-size:13px;color:var(--quiet)}
.none{color:var(--quiet);font-size:14px}
</style></head><body><main>
<h1>Viewbook</h1><p class="sub">A model per project: its views, what each has to do, and how each renders today.</p>
<ul>` + rows.String() + `</ul></main></body></html>`
}
