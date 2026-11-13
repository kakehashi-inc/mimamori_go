package workers

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
)

// testManifest is a small Vite manifest: the main entry and the
// administrator entry share one chunk; the administrator entry has its own
// CSS, an asset and a dynamically imported chunk.
const testManifest = `{
  "_shared.js": {"file": "assets/shared.js"},
  "index.html": {"file": "assets/main.js", "src": "index.html", "isEntry": true, "imports": ["_shared.js"], "css": ["assets/main.css"]},
  "src/admin/entry.tsx": {"file": "assets/admin.js", "src": "src/admin/entry.tsx", "isEntry": true,
    "imports": ["_shared.js"], "dynamicImports": ["_lazy.js"], "css": ["assets/admin.css"], "assets": ["assets/admin.png"]},
  "_lazy.js": {"file": "assets/lazy.js", "imports": ["_shared.js"]}
}`

func testSPA(withManifest bool) fstest.MapFS {
	fsys := fstest.MapFS{
		"index.html":       {Data: []byte("<!doctype html><title>shell</title>")},
		"assets/main.js":   {Data: []byte("main()")},
		"assets/main.css":  {Data: []byte("body{}")},
		"assets/shared.js": {Data: []byte("shared()")},
		"assets/admin.js":  {Data: []byte("admin()")},
		"assets/admin.css": {Data: []byte(".admin{}")},
		"assets/admin.png": {Data: []byte("png")},
		"assets/lazy.js":   {Data: []byte("lazy()")},
	}
	if withManifest {
		fsys[".vite/manifest.json"] = &fstest.MapFile{Data: []byte(testManifest)}
	}
	return fsys
}

// browserHdr builds request headers for a browser session (ck may be "").
func browserHdr(ck string, csrf bool) map[string]string {
	h := map[string]string{}
	if ck != "" {
		h["Cookie"] = sessionCookie + "=" + ck
	}
	if csrf {
		h[csrfHeader] = "1"
	}
	return h
}

// newViewer creates a viewer account and returns its session cookie value.
func newViewer(t *testing.T, e *testEnv) string {
	t.Helper()
	e.admin("POST", "/accounts", map[string]string{"email": "view@example.com", "display_name": "Viewer", "role": "viewer", "password": "Viewer-pass-1"})
	return login(t, e, "view@example.com", "Viewer-pass-1")
}

// sameResponse fails unless two responses have the same status, headers
// and body.
func sameResponse(t *testing.T, what string, got, want *httptest.ResponseRecorder) {
	t.Helper()
	if got.Code != want.Code || !reflect.DeepEqual(got.Header(), want.Header()) || !bytes.Equal(got.Body.Bytes(), want.Body.Bytes()) {
		t.Errorf("%s: got %d %v %q, want %d %v %q", what, got.Code, got.Header(), got.Body.String(), want.Code, want.Header(), want.Body.String())
	}
}

func TestWebAssetsRestrictedSet(t *testing.T) {
	e := newTestEnv(t)
	e.c.spaFS = testSPA(true)
	wa := e.c.loadWebAssets()
	want := map[string]bool{"assets/admin.js": true, "assets/admin.css": true, "assets/admin.png": true, "assets/lazy.js": true}
	if !reflect.DeepEqual(wa.restricted, want) {
		t.Fatalf("restricted %v, want %v", wa.restricted, want)
	}
	if !reflect.DeepEqual(wa.adminModules, []string{"/assets/admin.js"}) {
		t.Fatalf("modules %v", wa.adminModules)
	}
}

func TestStaticAdminFiles(t *testing.T) {
	e := newTestEnv(t)
	e.c.spaFS = testSPA(true)
	e.web = e.c.webHandler()
	adminCk := login(t, e, testAdminEmail, "Admin-pass-123")
	viewCk := newViewer(t, e)

	get := func(method, p, ck string) *httptest.ResponseRecorder {
		return e.do(e.web, method, p, nil, browserHdr(ck, false), "x:1")
	}
	hidden := []string{"/assets/admin.js", "/assets/admin.css", "/assets/admin.png", "/assets/lazy.js", "/.vite/manifest.json", "/.vite/", "/.vite"}
	for _, ck := range []string{"", viewCk, "mms_invalid"} {
		for _, method := range []string{"GET", "HEAD", "POST"} {
			unknown := get(method, "/assets/unknown.js", ck)
			for _, p := range hidden {
				sameResponse(t, method+" "+p+" as "+ck, get(method, p, ck), unknown)
			}
		}
	}
	// The manifest is hidden from administrators too.
	sameResponse(t, "manifest as admin", get("GET", "/.vite/manifest.json", adminCk), get("GET", "/assets/unknown.js", adminCk))

	// Administrators get the files, never cached.
	for _, p := range []string{"/assets/admin.js", "/assets/lazy.js", "/assets/admin.css"} {
		rec := get("GET", p, adminCk)
		if rec.Code != 200 || rec.Header().Get("Cache-Control") != "private, no-store" || strings.Contains(rec.Body.String(), "shell") {
			t.Fatalf("admin %s: %d %q %q", p, rec.Code, rec.Header().Get("Cache-Control"), rec.Body.String())
		}
	}
	if rec := get("GET", "/assets/admin.js", adminCk); rec.Body.String() != "admin()" || !strings.Contains(rec.Header().Get("Content-Type"), "javascript") {
		t.Fatalf("admin.js: %q %q", rec.Header().Get("Content-Type"), rec.Body.String())
	}
	// The main entry and the shared chunk stay public.
	for _, p := range []string{"/assets/main.js", "/assets/shared.js", "/assets/main.css"} {
		rec := get("GET", p, "")
		if rec.Code != 200 || strings.Contains(rec.Body.String(), "shell") || rec.Header().Get("Cache-Control") != "" {
			t.Fatalf("public %s: %d %q", p, rec.Code, rec.Body.String())
		}
	}

	// Serving a file does not extend the session.
	if err := e.exec(`UPDATE sessions SET last_seen_at = 1000`); err != nil {
		t.Fatal(err)
	}
	get("GET", "/assets/admin.js", adminCk)
	get("GET", "/assets/admin.js", viewCk)
	var touched int
	_ = e.c.db.Read().QueryRow(`SELECT COUNT(*) FROM sessions WHERE last_seen_at <> 1000`).Scan(&touched)
	if touched != 0 {
		t.Fatalf("%d sessions extended by static files", touched)
	}
}

// TestStaticAdminFilesInvalidSessionsAndPaths checks that an expired
// session and a session of a disabled administrator do not get the
// administrator files, and that other spellings of their paths do not reach
// them either.
func TestStaticAdminFilesInvalidSessionsAndPaths(t *testing.T) {
	e := newTestEnv(t)
	e.c.spaFS = testSPA(true)
	e.web = e.c.webHandler()
	get := func(p, ck string) *httptest.ResponseRecorder {
		return e.do(e.web, "GET", p, nil, browserHdr(ck, false), "x:1")
	}

	expiredCk := login(t, e, testAdminEmail, "Admin-pass-123")
	if err := e.exec(`UPDATE sessions SET expires_at = 1`); err != nil {
		t.Fatal(err)
	}
	e.admin("POST", "/accounts", map[string]string{"email": "admin2@example.com", "display_name": "Admin 2", "role": "admin", "password": "Admin2-pass-12"})
	disabledCk := login(t, e, "admin2@example.com", "Admin2-pass-12")
	if err := e.exec(`UPDATE accounts SET disabled = 1 WHERE email = 'admin2@example.com'`); err != nil {
		t.Fatal(err)
	}
	for name, ck := range map[string]string{"expired": expiredCk, "disabled": disabledCk} {
		sameResponse(t, name+" admin.js", get("/assets/admin.js", ck), get("/assets/unknown.js", ck))
	}

	viewCk := newViewer(t, e)
	unknown := get("/assets/unknown.js", viewCk)
	for _, p := range []string{"/assets/admin.js/", "/assets/admin.js?v=1", "/assets%2Fadmin.js", "/ASSETS/admin.js", "/assets/ADMIN.JS", "/./assets/admin.js", "/assets//admin.js", "/x/../assets/admin.js"} {
		rec := get(p, viewCk)
		if rec.Body.String() == "admin()" {
			t.Fatalf("viewer %s: got the administrator file", p)
		}
		// ServeMux redirects unclean paths (the same for every path); the
		// rest must answer like an unknown file.
		if rec.Code < 300 || rec.Code >= 400 {
			sameResponse(t, "viewer "+p, rec, unknown)
		}
	}
}

func TestUIModules(t *testing.T) {
	e := newTestEnv(t)
	e.c.spaFS = testSPA(true)
	e.web = e.c.webHandler()
	adminCk := login(t, e, testAdminEmail, "Admin-pass-123")
	viewCk := newViewer(t, e)
	if rec := e.do(e.web, "GET", "/api/v1/ui/modules", nil, nil, "x:1"); rec.Code != 401 {
		t.Fatalf("anonymous: %d", rec.Code)
	}
	if got := decode[[]string](t, e.do(e.web, "GET", "/api/v1/ui/modules", nil, browserHdr(viewCk, false), "x:1")); got == nil || len(got) != 0 {
		t.Fatalf("viewer modules %v", got)
	}
	if got := decode[[]string](t, e.do(e.web, "GET", "/api/v1/ui/modules", nil, browserHdr(adminCk, false), "x:1")); !reflect.DeepEqual(got, []string{"/assets/admin.js"}) {
		t.Fatalf("admin modules %v", got)
	}
}

func TestWebAssetsWithoutManifest(t *testing.T) {
	e := newTestEnv(t)
	var logs bytes.Buffer
	e.c.logger = log.New(&logs, "", 0)
	e.c.spaFS = testSPA(false)
	e.web = e.c.webHandler()
	if n := strings.Count(logs.String(), "\n"); n != 1 || !strings.Contains(logs.String(), "manifest not found") {
		t.Fatalf("log %q", logs.String())
	}
	adminCk := login(t, e, testAdminEmail, "Admin-pass-123")
	if rec := e.do(e.web, "GET", "/assets/admin.js", nil, nil, "x:1"); rec.Body.String() != "admin()" {
		t.Fatalf("unrestricted file: %q", rec.Body.String())
	}
	rec := e.do(e.web, "GET", "/api/v1/ui/modules", nil, browserHdr(adminCk, false), "x:1")
	if strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("modules without manifest: %q", rec.Body.String())
	}
}

// TestViewerAdminRoutesDoNotExist checks that a viewer cannot tell an
// administrator route from a route that does not exist, by any method.
func TestViewerAdminRoutesDoNotExist(t *testing.T) {
	e := newTestEnv(t)
	adminCk := login(t, e, testAdminEmail, "Admin-pass-123")
	viewCk := newViewer(t, e)
	call := func(method, p, ck string) *httptest.ResponseRecorder {
		var body any
		if method != "GET" && method != "HEAD" {
			body = map[string]any{}
		}
		return e.do(e.web, method, p, body, browserHdr(ck, true), "x:1")
	}
	// Each administrator path with a path of the same shape that a viewer
	// has with the same routes (none, or GET only).
	twins := map[string]string{
		"/api/v1/accounts":                  "/api/v1/nothing",
		"/api/v1/accounts/2":                "/api/v1/nothing/2",
		"/api/v1/accounts/2/email":          "/api/v1/nothing/2/email",
		"/api/v1/accounts/2/display-name":   "/api/v1/nothing/2/display-name",
		"/api/v1/accounts/2/role":           "/api/v1/nothing/2/role",
		"/api/v1/accounts/2/notify":         "/api/v1/nothing/2/notify",
		"/api/v1/accounts/2/password":       "/api/v1/nothing/2/password",
		"/api/v1/accounts/2/disable":        "/api/v1/nothing/2/disable",
		"/api/v1/accounts/2/enable":         "/api/v1/nothing/2/enable",
		"/api/v1/config":                    "/api/v1/nothing",
		"/api/v1/detections/ack":            "/api/v1/nothing/ack",
		"/api/v1/tokens/1/reissue":          "/api/v1/nothing/1/reissue",
		"/api/v1/tokens/1/unbind":           "/api/v1/nothing/1/unbind",
		"/api/v1/tokens/1/revoke":           "/api/v1/nothing/1/revoke",
		"/api/v1/rules/1/apply":             "/api/v1/nothing/1/apply",
		"/api/v1/rules/1/to-template":       "/api/v1/nothing/1/to-template",
		"/api/v1/suppressions/1":            "/api/v1/nothing/1",
		"/api/v1/tokens":                    "/api/v1/agents",
		"/api/v1/rules":                     "/api/v1/agents",
		"/api/v1/templates":                 "/api/v1/agents",
		"/api/v1/suppressions":              "/api/v1/agents",
		"/api/v1/rules/1":                   "/api/v1/agents/1",
		"/api/v1/rules/merge-preview":       "/api/v1/agents/1",
		"/api/v1/rules/validate":            "/api/v1/agents/1",
		"/api/v1/templates/base":            "/api/v1/agents/1",
		"/api/v1/accounts/2/role?x=1":       "/api/v1/nothing/2/role?x=1",
		"/api/v1/config/":                   "/api/v1/nothing/",
		"/api/v1/suppressions/1/extra/path": "/api/v1/nothing/1/extra/path",
		// Command set definitions and run requests.
		"/api/v1/agents/1/command-sets":          "/api/v1/nothing/1/command-sets",
		"/api/v1/agents/1/command-sets/1":        "/api/v1/nothing/1/command-sets/1",
		"/api/v1/agents/1/command-sets/validate": "/api/v1/nothing/1/command-sets/validate",
		"/api/v1/agents/1/command-sets/import":   "/api/v1/nothing/1/command-sets/import",
		"/api/v1/agents/1/command-sets/1/run":    "/api/v1/nothing/1/command-sets/1/run",
		"/api/v1/agents/1/command-requests/1":    "/api/v1/nothing/1/command-requests/1",
		// Re-authentication for command set changes.
		"/api/v1/auth/step-up":        "/api/v1/nothing/step-up",
		"/api/v1/auth/step-up/verify": "/api/v1/nothing/step-up/verify",
	}
	methods := []string{"GET", "HEAD", "POST", "PUT", "DELETE", "PATCH", "OPTIONS"}
	for p, twin := range twins {
		for _, m := range methods {
			if strings.HasSuffix(twin, "/agents") || strings.HasSuffix(twin, "/agents/1") {
				if m == "GET" || m == "HEAD" {
					continue // a route the viewer has
				}
			}
			sameResponse(t, "viewer "+m+" "+p, call(m, p, viewCk), call(m, twin, viewCk))
		}
	}
	// Unauthenticated requests keep their 401.
	if rec := call("GET", "/api/v1/accounts", ""); rec.Code != 401 {
		t.Fatalf("anonymous accounts: %d", rec.Code)
	}
	// Administrators still have every route.
	for _, r := range [][2]string{{"GET", "/api/v1/accounts"}, {"GET", "/api/v1/config"}, {"PUT", "/api/v1/accounts/2/role"}, {"PUT", "/api/v1/accounts/2/notify"}, {"POST", "/api/v1/rules/validate"},
		{"GET", "/api/v1/agents/1/command-sets"}, {"POST", "/api/v1/agents/1/command-sets/validate"}, {"POST", "/api/v1/agents/1/command-sets/import"},
		{"POST", "/api/v1/auth/step-up"}} {
		if rec := call(r[0], r[1], adminCk); rec.Code == 404 || rec.Code == 405 {
			t.Fatalf("admin %s %s: %d", r[0], r[1], rec.Code)
		}
	}
	// requireAdmin, the second line, answers like a missing route.
	rec := httptest.NewRecorder()
	requireAdmin(func(w http.ResponseWriter, r *http.Request) { t.Fatal("reached") })(rec, httptest.NewRequest("GET", "/x", nil))
	want := httptest.NewRecorder()
	http.NotFound(want, httptest.NewRequest("GET", "/x", nil))
	sameResponse(t, "requireAdmin", rec, want)
	// The management API is not affected.
	if got := e.admin("GET", "/accounts", nil); got == nil {
		t.Fatal("management API accounts")
	}
}
