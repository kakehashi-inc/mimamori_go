package workers

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Every error answer a viewer or a logged-out browser can receive carries
// codes of the bundle every account loads: an administrator code would be
// shown to them in English only.
func TestViewerErrorAnswersUseEveryoneCodes(t *testing.T) {
	e := newTestEnv(t)
	e.admin("POST", "/rules", map[string]any{"name": "r1"})
	e.admin("POST", "/accounts", map[string]any{"email": "admin2@example.com", "display_name": "Second", "role": "admin", "password": "Admin2-pass-123"})
	viewer := newViewer(t, e)
	admin := adminOnlyCodes(t)
	type req struct {
		method, path string
		body         any
		session      bool
	}
	cases := []req{
		{"GET", "/overview", nil, false},
		{"POST", "/auth/login", map[string]any{"email": "view@example.com", "password": "wrong"}, false},
		{"POST", "/auth/login", map[string]any{"x": 1}, false},
		{"POST", "/auth/login/verify", map[string]any{"challenge": "x", "code": "1"}, false},
		{"GET", "/agents/999", nil, true},
		{"GET", "/rules/999", nil, true},
		{"GET", "/rules/1/versions/9", nil, true},
		{"GET", "/rules/1/diff?from=x&to=1", nil, true},
		{"GET", "/templates/nosuch", nil, true},
		{"GET", "/tokens/999", nil, true},
		{"GET", "/detections/1/999", nil, true},
		{"GET", "/detections?from=garbage", nil, true},
		{"GET", "/detections?acknowledged=maybe", nil, true},
		{"GET", "/detections?agent_id=x", nil, true},
		{"GET", "/history?limit=x", nil, true},
		{"GET", "/agents/1/command-runs/999", nil, true},
		{"GET", "/agents/1/command-sets/999/runs", nil, true},
		{"POST", "/auth/password", map[string]any{"current_password": "x", "new_password": "Another-pass-123"}, true},
		{"POST", "/auth/password", map[string]any{"current_password": "Viewer-pass-1", "new_password": "short"}, true},
		{"PUT", "/auth/language", map[string]any{"language": "fr"}, true},
		{"PUT", "/auth/display-name", map[string]any{"display_name": strings.Repeat("x", 70)}, true},
		{"POST", "/auth/email", map[string]any{"email": "a b@example.com"}, true},
		{"POST", "/auth/email", map[string]any{"email": "view@example.com"}, true},
		{"POST", "/auth/email", map[string]any{"email": "admin2@example.com"}, true},
		{"POST", "/auth/email/verify", map[string]any{"code": "1"}, true},
		{"PUT", "/auth/language", "not an object", true},
	}
	for _, c := range cases {
		hdr := map[string]string{csrfHeader: "1"}
		if c.session {
			hdr["Cookie"] = sessionCookie + "=" + viewer
		}
		rec := e.do(e.web, c.method, "/api/v1"+c.path, c.body, hdr, "192.0.2.1:1")
		name := c.method + " " + c.path
		if rec.Code < http.StatusBadRequest {
			t.Errorf("%s: status %d, want an error", name, rec.Code)
			continue
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["error_i18n"] == nil {
			t.Errorf("%s: %d %s: no error_i18n", name, rec.Code, rec.Body.String())
			continue
		}
		for _, code := range codesIn(body, nil) {
			if admin[code] {
				t.Errorf("%s: a viewer receives the administrator code %s", name, code)
			}
		}
	}
}
