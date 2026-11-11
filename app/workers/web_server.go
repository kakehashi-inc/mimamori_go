package workers

import (
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"
)

func init() {
	// Extensions the Go built-in MIME table does not know. The OS table is not
	// always available (e.g. minimal containers), so register them explicitly.
	_ = mime.AddExtensionType(".webmanifest", "application/manifest+json")
	_ = mime.AddExtensionType(".ico", "image/x-icon")
	_ = mime.AddExtensionType(".woff2", "font/woff2")
}

// webHandler builds the browser listener's handler: the API used by the
// screens (session authentication) and the embedded SPA.
func (c *core) webHandler() http.Handler {
	api := http.NewServeMux()
	c.registerAPI(api, "/api/v1", false)
	mux := http.NewServeMux()
	mux.Handle("/api/", c.sessionAuth(api))
	mux.HandleFunc("/", c.serveSPA)
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

// serveSPA serves the embedded SPA, falling back to index.html for unknown
// client-side routes.
func (c *core) serveSPA(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	p := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if p == "" {
		p = "index.html"
	}
	data, err := fs.ReadFile(c.spaFS, p)
	if err != nil {
		p = "index.html"
		data, err = fs.ReadFile(c.spaFS, p)
		if err != nil {
			http.NotFound(w, r)
			return
		}
	}
	ctype := mime.TypeByExtension(path.Ext(p))
	switch path.Ext(p) {
	case ".webmanifest":
		ctype = "application/manifest+json"
	case ".ico":
		ctype = "image/x-icon"
	}
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ctype)
	if p == "index.html" {
		w.Header().Set("Cache-Control", "no-cache")
	}
	http.ServeContent(w, r, p, time.Time{}, strings.NewReader(string(data)))
}
