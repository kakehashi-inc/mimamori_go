package workers

import (
	"bufio"
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"mimamori/app/models"
	"mimamori/app/modules"
)

// While a mail is undelivered, no further mail is composed: new detections
// wait for the next mail, so at most one mail per interval goes out after an
// outage.
func TestNotificationNotComposedWhileUnsent(t *testing.T) {
	e := newTestEnv(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	cfg := *e.c.conf()
	cfg.SMTP = modules.SMTPConfig{Host: "127.0.0.1", Port: port, Security: "none", From: "a@example.com", TimeoutSec: 2}
	e.c.cfg.Store(&cfg)
	e.admin("PUT", "/accounts/1/notify", map[string]any{"notify": true})

	b, _ := models.GetBuiltinAgent(e.c.db)
	tr := &inProcessTransport{c: e.c, agentID: b.ID}
	_ = tr.Report(&modules.ReportRequest{Detections: []modules.DetectionReport{{Type: "exec", Category: "alert", OSUser: "nginx", Exe: "/usr/bin/curl", Target: "curl x"}}})
	e.c.notify(time.Now())
	if list, _ := models.ListUnsentNotifications(e.c.db); len(list) != 1 {
		t.Fatalf("first mail: %d unsent", len(list))
	}
	if e.c.settingInt(settingLastNotifyAt) != 0 {
		t.Fatal("the interval must start only when a mail is delivered")
	}
	_ = tr.Report(&modules.ReportRequest{Detections: []modules.DetectionReport{{Type: "exec", Category: "normal", OSUser: "nginx", Exe: "/bin/sh", Target: "sh -c id"}}})
	e.c.notify(time.Now().Add(2 * time.Hour))
	if list, _ := models.ListUnsentNotifications(e.c.db); len(list) != 1 {
		t.Fatalf("a second mail was composed while one is undelivered: %d unsent", len(list))
	}
	tdb, _ := e.c.targetDB(b)
	if pending, _ := models.ListPendingNotify(tdb); len(pending) != 1 {
		t.Fatalf("the new detection must wait for the next mail: %d pending", len(pending))
	}
}

func TestBusyResponseLength(t *testing.T) {
	resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(busyResponse)), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusServiceUnavailable || string(body) != `{"error":"busy"}` {
		t.Fatalf("503 response: %d %q %v", resp.StatusCode, body, err)
	}
}

func TestRequestInputValidation(t *testing.T) {
	e := newTestEnv(t)
	cli := map[string]string{"Authorization": "Bearer " + e.cliKey}
	req := httptest.NewRequest("POST", "/admin/v1/tokens", strings.NewReader(`{"name":"t-trailing"} {"x":1}`))
	req.RemoteAddr = "127.0.0.1:1"
	req.Header.Set("Authorization", "Bearer "+e.cliKey)
	rec := httptest.NewRecorder()
	e.agent.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("trailing data after the JSON body: %d", rec.Code)
	}
	if list := e.admin("GET", "/tokens", nil)["list"].([]any); len(list) != 0 {
		t.Fatal("no token may be issued from a rejected body")
	}
	for _, q := range []string{
		"/detections?from=garbage", "/detections?to=2026-13-01", "/detections?acknowledged=yes", "/detections?suppressed=%27%20OR%201",
		"/detections?limit=ten", "/detections?offset=x", "/detections?agent_id=abc", "/history?limit=x", "/history?offset=1.5",
	} {
		if rec := e.do(e.agent, "GET", "/admin/v1"+q, nil, cli, "127.0.0.1:1"); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid") {
			t.Fatalf("%s: %d %s", q, rec.Code, rec.Body.String())
		}
	}
	for _, q := range []string{
		"/detections?from=2026-01-02&to=2026-01-03T10:00&acknowledged=false&suppressed=0&limit=1000&offset=0&agent_id=1",
		"/detections?from=2026-01-02T10:00:00Z&acknowledged=", "/history?limit=10&offset=0",
	} {
		e.admin("GET", q, nil)
	}
}

// An agent request that read the token before an administrator changed it
// must not write the old state back.
func TestTokenUseKeepsConcurrentRevoke(t *testing.T) {
	e := newTestEnv(t)
	issued := e.admin("POST", "/tokens", map[string]any{"name": "host-r", "valid_days": 10})
	id := int64(issued["token"].(map[string]any)["id"].(float64))
	read, _ := models.GetToken(e.c.db, id)
	prev := *read
	e.admin("POST", "/tokens/"+itoa64(id)+"/revoke", nil)
	read.InstanceID, read.BoundAt, read.UsedAt, read.LastSeenAt = "m", 1, 1, 1
	if err := models.RecordTokenUse(e.c.db, read, &prev); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("token use after a revoke: %v", err)
	}
	if got, _ := models.GetToken(e.c.db, id); got.RevokedAt == 0 || got.InstanceID != "" {
		t.Fatalf("revoke undone: %+v", got)
	}

	issued = e.admin("POST", "/tokens", map[string]any{"name": "host-s", "valid_days": 10})
	id = int64(issued["token"].(map[string]any)["id"].(float64))
	read, _ = models.GetToken(e.c.db, id)
	re := e.admin("POST", "/tokens/"+itoa64(id)+"/reissue", map[string]any{})
	read.RenewCounter, read.PendingHash, read.PendingExpiresAt = 1, "x", 1
	if err := models.SetTokenRenewal(e.c.db, read, "", 0); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("renewal after a reissue: %v", err)
	}
	if rec := e.agentCall("sync", re["value"].(string), "m", modules.SyncRequest{}); rec.Code != 200 {
		t.Fatalf("reissued value after the stale write: %d", rec.Code)
	}
}

func TestLoginFailureCountedAtomically(t *testing.T) {
	e := newTestEnv(t)
	lock := time.Now().Unix() + 900
	for i := 1; i <= modules.LoginMaxFailures; i++ {
		locked, err := models.RecordLoginFailure(e.c.db, 1, modules.LoginMaxFailures, lock)
		if err != nil || locked != (i == modules.LoginMaxFailures) {
			t.Fatalf("failure %d: locked %v %v", i, locked, err)
		}
	}
	// A failure of a request that started before the lock must keep it.
	if locked, _ := models.RecordLoginFailure(e.c.db, 1, modules.LoginMaxFailures, 0); locked {
		t.Fatal("a single failure must not lock")
	}
	a, _ := models.GetAccount(e.c.db, 1)
	if a.LockedUntil != lock || a.FailedLogins != 1 {
		t.Fatalf("lock cleared or count lost: %+v", a)
	}
}

func TestLastAdminProtectedInTransaction(t *testing.T) {
	e := newTestEnv(t)
	e.admin("POST", "/accounts", map[string]any{"email": "adm2@example.com", "display_name": "Admin 2", "role": "admin", "password": "Admin-pass-456"})
	e.admin("POST", "/accounts/1/disable", nil)
	cli := map[string]string{"Authorization": "Bearer " + e.cliKey}
	for _, rq := range []struct {
		method, path string
		body         any
	}{{"POST", "/accounts/2/disable", nil}, {"PUT", "/accounts/2/role", map[string]string{"role": "viewer"}}, {"DELETE", "/accounts/2", nil}} {
		if rec := e.do(e.agent, rq.method, "/admin/v1"+rq.path, rq.body, cli, "127.0.0.1:1"); rec.Code != http.StatusConflict {
			t.Fatalf("%s %s: %d", rq.method, rq.path, rec.Code)
		}
	}
	if n, _ := models.CountActiveAdmins(e.c.db); n != 1 {
		t.Fatalf("enabled administrators: %d", n)
	}
	e.admin("DELETE", "/accounts/1", nil)
}

func TestTokenIssueUndoneWhenAgentFails(t *testing.T) {
	e := newTestEnv(t)
	blocker := modules.TargetDBPath(e.c.dataDir, "agent-2.db")
	if err := os.MkdirAll(blocker, 0o700); err != nil {
		t.Fatal(err)
	}
	cli := map[string]string{"Authorization": "Bearer " + e.cliKey}
	if rec := e.do(e.agent, "POST", "/admin/v1/tokens", map[string]any{"name": "host-f"}, cli, "127.0.0.1:1"); rec.Code != http.StatusInternalServerError {
		t.Fatalf("issue with an unusable target database: %d", rec.Code)
	}
	if list := e.admin("GET", "/tokens", nil)["list"].([]any); len(list) != 0 {
		t.Fatalf("a token was left without its agent: %v", list)
	}
	e.admin("POST", "/tokens", map[string]any{"name": "host-f"})
}

func TestSuppressionKeepsConcurrentOccurrences(t *testing.T) {
	e := newTestEnv(t)
	b, _ := models.GetBuiltinAgent(e.c.db)
	tr := &inProcessTransport{c: e.c, agentID: b.ID}
	rep := &modules.ReportRequest{Detections: []modules.DetectionReport{{Type: "exec", Category: "normal", OSUser: "nginx", Exe: "/bin/sh", Target: "sh", Count: 1, FirstAt: 100, LastAt: 100}}}
	_ = tr.Report(rep)
	tdb, _ := e.c.targetDB(b)
	f := false
	stale, _, _ := models.ListDetections(tdb, models.DetectionFilter{Suppressed: &f}, 10)
	rep.Detections[0].LastAt = 200
	_ = tr.Report(rep)
	if err := models.MarkDetectionSuppressed(tdb, stale[0].ID, 7); err != nil {
		t.Fatal(err)
	}
	d, _ := models.GetDetection(tdb, stale[0].ID)
	if !d.Suppressed || d.SuppressionID != 7 || d.Count != 2 || d.LastAt != 200 || d.NotifyState != models.NotifySkipped {
		t.Fatalf("detection after suppression: %+v", d)
	}
}

func TestRejectedTokenReasons(t *testing.T) {
	e := newTestEnv(t)
	for _, tc := range []struct{ auth, reason string }{{"", "missing token"}, {"Bearer bad", "invalid token"}} {
		hdr := map[string]string{modules.HeaderInstanceID: "m"}
		if tc.auth != "" {
			hdr["Authorization"] = tc.auth
		}
		if rec := e.do(e.agent, "POST", "/agent/v1/sync", modules.SyncRequest{}, hdr, "192.0.2.20:1"); rec.Code != 401 {
			t.Fatalf("%q: %d", tc.auth, rec.Code)
		}
		items, _, _ := models.ListHistory(e.c.db, models.HistRejected, "", nil, 1, 0)
		if len(items) == 0 || items[0].Action != tc.reason {
			t.Fatalf("%q recorded as %+v", tc.auth, items)
		}
	}
}

func TestNeverConnectedAgentNotMissingCA(t *testing.T) {
	e := newTestEnv(t)
	e.admin("POST", "/tokens", map[string]any{"name": "host-n"})
	for _, it := range e.admin("GET", "/agents", nil)["list"].([]any) {
		if a := it.(map[string]any); a["ca_up_to_date"] != true {
			t.Fatalf("never connected agent flagged: %v", a)
		}
	}
	if m := e.admin("GET", "/certificates", nil)["agents_missing_new_ca"].([]any); len(m) != 0 {
		t.Fatalf("certificate status: %v", m)
	}
}

func TestSuppressionFieldLimits(t *testing.T) {
	e := newTestEnv(t)
	cli := map[string]string{"Authorization": "Bearer " + e.cliKey}
	for _, body := range []map[string]any{
		{"exe": "/bin/a\x00b"}, {"target": "a\r\nb"}, {"os_user": "a\tb"}, {"exe": "/" + strings.Repeat("x", 4096)},
		{"os_user": strings.Repeat("u", 257)}, {"type": "exec", "comment": strings.Repeat("c", 1001)},
	} {
		if rec := e.do(e.agent, "POST", "/admin/v1/suppressions", body, cli, "127.0.0.1:1"); rec.Code != http.StatusBadRequest {
			t.Fatalf("%.40v: %d", body, rec.Code)
		}
	}
	e.admin("POST", "/suppressions", map[string]any{"exe": "/" + strings.Repeat("x", 4095), "os_user": strings.Repeat("u", 256), "comment": strings.Repeat("\u3042", 1000)})
}

// Requests of one agent that use a new token at the same time are all
// accepted: the request that loses the first binding checks the token again.
func TestConcurrentFirstUseOfToken(t *testing.T) {
	e := newTestEnv(t)
	for round := 0; round < 5; round++ {
		issued := e.admin("POST", "/tokens", map[string]any{"name": fmt.Sprintf("race%d", round)})
		value, _ := issued["value"].(string)
		if value == "" {
			t.Fatalf("no token value: %v", issued)
		}
		var wg sync.WaitGroup
		codes := make(chan int, 8)
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				codes <- e.agentCall("sync", value, "instance-1", map[string]any{}).Code
			}()
		}
		wg.Wait()
		close(codes)
		for code := range codes {
			if code != http.StatusOK {
				t.Fatalf("round %d: status %d, want 200", round, code)
			}
		}
		// Another instance is still refused.
		if code := e.agentCall("sync", value, "instance-2", map[string]any{}).Code; code != http.StatusUnauthorized {
			t.Fatalf("round %d: other instance got %d", round, code)
		}
	}
}
