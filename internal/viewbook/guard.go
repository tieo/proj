package viewbook

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// keyCookie is where a browser keeps the key once it has been let in.
const keyCookie = "viewbook"

// Guard stands in front of a viewbook.
//
// A page here types into the session working on a project, so reaching this
// server is reaching the machine. Two things therefore hold for every request.
//
// It is same-origin. A page on any other site can make a browser send a request
// here, and a request that arrives from one carries the same cookies as one the
// user made; without this check, opening the wrong web page is enough to type a
// command into a session.
//
// It carries the key. The key is a file only its owner can read, so a process
// that reaches the port - through the tunnel, from another account, from
// anything that gets as far as the socket - still cannot say anything.
//
// An empty key turns the second check off, which is for a book with no session
// behind it and nothing to say into.
func Guard(key string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			http.Error(w, "cross-site request refused", http.StatusForbidden)
			return
		}
		if key == "" {
			next.ServeHTTP(w, r)
			return
		}
		if given := r.URL.Query().Get("key"); given != "" {
			if subtle.ConstantTimeCompare([]byte(given), []byte(key)) != 1 {
				http.Error(w, "wrong key", http.StatusUnauthorized)
				return
			}
			http.SetCookie(w, &http.Cookie{
				Name:     keyCookie,
				Value:    key,
				Path:     "/",
				HttpOnly: true,
				Secure:   overTLS(r),
				SameSite: http.SameSiteStrictMode,
				MaxAge:   365 * 24 * 60 * 60,
			})
			// The key belongs in the cookie jar, not in the address bar, the
			// history, or the referrer of every link the page carries.
			clean := *r.URL
			query := clean.Query()
			query.Del("key")
			clean.RawQuery = query.Encode()
			http.Redirect(w, r, clean.RequestURI(), http.StatusSeeOther)
			return
		}
		if !carriesKey(r, key) {
			http.Error(w, "viewbook needs its key: open the address it printed on startup", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// sameOrigin is whether the request came from this book rather than from
// another site that happens to be open in the same browser.
//
// What has to be refused is a request another site makes on the browser's
// behalf: a form post, a fetch, an image that is really a command. Following a
// link here is none of those - arriving from a login portal or from a chat
// window is how anyone gets here at all - so a plain navigation is let through
// and judged by the key.
//
// Fetch metadata says where a request came from and every current browser sends
// it; Origin covers a form post, which is the one cross-site request that
// carries no preflight. A request with neither is not from a browser - curl, a
// health check, the screenshot tool - and the key alone decides.
func sameOrigin(r *http.Request) bool {
	reading := r.Method == http.MethodGet || r.Method == http.MethodHead
	if reading && r.Header.Get("Sec-Fetch-Mode") == "navigate" {
		return true
	}
	switch r.Header.Get("Sec-Fetch-Site") {
	case "cross-site", "same-site":
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" && !reading {
		from, err := url.Parse(origin)
		if err != nil || from.Host != r.Host {
			return false
		}
	}
	return true
}

func carriesKey(r *http.Request, key string) bool {
	given := r.Header.Get("X-Viewbook-Key")
	if given == "" {
		if cookie, err := r.Cookie(keyCookie); err == nil {
			given = cookie.Value
		}
	}
	return subtle.ConstantTimeCompare([]byte(given), []byte(key)) == 1
}

func overTLS(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

// KeyAt is the key kept at path, made the first time it is asked for. The file
// is the owner's alone, so who can read it is who can open the book.
func KeyAt(path string) (string, error) {
	if body, err := os.ReadFile(path); err == nil {
		if key := strings.TrimSpace(string(body)); key != "" {
			return key, nil
		}
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	key := hex.EncodeToString(raw)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(key+"\n"), 0o600); err != nil {
		return "", err
	}
	return key, nil
}
