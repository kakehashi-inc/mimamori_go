package workers

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mimamori/app/models"
	"mimamori/app/modules"
)

// stepUpFixture is a central with an SMTP server (so re-authentication
// applies), an agent and two stored command sets: setID is changed and
// imported, delID is deleted.
type stepUpFixture struct {
	e     *testEnv
	mb    *mailbox
	base  string // /api/v1/agents/<id>/command-sets
	setID string
	delID string
	agent int64
}

func newStepUpFixture(t *testing.T) *stepUpFixture {
	t.Helper()
	e := newTestEnv(t)
	_, a, _ := commandAgent(t, e, "host-s")
	ids := []string{}
	for _, name := range []string{"first", "second"} {
		body := commandSetBody(name, "collect_output")
		body["os_user"] = "svc"
		rec := e.cli("POST", "/agents/"+itoa64(a.ID)+"/command-sets", body)
		if rec.Code != 200 {
			t.Fatalf("create through the CLI: %d %s", rec.Code, rec.Body.String())
		}
		ids = append(ids, itoa64(int64(decode[map[string]any](t, rec)["id"].(float64))))
	}
	mb := useMailbox(e)
	return &stepUpFixture{e: e, mb: mb, base: "/api/v1/agents/" + itoa64(a.ID) + "/command-sets", setID: ids[0], delID: ids[1], agent: a.ID}
}

// login logs the administrator in with the login code and returns the
// session cookie value.
func (f *stepUpFixture) login(t *testing.T) string {
	t.Helper()
	ck, _ := twoStepLogin(t, f.e, f.mb, testAdminEmail, "Admin-pass-123", false)
	return ck.Value
}

func (f *stepUpFixture) call(ck, method, path string, body any) *httptest.ResponseRecorder {
	return f.e.do(f.e.web, method, path, body, browserHdr(ck, true), "192.0.2.1:1")
}

// changes sends a create, an update and an import (the changing operations
// besides delete) and returns the status codes.
func (f *stepUpFixture) changes(t *testing.T, ck, suffix string) []int {
	t.Helper()
	create := commandSetBody("new-"+suffix, "run_only")
	create["os_user"] = "svc"
	for _, c := range create["commands"].([]map[string]any) {
		delete(c, "patterns")
	}
	update := commandSetBody("first", "collect_output")
	update["os_user"] = "svc"
	update["description"] = "changed " + suffix
	imp := map[string]any{"source_agent_id": f.agent, "items": []any{map[string]any{"set_id": json.Number(f.setID), "name": "copy-" + suffix}}}
	return []int{
		f.call(ck, "POST", f.base, create).Code,
		f.call(ck, "PUT", f.base+"/"+f.setID, update).Code,
		f.call(ck, "POST", f.base+"/import", imp).Code,
	}
}

// stepUp requests a code and enters it.
func (f *stepUpFixture) stepUp(t *testing.T, ck string) {
	t.Helper()
	if rec := f.call(ck, "POST", "/api/v1/auth/step-up", nil); rec.Code != 202 {
		t.Fatalf("request code: %d %s", rec.Code, rec.Body.String())
	}
	rec := f.call(ck, "POST", "/api/v1/auth/step-up/verify", map[string]any{"code": f.mb.lastCode(t)})
	if rec.Code != 200 {
		t.Fatalf("verify: %d %s", rec.Code, rec.Body.String())
	}
	if out := decode[map[string]any](t, rec); out["required"] != true || len(out) != 1 {
		t.Fatalf("verify answer: %v", out)
	}
}

func allStatus(codes []int, want int) bool {
	for _, c := range codes {
		if c != want {
			return false
		}
	}
	return true
}

func TestStepUpRequiredForChanges(t *testing.T) {
	f := newStepUpFixture(t)
	e := f.e
	ck := f.login(t) // the login code does not count

	if got := f.changes(t, ck, "a"); !allStatus(got, 428) {
		t.Fatalf("changes without re-authentication: %v, want 428", got)
	}
	rec := f.call(ck, "DELETE", f.base+"/"+f.delID, nil)
	if rec.Code != 428 {
		t.Fatalf("delete without re-authentication: %d", rec.Code)
	}
	if out := decode[map[string]any](t, rec); out["step_up_required"] != true || !strings.Contains(out["error"].(string), "re-authentication") {
		t.Fatalf("428 body: %v", out)
	}
	// Nothing was changed by the refused requests.
	if n := len(decode[map[string]any](t, f.call(ck, "GET", f.base, nil))["sets"].([]any)); n != 2 {
		t.Fatalf("sets after refused changes: %d", n)
	}

	// Reading, validating, requesting and discarding runs are not asked.
	if rec := f.call(ck, "POST", f.base+"/validate", commandSetBody("x", "collect_output")); rec.Code != 200 {
		t.Fatalf("validate: %d", rec.Code)
	}
	rec = f.call(ck, "POST", f.base+"/"+f.setID+"/run", nil)
	if rec.Code != 200 {
		t.Fatalf("run request: %d %s", rec.Code, rec.Body.String())
	}
	req := itoa64(int64(decode[map[string]any](t, rec)["id"].(float64)))
	if rec := f.call(ck, "DELETE", "/api/v1/agents/"+itoa64(f.agent)+"/command-requests/"+req, nil); rec.Code != 200 {
		t.Fatalf("discard: %d %s", rec.Code, rec.Body.String())
	}

	// The code goes to the account's address.
	sent := len(f.mb.sent)
	f.stepUp(t, ck)
	if len(f.mb.sent) != sent+1 || f.mb.sent[sent].to[0] != testAdminEmail || !strings.Contains(f.mb.sent[sent].body, "command sets") {
		t.Fatalf("mail: %+v", f.mb.sent[sent:])
	}
	if got := f.changes(t, ck, "b"); !allStatus(got, 200) {
		t.Fatalf("changes after re-authentication: %v", got)
	}
	if rec := f.call(ck, "DELETE", f.base+"/"+f.delID, nil); rec.Code != 200 {
		t.Fatalf("delete after re-authentication: %d %s", rec.Code, rec.Body.String())
	}

	// Another session of the same account is not covered.
	other := f.login(t)
	if got := f.changes(t, other, "c"); !allStatus(got, 428) {
		t.Fatalf("other session: %v", got)
	}

	// The re-authentication lasts one hour.
	sid := modules.HashSecret(ck)
	move := func(by time.Duration) {
		t.Helper()
		if err := e.exec(`UPDATE sessions SET step_up_at = step_up_at - ? WHERE id = ?`, int64(by/time.Second), sid); err != nil {
			t.Fatal(err)
		}
	}
	move(modules.StepUpValidity - time.Minute)
	if got := f.changes(t, ck, "d"); !allStatus(got, 200) {
		t.Fatalf("59 minutes after: %v", got)
	}
	move(time.Minute)
	if got := f.changes(t, ck, "e"); !allStatus(got, 428) {
		t.Fatalf("an hour after: %v", got)
	}

	// The CLI is never asked.
	body := commandSetBody("from-cli", "collect_output")
	body["os_user"] = "svc"
	if rec := e.cli("POST", "/agents/"+itoa64(f.agent)+"/command-sets", body); rec.Code != 200 {
		t.Fatalf("CLI create: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.cli("POST", "/auth/step-up", nil); rec.Code != 404 {
		t.Fatalf("the management API has no step-up route: %d", rec.Code)
	}
}

func TestStepUpCode(t *testing.T) {
	f := newStepUpFixture(t)
	e := f.e
	ck := f.login(t)
	request := func(c string) {
		t.Helper()
		if rec := f.call(c, "POST", "/api/v1/auth/step-up", nil); rec.Code != 202 {
			t.Fatalf("request code: %d %s", rec.Code, rec.Body.String())
		}
	}
	verify := func(c, code string) int {
		return f.call(c, "POST", "/api/v1/auth/step-up/verify", map[string]any{"code": code}).Code
	}

	// Without a requested code.
	if got := verify(ck, "123456"); got != 410 {
		t.Fatalf("no code requested: %d", got)
	}
	// A wrong code is 400 (never 401) and the session stays.
	request(ck)
	code := f.mb.lastCode(t)
	if got := verify(ck, wrongCode(code)); got != 400 {
		t.Fatalf("wrong code: %d", got)
	}
	if rec := f.call(ck, "GET", "/api/v1/auth/me", nil); rec.Code != 200 {
		t.Fatalf("session after a wrong code: %d", rec.Code)
	}
	// The code is bound to the session that requested it.
	other := f.login(t)
	if got := verify(other, code); got != 410 {
		t.Fatalf("code entered in another session: %d", got)
	}
	// Stored only as a keyed hash.
	var stored string
	if err := e.c.db.Read().QueryRow(`SELECT code_hash FROM auth_codes WHERE purpose = ?`, models.CodePurposeStepUp).Scan(&stored); err != nil || stored == code || strings.Contains(stored, code) {
		t.Fatalf("stored code: %q %v", stored, err)
	}
	// Used once.
	if got := verify(ck, code); got != 200 {
		t.Fatalf("right code: %d", got)
	}
	if got := verify(ck, code); got != 410 {
		t.Fatalf("code used twice: %d", got)
	}

	// Five wrong entries discard the code.
	request(ck)
	code = f.mb.lastCode(t)
	for i := 1; i < modules.AuthCodeMaxAttempts; i++ {
		if got := verify(ck, wrongCode(code)); got != 400 {
			t.Fatalf("wrong entry %d: %d", i, got)
		}
	}
	if got := verify(ck, wrongCode(code)); got != 410 {
		t.Fatalf("last wrong entry: %d", got)
	}
	if got := verify(ck, code); got != 410 {
		t.Fatalf("right code after too many wrong ones: %d", got)
	}
	// Wrong codes do not lock the login.
	if a, err := models.GetAccountByEmail(e.c.db.Read(), testAdminEmail); err != nil || a.LockedUntil > 0 {
		t.Fatalf("account after wrong codes: %+v %v", a, err)
	}

	// An expired code.
	request(ck)
	code = f.mb.lastCode(t)
	if err := e.exec(`UPDATE auth_codes SET expires_at = ? WHERE purpose = ?`, time.Now().Unix()-1, models.CodePurposeStepUp); err != nil {
		t.Fatal(err)
	}
	if got := verify(ck, code); got != 410 {
		t.Fatalf("expired code: %d", got)
	}

	// A mail that cannot be sent: 503, nothing stored, history.
	f.mb.fail = errors.New("connection refused")
	if rec := f.call(ck, "POST", "/api/v1/auth/step-up", nil); rec.Code != 503 {
		t.Fatalf("mail failure: %d", rec.Code)
	}
	if n := countAll(t, e, "auth_codes WHERE purpose = 'step_up'"); n != 0 {
		t.Fatalf("codes after a mail failure: %d", n)
	}
	items := e.admin("GET", "/history?category=account&limit=1", nil)["items"].([]any)
	if h := items[0].(map[string]any); h["action"] != "step_up_code_failed" || !strings.Contains(h["detail"].(string), "connection refused") {
		t.Fatalf("history: %v", h)
	}
}

func TestStepUpEndsWithPasswordChange(t *testing.T) {
	f := newStepUpFixture(t)
	ck := f.login(t)
	f.stepUp(t, ck)
	rec := f.call(ck, "POST", "/api/v1/auth/password", map[string]any{"current_password": "Admin-pass-123", "new_password": "Changed-pass-456"})
	if rec.Code != 200 {
		t.Fatalf("password change: %d %s", rec.Code, rec.Body.String())
	}
	// The session that changed the password stays, its re-authentication ends.
	if rec := f.call(ck, "GET", "/api/v1/auth/me", nil); rec.Code != 200 {
		t.Fatalf("session after the password change: %d", rec.Code)
	}
	if got := f.changes(t, ck, "p"); !allStatus(got, 428) {
		t.Fatalf("changes after the password change: %v", got)
	}
}

func TestStepUpDevelopmentModeAndViewer(t *testing.T) {
	e := newTestEnv(t)
	_, a, _ := commandAgent(t, e, "host-d")
	viewer := newViewer(t, e)
	ck := login(t, e, testAdminEmail, "Admin-pass-123")
	body := commandSetBody("dev", "collect_output")
	body["os_user"] = "svc"
	// No smtp.host: no re-authentication.
	if rec := e.do(e.web, "POST", "/api/v1/agents/"+itoa64(a.ID)+"/command-sets", body, browserHdr(ck, true), "x:1"); rec.Code != 200 {
		t.Fatalf("create in development mode: %d %s", rec.Code, rec.Body.String())
	}
	rec := e.do(e.web, "POST", "/api/v1/auth/step-up", nil, browserHdr(ck, true), "x:1")
	if out := decode[map[string]any](t, rec); rec.Code != 200 || out["required"] != false {
		t.Fatalf("request in development mode: %d %v", rec.Code, out)
	}
	// The step-up routes are administrator routes.
	for _, p := range []string{"/api/v1/auth/step-up", "/api/v1/auth/step-up/verify"} {
		if rec := e.do(e.web, "POST", p, map[string]any{"code": "123456"}, browserHdr(viewer, true), "x:1"); rec.Code != 404 {
			t.Fatalf("viewer %s: %d", p, rec.Code)
		}
	}
}
