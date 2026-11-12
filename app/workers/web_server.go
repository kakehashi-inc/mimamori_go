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
// screens (session authentication; one route table for administrators and
// one for everybody else) and the embedded SPA.
func (c *core) webHandler() http.Handler {
	c.assets = c.loadWebAssets()
	adminAPI := http.NewServeMux()
	viewerAPI := http.NewServeMux()
	c.registerAPI(adminAPI, viewerAPI, "/api/v1", false)
	c.webAdminRoutes, c.webViewerRoutes = adminAPI, viewerAPI // for admitWebBody
	mux := http.NewServeMux()
	mux.Handle("/api/", c.sessionAuth(adminAPI, viewerAPI))
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
// client-side routes. The build manifest (.vite/) is never served, and the
// files only the administrator entry uses are served only to a request with
// a valid administrator session (never cached); every other request for
// them gets exactly what an unknown path gets.
func (c *core) serveSPA(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	p := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if p == "" {
		p = "index.html"
	}
	restricted := false
	if p == ".vite" || strings.HasPrefix(p, ".vite/") {
		p = "index.html"
	} else if c.assets != nil && c.assets.restricted[p] {
		if pr, _ := c.sessionPrincipal(r); pr.isAdmin() {
			restricted = true
		} else {
			p = "index.html"
		}
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
	switch {
	case restricted:
		w.Header().Set("Cache-Control", "private, no-store")
	case p == "index.html":
		w.Header().Set("Cache-Control", "no-cache")
	}
	http.ServeContent(w, r, p, time.Time{}, strings.NewReader(string(data)))
}
