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

// reportAlert stores one new alert detection of the built-in agent.
func reportAlert(t *testing.T, e *testEnv, target string) {
	t.Helper()
	b, err := models.GetBuiltinAgent(e.c.db)
	if err != nil {
		t.Fatal(err)
	}
	tr := &inProcessTransport{c: e.c, agentID: b.ID}
	if err := tr.Report(&modules.ReportRequest{Detections: []modules.DetectionReport{{Type: "exec", Category: "alert", OSUser: "nginx", Exe: "/usr/bin/curl", Target: target}}}); err != nil {
		t.Fatal(err)
	}
}

func pendingDetections(t *testing.T, e *testEnv) int {
	t.Helper()
	b, _ := models.GetBuiltinAgent(e.c.db)
	tdb, err := e.c.targetDB(b)
	if err != nil {
		t.Fatal(err)
	}
	list, err := models.ListPendingNotify(tdb)
	if err != nil {
		t.Fatal(err)
	}
	return len(list)
}

func createAccount(t *testing.T, e *testEnv, email, role string) int64 {
	t.Helper()
	out := e.admin("POST", "/accounts", map[string]any{"email": email, "display_name": email, "role": role, "password": "Some-pass-123"})
	return int64(out["id"].(float64))
}

// Notification mails go to the enabled accounts chosen as recipients and to
// no other address. Without any recipient
// nothing is composed or sent and nothing fails.
func TestNotificationRecipientsFromAccounts(t *testing.T) {
	e := newTestEnv(t)
	mb := useMailbox(e)

	// No recipient: no mail, no stored notification, the detection waits.
	reportAlert(t, e, "curl a")
	e.c.notify(time.Now())
	if len(mb.sent) != 0 {
		t.Fatalf("mail without recipients: %+v", mb.sent)
	}
	if list, _ := models.ListUnsentNotifications(e.c.db); len(list) != 0 {
		t.Fatalf("a notification was composed without recipients: %d", len(list))
	}
	if n := pendingDetections(t, e); n != 1 {
		t.Fatalf("pending detections: %d", n)
	}

	alice := createAccount(t, e, "alice@example.com", models.RoleViewer)
	bob := createAccount(t, e, "bob@example.com", models.RoleAdmin)
	createAccount(t, e, "carol@example.com", models.RoleViewer) // never chosen
	for _, id := range []int64{1, alice, bob} {
		if out := e.admin("PUT", "/accounts/"+strconv.FormatInt(id, 10)+"/notify", map[string]any{"notify": true}); out["notify"] != true {
			t.Fatalf("notify on: %v", out)
		}
	}
	// A disabled account keeps its choice but receives nothing.
	e.admin("POST", "/accounts/"+strconv.FormatInt(bob, 10)+"/disable", nil)

	e.c.notify(time.Now())
	if len(mb.sent) != 1 {
		t.Fatalf("mails: %d", len(mb.sent))
	}
	if got := strings.Join(mb.sent[0].to, ","); got != testAdminEmail+",alice@example.com" {
		t.Fatalf("recipients: %s", got)
	}
	if !strings.Contains(mb.sent[0].body, "curl a") || pendingDetections(t, e) != 0 {
		t.Fatalf("mail body %q, pending %d", mb.sent[0].body, pendingDetections(t, e))
	}

	for _, id := range []int64{1, alice} {
		e.admin("PUT", "/accounts/"+strconv.FormatInt(id, 10)+"/notify", map[string]any{"notify": false})
	}

	// Recipients are read when a mail is sent: an undelivered mail goes to
	// the recipients chosen meanwhile, and waits while there are none.
	_ = models.SetSetting(e.c.db, settingLastNotifyAt, "0")
	reportAlert(t, e, "curl c")
	e.c.notify(time.Now())
	if len(mb.sent) != 1 || pendingDetections(t, e) != 1 {
		t.Fatalf("no recipients: %d mails, %d pending", len(mb.sent), pendingDetections(t, e))
	}
	e.admin("POST", "/accounts/"+strconv.FormatInt(bob, 10)+"/enable", nil)
	e.c.notify(time.Now())
	if len(mb.sent) != 2 || strings.Join(mb.sent[1].to, ",") != "bob@example.com" {
		t.Fatalf("re-enabled account: %+v", mb.sent)
	}

	// Without smtp.from nothing is sent even with recipients.
	setConfig(e, func(c *modules.CentralConfig) { c.SMTP.From = "" })
	_ = models.SetSetting(e.c.db, settingLastNotifyAt, "0")
	reportAlert(t, e, "curl d")
	e.c.notify(time.Now())
	if len(mb.sent) != 2 {
		t.Fatalf("mail without smtp.from: %d", len(mb.sent))
	}
}

// The account list returns the flag; the notify route sets it, records the
// change, and validates its input. Administrators in the browser use the
// same route.
func TestAccountNotifyAPI(t *testing.T) {
	e := newTestEnv(t)
	id := createAccount(t, e, "dave@example.com", models.RoleViewer)
	path := "/accounts/" + strconv.FormatInt(id, 10) + "/notify"
	listed := func() map[string]any {
		for _, a := range e.admin("GET", "/accounts", nil)["list"].([]any) {
			if m := a.(map[string]any); m["email"] == "dave@example.com" {
				return m
			}
		}
		t.Fatal("account not listed")
		return nil
	}
	if v, ok := listed()["notify"]; !ok || v != false {
		t.Fatalf("default notify: %v", listed())
	}
	if out := e.admin("PUT", path, map[string]any{"notify": true}); out["notify"] != true || out["email"] != "dave@example.com" {
		t.Fatalf("notify on: %v", out)
	}
	if listed()["notify"] != true {
		t.Fatal("flag not stored")
	}
	e.admin("PUT", path, map[string]any{"notify": true}) // unchanged: no history
	for _, body := range []any{map[string]any{}, map[string]any{"notify": "yes"}, map[string]any{"notify": nil}, map[string]any{"notify": true, "x": 1}} {
		if code := e.adminStatus("PUT", path, body); code != http.StatusBadRequest {
			t.Errorf("body %v: %d", body, code)
		}
	}
	if code := e.adminStatus("PUT", "/accounts/99/notify", map[string]any{"notify": true}); code != http.StatusNotFound {
		t.Errorf("unknown account: %d", code)
	}
	adminCk := login(t, e, testAdminEmail, "Admin-pass-123")
	rec := e.do(e.web, "PUT", "/api/v1"+path, map[string]any{"notify": false}, browserHeaders(adminCk), "x:1")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"notify":false`) {
		t.Fatalf("browser: %d %s", rec.Code, rec.Body.String())
	}
	var details []string
	for _, it := range e.admin("GET", "/history?category=account", nil)["items"].([]any) {
		if h := it.(map[string]any); h["action"] == "notify_changed" {
			if h["target"] != "dave@example.com" {
				t.Fatalf("history target: %v", h)
			}
			details = append(details, h["account_name"].(string)+": "+h["detail"].(string))
		}
	}
	if got := strings.Join(details, "|"); got != testAdminEmail+": notification mails off|cli: notification mails on" {
		t.Fatalf("history: %s", got)
	}
	// The flag survives disabling (and is kept for when it is enabled again).
	e.admin("POST", "/accounts/"+strconv.FormatInt(id, 10)+"/disable", nil)
	e.admin("PUT", path, map[string]any{"notify": true})
	if a, _ := models.GetAccount(e.c.db, id); !a.Notify || !a.Disabled {
		t.Fatalf("disabled account flag: %+v", a)
	}
	if list, _ := models.ListNotifyEmails(e.c.db); len(list) != 0 {
		t.Fatalf("disabled account listed as recipient: %v", list)
	}
}

// Deleting a suppression releases the unacknowledged detections it
// suppressed: they are notified like any pending detection. Acknowledged
// ones stay as they are, and one that another suppression matches stays
// suppressed under that suppression.
func TestDeleteSuppressionReleases(t *testing.T) {
	e := newTestEnv(t)
	mb := useMailbox(e)
	e.admin("PUT", "/accounts/1/notify", map[string]any{"notify": true})
	b, _ := models.GetBuiltinAgent(e.c.db)
	tdb, err := e.c.targetDB(b)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"curl a", "curl b", "curl c"} {
		reportAlert(t, e, target)
	}
	byTarget := func() map[string]*models.Detection {
		list, _, err := models.ListDetections(tdb, models.DetectionFilter{}, 100)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]*models.Detection{}
		for _, d := range list {
			out[d.Target] = d
		}
		return out
	}
	s1 := e.admin("POST", "/suppressions", map[string]any{"type": "exec", "exe": "/usr/bin/curl"})
	s1ID := int64(s1["id"].(float64))
	s2 := e.admin("POST", "/suppressions", map[string]any{"target": "curl c"})
	s2ID := int64(s2["id"].(float64))
	ds := byTarget()
	for _, target := range []string{"curl a", "curl b", "curl c"} {
		if d := ds[target]; !d.Suppressed || d.SuppressionID != s1ID || d.NotifyState != models.NotifySkipped {
			t.Fatalf("%s before: %+v", target, d)
		}
	}
	e.admin("POST", "/detections/ack", map[string]any{"items": []map[string]any{{"agent_id": b.ID, "id": ds["curl b"].ID}}})
	if pendingDetections(t, e) != 0 {
		t.Fatal("suppressed detections pending")
	}

	e.admin("DELETE", "/suppressions/"+strconv.FormatInt(s1ID, 10), nil)
	ds = byTarget()
	if d := ds["curl a"]; d.Suppressed || d.SuppressionID != 0 || d.NotifyState != models.NotifyPending {
		t.Fatalf("curl a not released: %+v", d)
	}
	if d := ds["curl b"]; !d.Suppressed || d.SuppressionID != s1ID || d.AcknowledgedAt == 0 {
		t.Fatalf("acknowledged curl b changed: %+v", d)
	}
	if d := ds["curl c"]; !d.Suppressed || d.SuppressionID != s2ID || d.NotifyState != models.NotifySkipped {
		t.Fatalf("curl c must stay suppressed by the other suppression: %+v", d)
	}
	hist, _, _ := models.ListHistory(e.c.db, models.HistSuppression, "", nil, 10, 0)
	if len(hist) == 0 || hist[0].Action != "deleted" || !strings.Contains(hist[0].Detail, "1 detection(s) no longer suppressed") {
		t.Fatalf("history: %+v", hist)
	}
	e.c.notify(time.Now())
	if len(mb.sent) != 1 || !strings.Contains(mb.sent[0].body, "curl a") || strings.Contains(mb.sent[0].body, "curl c") {
		t.Fatalf("mails after release: %+v", mb.sent)
	}

	// A released detection notified within the dedupe period stays skipped.
	e.admin("POST", "/suppressions", map[string]any{"target": "curl a"})
	s3 := e.admin("GET", "/suppressions", nil)["list"].([]any)[0].(map[string]any)
	e.admin("DELETE", "/suppressions/"+strconv.FormatInt(int64(s3["id"].(float64)), 10), nil)
	if d := byTarget()["curl a"]; d.Suppressed || d.NotifyState != models.NotifySent {
		t.Fatalf("notified curl a after a second release: %+v", d)
	}
}
