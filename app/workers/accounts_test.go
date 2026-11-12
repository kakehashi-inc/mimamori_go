package workers

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"mimamori/app/models"
	"mimamori/app/modules"
)

// browserHeaders returns the headers of a browser request with a session.
func browserHeaders(ck string) map[string]string {
	return map[string]string{"Cookie": sessionCookie + "=" + ck, csrfHeader: "1"}
}

// adminRequest sends a management API request and returns the status.
func (e *testEnv) adminStatus(method, path string, body any) int {
	e.t.Helper()
	return e.do(e.agent, method, "/admin/v1"+path, body, map[string]string{"Authorization": "Bearer " + e.cliKey}, "127.0.0.1:5000").Code
}

// addAuthState gives an account a browser verification and a pending code,
// as the two-step login leaves them.
func addAuthState(t *testing.T, e *testEnv, id int64, tag string) {
	t.Helper()
	now := time.Now().Unix()
	if err := models.InsertBrowserVerification(e.c.db, &models.BrowserVerification{ID: tag, AccountID: id, CreatedAt: now, ExpiresAt: now + 3600}); err != nil {
		t.Fatal(err)
	}
	if err := models.InsertAuthCode(e.c.db, &models.AuthCode{Purpose: models.CodePurposeLogin, AccountID: id, ChallengeHash: tag, CodeHash: "x", CreatedAt: now, ExpiresAt: now + 600}); err != nil {
		t.Fatal(err)
	}
}

func countRows(t *testing.T, e *testEnv, table string, id int64) int {
	t.Helper()
	var n int
	if err := e.c.db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE account_id = ?`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAccountFields(t *testing.T) {
	e := newTestEnv(t)
	created := e.admin("POST", "/accounts", map[string]any{"email": " Alice@Example.com ", "display_name": " Alice ", "role": "viewer", "password": "Viewer-pass-123"})
	if created["email"] != "Alice@Example.com" || created["display_name"] != "Alice" || created["name"] != nil {
		t.Fatalf("created: %v", created)
	}
	for _, body := range []map[string]any{
		{"email": "alice@example.com", "display_name": "Other", "role": "viewer", "password": "Viewer-pass-123"},
		{"email": "ALICE@EXAMPLE.COM", "display_name": "Other", "role": "viewer", "password": "Viewer-pass-123"},
	} {
		if code := e.adminStatus("POST", "/accounts", body); code != http.StatusConflict {
			t.Fatalf("duplicate %v: %d", body["email"], code)
		}
	}
	for _, body := range []map[string]any{
		{"email": "Alice <bob@example.com>", "display_name": "Bob", "role": "viewer", "password": "Viewer-pass-123"},
		{"email": "bob", "display_name": "Bob", "role": "viewer", "password": "Viewer-pass-123"},
		{"email": "bob@example.com", "display_name": " ", "role": "viewer", "password": "Viewer-pass-123"},
		{"email": "bob@example.com", "display_name": strings.Repeat("x", 65), "role": "viewer", "password": "Viewer-pass-123"},
		{"name": "bob", "role": "viewer", "password": "Viewer-pass-123"},
	} {
		if code := e.adminStatus("POST", "/accounts", body); code != http.StatusBadRequest {
			t.Fatalf("invalid %v: %d", body, code)
		}
	}
	// Display names may repeat.
	e.admin("POST", "/accounts", map[string]any{"email": "bob@localhost", "display_name": "Alice", "role": "viewer", "password": "Viewer-pass-123"})

	// Login by e-mail address without regard to case; Me carries the fields.
	ck := login(t, e, "alice@EXAMPLE.com", "Viewer-pass-123")
	me := decode[map[string]any](t, e.do(e.web, "GET", "/api/v1/auth/me", nil, browserHeaders(ck), "x:1"))
	if me["email"] != "Alice@Example.com" || me["display_name"] != "Alice" || me["role"] != "viewer" || me["name"] != nil {
		t.Fatalf("me: %v", me)
	}
	rec := e.do(e.web, "POST", "/api/v1/auth/login", map[string]string{"email": "nobody@example.com", "password": "Viewer-pass-123"}, map[string]string{csrfHeader: "1"}, "x:1")
	if rec.Code != 401 || !strings.Contains(rec.Body.String(), "invalid e-mail address or password") {
		t.Fatalf("unknown address: %d %s", rec.Code, rec.Body.String())
	}
	list := e.admin("GET", "/accounts", nil)["list"].([]any)
	if first := list[0].(map[string]any); first["email"] != testAdminEmail || first["display_name"] != "Admin" {
		t.Fatalf("list: %v", first)
	}
}

func TestAdministratorChangesEmailAndDisplayName(t *testing.T) {
	e := newTestEnv(t)
	created := e.admin("POST", "/accounts", map[string]any{"email": "carol@example.com", "display_name": "Carol", "role": "viewer", "password": "Viewer-pass-123"})
	id := int64(created["id"].(float64))
	ck := login(t, e, "carol@example.com", "Viewer-pass-123")
	addAuthState(t, e, id, "carol")

	// The display name changes alone: the session stays.
	out := e.admin("PUT", "/accounts/"+strconv.FormatInt(id, 10)+"/display-name", map[string]any{"display_name": " Carol C "})
	if out["display_name"] != "Carol C" {
		t.Fatalf("display name: %v", out)
	}
	if code := e.do(e.web, "GET", "/api/v1/auth/me", nil, browserHeaders(ck), "x:1").Code; code != 200 {
		t.Fatalf("session after a display name change: %d", code)
	}
	if countRows(t, e, "browser_verifications", id) != 1 || countRows(t, e, "auth_codes", id) != 1 {
		t.Fatal("a display name change must not revoke anything")
	}
	if code := e.adminStatus("PUT", "/accounts/"+strconv.FormatInt(id, 10)+"/display-name", map[string]any{"display_name": "a\x01b"}); code != 400 {
		t.Fatalf("control character in display name: %d", code)
	}

	// Duplicate and invalid addresses are refused.
	if code := e.adminStatus("PUT", "/accounts/"+strconv.FormatInt(id, 10)+"/email", map[string]any{"email": "ADMIN@example.com"}); code != http.StatusConflict {
		t.Fatalf("duplicate address: %d", code)
	}
	if code := e.adminStatus("PUT", "/accounts/"+strconv.FormatInt(id, 10)+"/email", map[string]any{"email": "a,b@example.com"}); code != 400 {
		t.Fatalf("invalid address: %d", code)
	}
	if code := e.adminStatus("PUT", "/accounts/99/email", map[string]any{"email": "x@example.com"}); code != 404 {
		t.Fatalf("unknown account: %d", code)
	}

	// The forced address change ends every session, verification and code.
	out = e.admin("PUT", "/accounts/"+strconv.FormatInt(id, 10)+"/email", map[string]any{"email": "carol.new@example.com"})
	if out["email"] != "carol.new@example.com" {
		t.Fatalf("email: %v", out)
	}
	if code := e.do(e.web, "GET", "/api/v1/auth/me", nil, browserHeaders(ck), "x:1").Code; code != 401 {
		t.Fatalf("session after the address change: %d", code)
	}
	if countRows(t, e, "browser_verifications", id) != 0 || countRows(t, e, "auth_codes", id) != 0 {
		t.Fatal("the address change must revoke the verifications and codes")
	}
	if rec := e.do(e.web, "POST", "/api/v1/auth/login", map[string]string{"email": "carol@example.com", "password": "Viewer-pass-123"}, map[string]string{csrfHeader: "1"}, "x:1"); rec.Code != 401 {
		t.Fatalf("login with the old address: %d", rec.Code)
	}
	login(t, e, "carol.new@example.com", "Viewer-pass-123")

	// The history records the operator ("cli" here) and the address.
	items := e.admin("GET", "/history?category=account", nil)["items"].([]any)
	found := false
	for _, it := range items {
		h := it.(map[string]any)
		if h["action"] == "email_changed" {
			found = true
			if h["account_name"] != "cli" || h["target"] != "carol.new@example.com" || !strings.Contains(h["detail"].(string), "carol@example.com -> carol.new@example.com") {
				t.Fatalf("history: %v", h)
			}
		}
	}
	if !found {
		t.Fatal("email_changed not recorded")
	}

	// An administrator in the browser uses the same routes and is recorded
	// by e-mail address.
	adminCk := login(t, e, testAdminEmail, "Admin-pass-123")
	rec := e.do(e.web, "PUT", "/api/v1/accounts/"+strconv.FormatInt(id, 10)+"/display-name", map[string]any{"display_name": "Carol B"}, browserHeaders(adminCk), "x:1")
	if rec.Code != 200 {
		t.Fatalf("browser display name change: %d %s", rec.Code, rec.Body.String())
	}
	items = e.admin("GET", "/history?category=account&limit=1", nil)["items"].([]any)
	if h := items[0].(map[string]any); h["action"] != "display_name_changed" || h["account_name"] != testAdminEmail || h["detail"] != "Carol C -> Carol B" {
		t.Fatalf("history: %v", h)
	}
}

func TestRevocationOnPasswordAndDisable(t *testing.T) {
	e := newTestEnv(t)
	created := e.admin("POST", "/accounts", map[string]any{"email": "dave@example.com", "display_name": "Dave", "role": "viewer", "password": "Viewer-pass-123"})
	id := int64(created["id"].(float64))
	ck1 := login(t, e, "dave@example.com", "Viewer-pass-123")
	ck2 := login(t, e, "dave@example.com", "Viewer-pass-123")
	addAuthState(t, e, id, "dave1")

	// Own password change: the other sessions, all verifications and codes.
	rec := e.do(e.web, "POST", "/api/v1/auth/password", map[string]string{"current_password": "Viewer-pass-123", "new_password": "Viewer-pass-456"}, browserHeaders(ck1), "x:1")
	if rec.Code != 200 {
		t.Fatalf("own password: %d %s", rec.Code, rec.Body.String())
	}
	if e.do(e.web, "GET", "/api/v1/auth/me", nil, browserHeaders(ck1), "x:1").Code != 200 || e.do(e.web, "GET", "/api/v1/auth/me", nil, browserHeaders(ck2), "x:1").Code != 401 {
		t.Fatal("own password change: the own session stays, the others end")
	}
	if countRows(t, e, "browser_verifications", id) != 0 || countRows(t, e, "auth_codes", id) != 0 {
		t.Fatal("own password change must revoke the verifications and codes")
	}

	// Password reset by an administrator: everything.
	addAuthState(t, e, id, "dave2")
	e.admin("PUT", "/accounts/"+strconv.FormatInt(id, 10)+"/password", map[string]any{"password": "Viewer-pass-789"})
	if e.do(e.web, "GET", "/api/v1/auth/me", nil, browserHeaders(ck1), "x:1").Code != 401 || countRows(t, e, "browser_verifications", id) != 0 || countRows(t, e, "auth_codes", id) != 0 {
		t.Fatal("password reset must revoke everything")
	}

	// Role change: sessions only.
	ck := login(t, e, "dave@example.com", "Viewer-pass-789")
	addAuthState(t, e, id, "dave3")
	e.admin("PUT", "/accounts/"+strconv.FormatInt(id, 10)+"/role", map[string]any{"role": "admin"})
	if e.do(e.web, "GET", "/api/v1/auth/me", nil, browserHeaders(ck), "x:1").Code != 401 || countRows(t, e, "browser_verifications", id) != 1 || countRows(t, e, "auth_codes", id) != 1 {
		t.Fatal("role change ends the sessions only")
	}

	// Disable: everything.
	ck = login(t, e, "dave@example.com", "Viewer-pass-789")
	e.admin("POST", "/accounts/"+strconv.FormatInt(id, 10)+"/disable", nil)
	if countRows(t, e, "sessions", id) != 0 || countRows(t, e, "browser_verifications", id) != 0 || countRows(t, e, "auth_codes", id) != 0 {
		t.Fatal("disable must revoke everything")
	}
	_ = ck

	// Delete: cascade.
	e.admin("POST", "/accounts/"+strconv.FormatInt(id, 10)+"/enable", nil)
	addAuthState(t, e, id, "dave4")
	e.admin("DELETE", "/accounts/"+strconv.FormatInt(id, 10), nil)
	if countRows(t, e, "browser_verifications", id) != 0 || countRows(t, e, "auth_codes", id) != 0 {
		t.Fatal("delete must cascade")
	}
}

func TestViewerSeesDisplayNames(t *testing.T) {
	e := newTestEnv(t)
	e.admin("POST", "/accounts", map[string]any{"email": "erin@example.com", "display_name": "Erin", "role": "viewer", "password": "Viewer-pass-123"})
	gone := e.admin("POST", "/accounts", map[string]any{"email": "gone@example.com", "display_name": "Gone", "role": "admin", "password": "Admin-pass-456"})
	adminCk := login(t, e, testAdminEmail, "Admin-pass-123")
	goneCk := login(t, e, "gone@example.com", "Admin-pass-456")
	// Records by an account in the browser, by an account deleted later and
	// by the CLI.
	if rec := e.do(e.web, "POST", "/api/v1/rules", map[string]any{"name": "web"}, browserHeaders(adminCk), "x:1"); rec.Code != 200 {
		t.Fatalf("create rule: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(e.web, "POST", "/api/v1/suppressions", map[string]any{"type": "exec", "comment": "c"}, browserHeaders(goneCk), "x:1"); rec.Code != 200 {
		t.Fatalf("create suppression: %d %s", rec.Code, rec.Body.String())
	}
	e.admin("POST", "/tokens", map[string]any{"name": "host-a"})
	b, _ := models.GetBuiltinAgent(e.c.db)
	tr := &inProcessTransport{c: e.c, agentID: b.ID}
	if err := tr.Report(&modules.ReportRequest{Detections: []modules.DetectionReport{{Type: "read", Category: "normal", Target: "/etc/x"}}}); err != nil {
		t.Fatal(err)
	}
	ack := map[string]any{"items": []map[string]int64{{"agent_id": b.ID, "id": 1}}}
	if rec := e.do(e.web, "POST", "/api/v1/detections/ack", ack, browserHeaders(adminCk), "x:1"); rec.Code != 200 {
		t.Fatalf("ack: %d %s", rec.Code, rec.Body.String())
	}
	e.admin("DELETE", "/accounts/"+strconv.FormatInt(int64(gone["id"].(float64)), 10), nil)

	var stored string
	_ = e.c.db.QueryRow(`SELECT created_by FROM rules WHERE name = 'web'`).Scan(&stored)
	if stored != testAdminEmail {
		t.Fatalf("rules.created_by %q, want the e-mail address", stored)
	}

	viewerCk := login(t, e, "erin@example.com", "Viewer-pass-123")
	get := func(ck, path string) string {
		rec := e.do(e.web, "GET", path, nil, browserHeaders(ck), "x:1")
		if rec.Code != 200 {
			t.Fatalf("%s: %d", path, rec.Code)
		}
		return rec.Body.String()
	}
	// The viewer sees display names instead of the recorded addresses (the
	// target of an account history entry is not an operator and is kept).
	for _, path := range []string{"/api/v1/history", "/api/v1/detections", "/api/v1/detections/" + strconv.FormatInt(b.ID, 10) + "/1", "/api/v1/rules/1", "/api/v1/rules/1/versions/1", "/api/v1/suppressions", "/api/v1/tokens"} {
		body := get(viewerCk, path)
		for _, field := range []string{"account_name", "created_by", "acknowledged_by"} {
			if strings.Contains(body, `"`+field+`":"`+testAdminEmail) || strings.Contains(body, `"`+field+`":"gone@example.com`) {
				t.Fatalf("viewer %s shows an address as %s: %s", path, field, body)
			}
		}
	}
	if body := get(viewerCk, "/api/v1/rules/1"); !strings.Contains(body, `"created_by":"Admin"`) {
		t.Fatalf("viewer rule: %s", body)
	}
	if body := get(viewerCk, "/api/v1/suppressions"); !strings.Contains(body, `"created_by":""`) {
		t.Fatalf("viewer suppression of a deleted account: %s", body)
	}
	if body := get(viewerCk, "/api/v1/tokens"); !strings.Contains(body, `"created_by":"cli"`) {
		t.Fatalf("viewer token by the CLI: %s", body)
	}
	if body := get(viewerCk, "/api/v1/history"); !strings.Contains(body, `"account_name":"Admin"`) || !strings.Contains(body, `"account_name":"cli"`) {
		t.Fatalf("viewer history: %s", body)
	}
	if body := get(viewerCk, "/api/v1/detections"); !strings.Contains(body, `"acknowledged_by":"Admin"`) {
		t.Fatalf("viewer detections: %s", body)
	}
	if body := get(adminCk, "/api/v1/detections"); !strings.Contains(body, `"acknowledged_by":"`+testAdminEmail+`"`) {
		t.Fatalf("admin detections: %s", body)
	}
	// Administrators see the recorded values.
	if body := get(adminCk, "/api/v1/rules/1"); !strings.Contains(body, `"created_by":"`+testAdminEmail+`"`) {
		t.Fatalf("admin rule: %s", body)
	}
	if body := get(adminCk, "/api/v1/suppressions"); !strings.Contains(body, `"created_by":"gone@example.com"`) {
		t.Fatalf("admin suppression: %s", body)
	}
}

func TestSessionIdleExpiry(t *testing.T) {
	e := newTestEnv(t)
	ck := login(t, e, testAdminEmail, "Admin-pass-123")
	id := modules.HashSecret(ck)
	idle := int64(modules.SessionIdleTimeout / time.Second)
	shift := func(lastSeen, expires int64) {
		if _, err := e.c.db.Exec(`UPDATE sessions SET last_seen_at = ?, expires_at = ? WHERE id = ?`, lastSeen, expires, id); err != nil {
			t.Fatal(err)
		}
	}
	session := func() *models.Session {
		s, err := models.GetSession(e.c.db, id)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	now := time.Now().Unix()
	// A background request is authenticated but does not extend the session.
	shift(now-600, now-600+idle)
	hdr := browserHeaders(ck)
	hdr[backgroundHeader] = "1"
	if rec := e.do(e.web, "GET", "/api/v1/overview", nil, hdr, "x:1"); rec.Code != 200 {
		t.Fatalf("background request: %d", rec.Code)
	}
	if s := session(); s.LastSeenAt != now-600 || s.ExpiresAt != now-600+idle {
		t.Fatalf("background request extended the session: %+v", s)
	}
	// An operation extends it to the idle period from now.
	if rec := e.do(e.web, "GET", "/api/v1/overview", nil, browserHeaders(ck), "x:1"); rec.Code != 200 {
		t.Fatalf("operation: %d", rec.Code)
	}
	if s := session(); s.LastSeenAt < now || s.ExpiresAt-s.LastSeenAt != idle {
		t.Fatalf("operation did not extend the session: %+v", s)
	}
	// One idle period and a second after the last operation, it has ended.
	shift(now-idle-1, now-1)
	if rec := e.do(e.web, "GET", "/api/v1/overview", nil, hdr, "x:1"); rec.Code != 401 {
		t.Fatalf("expired session: %d", rec.Code)
	}

	// A remembered session is not extended by operations.
	rec := e.do(e.web, "POST", "/api/v1/auth/login", map[string]any{"email": testAdminEmail, "password": "Admin-pass-123", "remember": true}, map[string]string{csrfHeader: "1"}, "x:1")
	ck = rec.Result().Cookies()[0].Value
	id = modules.HashSecret(ck)
	before := session().ExpiresAt
	shift(now-600, before)
	e.do(e.web, "GET", "/api/v1/overview", nil, browserHeaders(ck), "x:1")
	if s := session(); s.ExpiresAt != before || s.LastSeenAt < now {
		t.Fatalf("remembered session: %+v (expiry before %d)", s, before)
	}
}

func TestHealthAndConfigWithoutFixedPeriods(t *testing.T) {
	e := newTestEnv(t)
	rec := e.do(e.web, "GET", "/api/v1/health", nil, nil, "x:1")
	h := decode[map[string]any](t, rec)
	if h["trust_days"] != float64(modules.DefaultTrustDays) || h["remember_days"] != float64(modules.DefaultRememberDays) {
		t.Fatalf("health: %v", h)
	}
	if body := rec.Body.String(); strings.Contains(body, "3600") || strings.Contains(body, "10800") {
		t.Fatalf("health shows a fixed period: %s", body)
	}
	cfg := e.admin("GET", "/config", nil)
	var names []string
	for _, it := range cfg["items"].([]any) {
		names = append(names, it.(map[string]any)["name"].(string))
	}
	joined := strings.Join(names, ",")
	if strings.Contains(joined, "session_hours") || !strings.Contains(joined, "two_step_trust_days") {
		t.Fatalf("config items: %s", joined)
	}
	e.admin("PUT", "/config", map[string]any{"set": map[string]string{"two_step_trust_days": "7"}})
	if e.c.conf().TwoStepTrustDays != 7 {
		t.Fatalf("two_step_trust_days not applied while running: %d", e.c.conf().TwoStepTrustDays)
	}
	if h := decode[map[string]any](t, e.do(e.web, "GET", "/api/v1/health", nil, nil, "x:1")); h["trust_days"] != float64(7) {
		t.Fatalf("health after change: %v", h)
	}
	if code := e.adminStatus("PUT", "/config", map[string]any{"set": map[string]string{"two_step_trust_days": "366"}}); code != 400 {
		t.Fatalf("two_step_trust_days 366: %d", code)
	}
	if code := e.adminStatus("PUT", "/config", map[string]any{"set": map[string]string{"session_hours": "12"}}); code != 400 {
		t.Fatalf("session_hours: %d", code)
	}
}

func TestRetentionRemovesExpiredCodes(t *testing.T) {
	e := newTestEnv(t)
	now := time.Now().Unix()
	if err := models.InsertBrowserVerification(e.c.db, &models.BrowserVerification{ID: "old", AccountID: 1, CreatedAt: now - 7200, ExpiresAt: now - 1}); err != nil {
		t.Fatal(err)
	}
	if err := models.InsertBrowserVerification(e.c.db, &models.BrowserVerification{ID: "new", AccountID: 1, CreatedAt: now, ExpiresAt: now + 3600}); err != nil {
		t.Fatal(err)
	}
	if err := models.InsertAuthCode(e.c.db, &models.AuthCode{Purpose: models.CodePurposeLogin, AccountID: 1, ChallengeHash: "old", CodeHash: "x", CreatedAt: now - 700, ExpiresAt: now - 100}); err != nil {
		t.Fatal(err)
	}
	e.c.retention(time.Now())
	if countRows(t, e, "browser_verifications", 1) != 1 || countRows(t, e, "auth_codes", 1) != 0 {
		t.Fatal("retention must remove the expired verifications and codes only")
	}
}

// Viewers do not receive the history of accounts and of the system settings.
func TestViewerHistoryCategories(t *testing.T) {
	e := newTestEnv(t)
	e.admin("POST", "/accounts", map[string]any{"email": "v@example.com", "display_name": "V", "role": "viewer", "password": "Viewer-pass-123"})
	e.admin("PUT", "/config", map[string]any{"set": map[string]string{"retention_days": "90"}})
	e.admin("POST", "/rules", map[string]any{"name": "r1"})
	viewer := login(t, e, "v@example.com", "Viewer-pass-123")
	admin := login(t, e, testAdminEmail, "Admin-pass-123")
	list := func(cookie, query string) (int, map[string]int) {
		rec := e.do(e.web, "GET", "/api/v1/history"+query, nil, map[string]string{"Cookie": sessionCookie + "=" + cookie}, "192.0.2.1:1")
		if rec.Code != http.StatusOK {
			t.Fatalf("history %s: %d %s", query, rec.Code, rec.Body.String())
		}
		out := decode[struct {
			Items []struct {
				Category string `json:"category"`
			} `json:"items"`
			Total int `json:"total"`
		}](t, rec)
		seen := map[string]int{}
		for _, it := range out.Items {
			seen[it.Category]++
		}
		return out.Total, seen
	}
	total, seen := list(viewer, "")
	if seen[models.HistAccount] != 0 || seen[models.HistConfig] != 0 || seen[models.HistRule] == 0 {
		t.Errorf("viewer history categories: %v", seen)
	}
	rows := 0
	for _, n := range seen {
		rows += n
	}
	if total != rows {
		t.Errorf("viewer total %d does not match the rows %v", total, seen)
	}
	for _, c := range []string{models.HistAccount, models.HistConfig} {
		if total, seen := list(viewer, "?category="+c); total != 0 || len(seen) != 0 {
			t.Errorf("viewer category %s: total %d rows %v", c, total, seen)
		}
		if total, _ := list(admin, "?category="+c); total == 0 {
			t.Errorf("administrator category %s is empty", c)
		}
	}
}
