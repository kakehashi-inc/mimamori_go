package workers

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"mimamori/app/models"
	"mimamori/app/modules"
)

type testEnv struct {
	t      *testing.T
	c      *core
	agent  http.Handler
	web    http.Handler
	cliKey string
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	modules.MigrationsFS = os.DirFS("../..")
	dir := t.TempDir()
	modules.SetDataDir(dir)
	db, err := modules.OpenSQLite(filepath.Join(dir, modules.CentralDBFileName), modules.SchemaCentral)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	cfg := modules.DefaultCentralConfig()
	c := newCore(db, cfg, dir, log.New(io.Discard, "", 0))
	t.Cleanup(func() { c.closeTargets(); db.Close() })
	c.spaFS = fstest.MapFS{"index.html": {Data: []byte("<!doctype html><title>shell</title>")}}
	if err := c.ensureServerKey(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ensureBuiltinAgent(); err != nil {
		t.Fatal(err)
	}
	if err := c.manageCertificates(); err != nil {
		t.Fatal(err)
	}
	hash, _ := modules.HashPassword("Admin-pass-123")
	admin := &models.Account{Name: "admin", Role: models.RoleAdmin, PasswordHash: hash}
	if err := models.InsertAccount(db, admin); err != nil {
		t.Fatal(err)
	}
	if err := c.writeCLISecret(); err != nil {
		t.Fatal(err)
	}
	return &testEnv{t: t, c: c, agent: c.agentHandler(), web: c.webHandler(), cliKey: c.cliSecret}
}

func (e *testEnv) do(h http.Handler, method, path string, body any, hdr map[string]string, remote string) *httptest.ResponseRecorder {
	e.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, r)
	req.RemoteAddr = remote
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func (e *testEnv) admin(method, path string, body any) map[string]any {
	e.t.Helper()
	rec := e.do(e.agent, method, "/admin/v1"+path, body, map[string]string{"Authorization": "Bearer " + e.cliKey}, "127.0.0.1:5000")
	if rec.Code != 200 {
		e.t.Fatalf("%s %s: %d %s", method, path, rec.Code, rec.Body.String())
	}
	var out any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	m, _ := out.(map[string]any)
	if m == nil {
		m = map[string]any{"list": out}
	}
	return m
}

func (e *testEnv) agentCall(path, token, machine string, body any) *httptest.ResponseRecorder {
	return e.do(e.agent, "POST", "/agent/v1/"+path, body, map[string]string{"Authorization": "Bearer " + token, modules.HeaderInstanceID: machine}, "192.0.2.10:4000")
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return v
}

func TestAdminAPIOnlyFromLoopback(t *testing.T) {
	e := newTestEnv(t)
	rec := e.do(e.agent, "GET", "/admin/v1/agents", nil, map[string]string{"Authorization": "Bearer " + e.cliKey, "X-Forwarded-For": "127.0.0.1"}, "192.0.2.5:1234")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("remote management API: status %d, want 404", rec.Code)
	}
	rec = e.do(e.agent, "GET", "/admin/v1/agents", nil, map[string]string{"Authorization": "Bearer mmc_wrong"}, "127.0.0.1:1234")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad credential: status %d, want 401", rec.Code)
	}
	if rec := e.do(e.agent, "GET", "/admin/v1/agents", nil, map[string]string{"Authorization": "Bearer " + e.cliKey}, "[::1]:1234"); rec.Code != 200 {
		t.Fatalf("::1 client: status %d", rec.Code)
	}
}

func TestAgentFlow(t *testing.T) {
	e := newTestEnv(t)
	e.admin("POST", "/rules", map[string]any{"name": "web", "templates": []string{"base", "apache"}})
	issued := e.admin("POST", "/tokens", map[string]any{"name": "host-a", "rule_id": 1})
	tok := issued["value"].(string)
	if !strings.HasPrefix(tok, modules.TokenPrefix) {
		t.Fatalf("token value %q", tok)
	}
	var stored int
	_ = e.c.db.QueryRow(`SELECT COUNT(*) FROM tokens WHERE hash = ?`, tok).Scan(&stored)
	if stored != 0 {
		t.Fatal("the token value itself must not be stored")
	}

	rec := e.agentCall("sync", tok, "machine-1", modules.SyncRequest{AgentVersion: "test"})
	if rec.Code != 200 {
		t.Fatalf("sync: %d %s", rec.Code, rec.Body.String())
	}
	resp := decode[modules.SyncResponse](t, rec)
	if resp.Rule == nil || resp.Rule.Version != 1 || len(resp.Rule.Content.Users) != 2 {
		t.Fatalf("sync rule: %+v", resp.Rule)
	}
	if len(resp.CAPEMs) == 0 || resp.AgentName != "host-a" {
		t.Fatalf("sync should send the CA and the token name: %+v", resp)
	}
	// Same token from another machine is refused and recorded.
	if rec := e.agentCall("sync", tok, "machine-2", modules.SyncRequest{}); rec.Code != 401 || strings.Contains(rec.Body.String(), "machine") {
		t.Fatalf("other machine: %d %s", rec.Code, rec.Body.String())
	}
	items, _, _ := models.ListHistory(e.c.db, models.HistRejected, "", 10, 0)
	if len(items) == 0 || items[0].Action != "instance id mismatch" {
		t.Fatalf("rejected connection not recorded: %+v", items)
	}

	rep := modules.ReportRequest{
		Detections: []modules.DetectionReport{
			{Type: "exec", Category: "alert", OSUser: "apache", Exe: "/usr/bin/curl", Target: "curl http://x", Count: 2, FirstAt: 100, LastAt: 200},
			{Type: "writable", Category: "alert", OSUser: "apache", Target: "/opt/a/x.php", Count: 1, FirstAt: 100, LastAt: 100, FileCtime: 99, FileOwner: "apache:apache", FileMode: "-rw-rw-rw-"},
		},
		HostInfo: &modules.HostInfo{Hostname: "host-a.example", OSName: "Rocky Linux", OSVersion: "9.4"},
		Apply:    &modules.ApplyResult{Status: "ok", RuleID: 1, Version: 1, At: time.Now().Unix()},
	}
	if rec := e.agentCall("report", tok, "machine-1", rep); rec.Code != 200 {
		t.Fatalf("report: %d %s", rec.Code, rec.Body.String())
	}
	rep.Detections = rep.Detections[:1]
	rep.Detections[0].Count, rep.Detections[0].LastAt = 3, 300
	if rec := e.agentCall("report", tok, "machine-1", rep); rec.Code != 200 {
		t.Fatalf("report 2: %d", rec.Code)
	}
	list := e.admin("GET", "/detections?category=alert", nil)
	if list["total"].(float64) != 2 {
		t.Fatalf("detections: %v", list)
	}
	for _, it := range list["items"].([]any) {
		d := it.(map[string]any)
		if d["type"] == "exec" && d["count"].(float64) != 5 {
			t.Fatalf("exec detection not aggregated: %v", d)
		}
		if d["hostname"] != "host-a.example" || d["agent_name"] != "host-a" {
			t.Fatalf("agent info: %v", d)
		}
	}
	// The detections live in the target's own database file.
	a, _ := models.GetAgentByToken(e.c.db, int64(issued["token"].(map[string]any)["id"].(float64)))
	if _, err := os.Stat(modules.TargetDBPath(e.c.dataDir, a.DBFile)); err != nil {
		t.Fatalf("per-target database: %v", err)
	}
	var n int
	_ = e.c.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'detections'`).Scan(&n)
	if n != 0 {
		t.Fatal("detections must not be stored in the central database")
	}

	// Suppression marks the open detection and later ones.
	e.admin("POST", "/suppressions", map[string]any{"type": "exec", "exe": "/usr/bin/*"})
	sup := e.admin("GET", "/detections?suppressed=true", nil)
	if sup["total"].(float64) != 1 {
		t.Fatalf("suppressed detections: %v", sup)
	}

	// The rule cannot be deleted while applied.
	rec = e.do(e.agent, "DELETE", "/admin/v1/rules/1", nil, map[string]string{"Authorization": "Bearer " + e.cliKey}, "127.0.0.1:1")
	if rec.Code != http.StatusConflict {
		t.Fatalf("delete applied rule: %d", rec.Code)
	}
	agents := e.admin("GET", "/agents", nil)["list"].([]any)
	got := agents[1].(map[string]any)
	if got["apply"].(map[string]any)["status"] != "ok" || got["rule"].(map[string]any)["outdated"] != false {
		t.Fatalf("agent summary: %v", got)
	}
}

func TestTokenRenewalAndReissue(t *testing.T) {
	e := newTestEnv(t)
	issued := e.admin("POST", "/tokens", map[string]any{"name": "host-b", "valid_days": 10})
	old := issued["value"].(string)
	if rec := e.agentCall("sync", old, "m", modules.SyncRequest{}); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	// Remaining 10 days is below the 30-day renewal threshold.
	r1 := decode[modules.SyncResponse](t, e.agentCall("sync", old, "m", modules.SyncRequest{}))
	r2 := decode[modules.SyncResponse](t, e.agentCall("sync", old, "m", modules.SyncRequest{}))
	if r1.TokenRenewal == nil || r2.TokenRenewal == nil || r1.TokenRenewal.Token != r2.TokenRenewal.Token {
		t.Fatalf("the same renewed value must be handed out until used: %+v %+v", r1.TokenRenewal, r2.TokenRenewal)
	}
	renewed := r1.TokenRenewal.Token
	r3 := decode[modules.SyncResponse](t, e.agentCall("sync", renewed, "m", modules.SyncRequest{}))
	if r3.TokenRenewal != nil || r3.AgentName != "host-b" {
		t.Fatalf("after renewal: %+v", r3)
	}
	if rec := e.agentCall("sync", old, "m", modules.SyncRequest{}); rec.Code != 401 {
		t.Fatalf("old value must stop working after the first use of the new one: %d", rec.Code)
	}
	id := int64(issued["token"].(map[string]any)["id"].(float64))
	re := e.admin("POST", "/tokens/"+itoa64(id)+"/reissue", map[string]any{})
	if rec := e.agentCall("sync", renewed, "m", modules.SyncRequest{}); rec.Code != 401 {
		t.Fatalf("reissue must invalidate previous values: %d", rec.Code)
	}
	if rec := e.agentCall("sync", re["value"].(string), "m", modules.SyncRequest{}); rec.Code != 200 {
		t.Fatalf("reissued value: %d", rec.Code)
	}
	e.admin("POST", "/tokens/"+itoa64(id)+"/revoke", nil)
	if rec := e.agentCall("sync", re["value"].(string), "m", modules.SyncRequest{}); rec.Code != 401 {
		t.Fatalf("revoked: %d", rec.Code)
	}
}

func login(t *testing.T, e *testEnv, name, pass string) string {
	rec := e.do(e.web, "POST", "/api/v1/auth/login", map[string]string{"name": name, "password": pass}, map[string]string{csrfHeader: "1"}, "192.0.2.1:1")
	if rec.Code != 200 {
		t.Fatalf("login %s: %d %s", name, rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			return c.Value
		}
	}
	t.Fatal("no session cookie")
	return ""
}

func TestBrowserPermissions(t *testing.T) {
	e := newTestEnv(t)
	adminCk := login(t, e, "admin", "Admin-pass-123")
	hdr := func(ck string, csrf bool) map[string]string {
		h := map[string]string{"Cookie": sessionCookie + "=" + ck}
		if csrf {
			h[csrfHeader] = "1"
		}
		return h
	}
	if rec := e.do(e.web, "POST", "/api/v1/accounts", map[string]string{"name": "view", "role": "viewer", "password": "Viewer-pass-1"}, hdr(adminCk, false), "x:1"); rec.Code != 403 {
		t.Fatalf("missing CSRF header: %d", rec.Code)
	}
	if rec := e.do(e.web, "POST", "/api/v1/accounts", map[string]string{"name": "view", "role": "viewer", "password": "Viewer-pass-1"}, hdr(adminCk, true), "x:1"); rec.Code != 200 {
		t.Fatalf("create viewer: %d %s", rec.Code, rec.Body.String())
	}
	viewCk := login(t, e, "view", "Viewer-pass-1")
	if rec := e.do(e.web, "GET", "/api/v1/agents", nil, hdr(viewCk, false), "x:1"); rec.Code != 200 {
		t.Fatalf("viewer list agents: %d", rec.Code)
	}
	for _, p := range []string{"/api/v1/tokens", "/api/v1/rules", "/api/v1/suppressions", "/api/v1/accounts"} {
		if rec := e.do(e.web, "POST", p, map[string]string{}, hdr(viewCk, true), "x:1"); rec.Code != 403 {
			t.Fatalf("viewer POST %s: %d", p, rec.Code)
		}
	}
	if rec := e.do(e.web, "GET", "/api/v1/accounts", nil, hdr(viewCk, false), "x:1"); rec.Code != 403 {
		t.Fatalf("viewer accounts: %d", rec.Code)
	}
	// The last enabled administrator is protected.
	for _, p := range []string{"/api/v1/accounts/1/disable"} {
		if rec := e.do(e.web, "POST", p, nil, hdr(adminCk, true), "x:1"); rec.Code != 409 {
			t.Fatalf("%s: %d", p, rec.Code)
		}
	}
	if rec := e.do(e.web, "DELETE", "/api/v1/accounts/1", nil, hdr(adminCk, true), "x:1"); rec.Code != 409 {
		t.Fatalf("delete last admin: %d", rec.Code)
	}
	// Changing the viewer's role ends its session immediately.
	if rec := e.do(e.web, "PUT", "/api/v1/accounts/2/role", map[string]string{"role": "admin"}, hdr(adminCk, true), "x:1"); rec.Code != 200 {
		t.Fatalf("role change: %d", rec.Code)
	}
	if rec := e.do(e.web, "GET", "/api/v1/agents", nil, hdr(viewCk, false), "x:1"); rec.Code != 401 {
		t.Fatalf("session after role change: %d", rec.Code)
	}
}

func TestLoginLockout(t *testing.T) {
	e := newTestEnv(t)
	for i := 0; i < modules.LoginMaxFailures; i++ {
		e.do(e.web, "POST", "/api/v1/auth/login", map[string]string{"name": "admin", "password": "wrong"}, map[string]string{csrfHeader: "1"}, "x:1")
	}
	rec := e.do(e.web, "POST", "/api/v1/auth/login", map[string]string{"name": "admin", "password": "Admin-pass-123"}, map[string]string{csrfHeader: "1"}, "x:1")
	if rec.Code != http.StatusLocked {
		t.Fatalf("locked login: %d", rec.Code)
	}
}

func TestBuiltinTransport(t *testing.T) {
	e := newTestEnv(t)
	b, _ := models.GetBuiltinAgent(e.c.db)
	e.admin("POST", "/rules", map[string]any{"name": "r"})
	e.admin("POST", "/rules/1/apply", map[string]any{"version": 1, "agent_ids": []int64{b.ID}})
	tr := &inProcessTransport{c: e.c, agentID: b.ID}
	resp, err := tr.Sync(&modules.SyncRequest{})
	if err != nil || resp.Rule == nil || resp.TokenRenewal != nil || resp.AgentName != modules.BuiltinAgentName {
		t.Fatalf("builtin sync: %+v %v", resp, err)
	}
	if err := tr.Report(&modules.ReportRequest{Detections: []modules.DetectionReport{{Type: "read", Category: "normal", Target: "/etc/ssh/x"}}}); err != nil {
		t.Fatal(err)
	}
	if got := e.admin("GET", "/detections?agent_id="+itoa64(b.ID), nil); got["total"].(float64) != 1 {
		t.Fatalf("builtin detections: %v", got)
	}
}

func TestNotificationCompose(t *testing.T) {
	e := newTestEnv(t)
	b, _ := models.GetBuiltinAgent(e.c.db)
	tr := &inProcessTransport{c: e.c, agentID: b.ID}
	_ = tr.Report(&modules.ReportRequest{Detections: []modules.DetectionReport{
		{Type: "exec", Category: "normal", OSUser: "nginx", Exe: "/bin/sh", Target: "sh -c id"},
		{Type: "exec", Category: "alert", OSUser: "nginx", Exe: "/usr/bin/curl", Target: "curl x"},
	}})
	e.c.composeNotification(time.Now())
	list, _ := models.ListUnsentNotifications(e.c.db)
	if len(list) != 1 || !strings.Contains(list[0].Subject, "ALERT") {
		t.Fatalf("notification: %+v", list)
	}
	body := list[0].Body
	if strings.Index(body, "[ALERT]") > strings.Index(body, "[NORMAL]") {
		t.Fatalf("alerts must come first:\n%s", body)
	}
	// Re-reporting within the dedupe period does not notify again.
	_ = tr.Report(&modules.ReportRequest{Detections: []modules.DetectionReport{{Type: "exec", Category: "alert", OSUser: "nginx", Exe: "/usr/bin/curl", Target: "curl x"}}})
	tdb, _ := e.c.targetDB(b)
	pending, _ := models.ListPendingNotify(tdb)
	if len(pending) != 0 {
		t.Fatalf("deduplicated detection is pending again: %+v", pending)
	}
}

func TestAccountLanguage(t *testing.T) {
	e := newTestEnv(t)
	created := e.admin("POST", "/accounts", map[string]any{"name": "u1", "role": "viewer", "password": "Viewer-pass-123"})
	if created["language"] != "auto" {
		t.Fatalf("default language: %v", created["language"])
	}
	created = e.admin("POST", "/accounts", map[string]any{"name": "u2", "role": "viewer", "language": "ja", "password": "Viewer-pass-123"})
	if created["language"] != "ja" {
		t.Fatalf("given language: %v", created["language"])
	}
	rec := e.do(e.agent, "POST", "/admin/v1/accounts", map[string]any{"name": "u3", "role": "viewer", "language": "fr", "password": "Viewer-pass-123"},
		map[string]string{"Authorization": "Bearer " + e.cliKey}, "127.0.0.1:1")
	if rec.Code != 400 {
		t.Fatalf("unsupported language: %d", rec.Code)
	}
	ck := login(t, e, "u2", "Viewer-pass-123")
	hdr := map[string]string{"Cookie": sessionCookie + "=" + ck, csrfHeader: "1"}
	if me := decode[map[string]any](t, e.do(e.web, "GET", "/api/v1/auth/me", nil, hdr, "x:1")); me["language"] != "ja" {
		t.Fatalf("me: %v", me)
	}
	if rec := e.do(e.web, "PUT", "/api/v1/auth/language", map[string]string{"language": "en"}, hdr, "x:1"); rec.Code != 200 {
		t.Fatalf("change own language: %d", rec.Code)
	}
	if me := decode[map[string]any](t, e.do(e.web, "GET", "/api/v1/auth/me", nil, hdr, "x:1")); me["language"] != "en" {
		t.Fatalf("me after change: %v", me)
	}
}

func TestLoginRemember(t *testing.T) {
	e := newTestEnv(t)
	for _, remember := range []bool{false, true} {
		rec := e.do(e.web, "POST", "/api/v1/auth/login", map[string]any{"name": "admin", "password": "Admin-pass-123", "remember": remember},
			map[string]string{csrfHeader: "1"}, "x:1")
		if rec.Code != 200 {
			t.Fatalf("login: %d", rec.Code)
		}
		ck := rec.Result().Cookies()[0]
		s, err := models.GetSession(e.c.db, modules.HashSecret(ck.Value))
		if err != nil {
			t.Fatal(err)
		}
		days := (s.ExpiresAt - s.CreatedAt) / 86400
		if remember {
			if ck.MaxAge != modules.DefaultRememberDays*86400 || !s.Remember || days != modules.DefaultRememberDays {
				t.Fatalf("remembered: max-age %d, remember %v, days %d", ck.MaxAge, s.Remember, days)
			}
		} else if ck.MaxAge != 0 || s.Remember || s.ExpiresAt-s.CreatedAt != modules.DefaultSessionHours*3600 {
			t.Fatalf("session cookie: max-age %d, remember %v", ck.MaxAge, s.Remember)
		}
	}
	if h := decode[map[string]any](t, e.do(e.web, "GET", "/api/v1/health", nil, nil, "x:1")); h["remember_days"] != float64(modules.DefaultRememberDays) {
		t.Fatalf("health: %v", h)
	}
}

func TestConfigUpdate(t *testing.T) {
	e := newTestEnv(t)
	item := func(m map[string]any, name string) map[string]any {
		for _, it := range m["items"].([]any) {
			if x := it.(map[string]any); x["name"] == name {
				return x
			}
		}
		t.Fatalf("item %s missing", name)
		return nil
	}
	out := e.admin("PUT", "/config", map[string]any{"set": map[string]string{
		"retention_days": "30", "agent_listen": "127.0.0.1:9443", "smtp.password": "pw", "smtp.to": "a@example.com, b@example.com",
	}})
	if e.c.conf().RetentionDays != 30 {
		t.Fatalf("retention_days not applied while running: %d", e.c.conf().RetentionDays)
	}
	if e.c.conf().AgentListen != modules.DefaultAgentListen {
		t.Fatalf("agent_listen must apply at restart only, running %s", e.c.conf().AgentListen)
	}
	if it := item(out, "agent_listen"); it["pending"] != true || it["restart"] != true || it["value"] != "127.0.0.1:9443" {
		t.Fatalf("agent_listen item: %v", it)
	}
	if it := item(out, "smtp.password"); it["value"] != "" || it["is_set"] != true {
		t.Fatalf("password item: %v", it)
	}
	if it := item(out, "smtp.to"); it["value"] != "a@example.com,b@example.com" {
		t.Fatalf("smtp.to item: %v", it)
	}
	rec := e.do(e.agent, "PUT", "/admin/v1/config", map[string]any{"set": map[string]string{"web_listen": "127.0.0.1:9443"}},
		map[string]string{"Authorization": "Bearer " + e.cliKey}, "127.0.0.1:5000")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("same listen addresses: status %d", rec.Code)
	}
	out = e.admin("PUT", "/config", map[string]any{"unset": []string{"retention_days", "agent_listen"}})
	if e.c.conf().RetentionDays != modules.DefaultRetentionDays {
		t.Fatalf("retention_days default not restored: %d", e.c.conf().RetentionDays)
	}
	if it := item(out, "agent_listen"); it["pending"] != false || it["stored"] != false {
		t.Fatalf("agent_listen after unset: %v", it)
	}
}
