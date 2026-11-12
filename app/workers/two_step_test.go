package workers

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"mimamori/app/models"
	"mimamori/app/modules"
)

// sentMail is one mail captured from core.sendMail.
type sentMail struct {
	cfg     modules.SMTPConfig
	to      []string
	subject string
	body    string
}

// mailbox replaces the mail function of the central and configures an SMTP
// server (host and sender), so the code steps are used. A non-nil fail makes
// every send fail with it.
type mailbox struct {
	sent []sentMail
	fail error
}

func useMailbox(e *testEnv) *mailbox {
	e.t.Helper()
	mb := &mailbox{}
	cfg := *e.c.conf()
	cfg.SMTP.Host = "smtp.example.com"
	cfg.SMTP.From = "mimamori@example.com"
	e.c.cfg.Store(&cfg)
	e.c.sendMail = func(c modules.SMTPConfig, to []string, subject, body string) error {
		if mb.fail != nil {
			return mb.fail
		}
		mb.sent = append(mb.sent, sentMail{cfg: c, to: to, subject: subject, body: body})
		return nil
	}
	return mb
}

var codeRe = regexp.MustCompile(`code: (\d{6})`)

// lastCode returns the code of the last captured mail.
func (mb *mailbox) lastCode(t *testing.T) string {
	t.Helper()
	if len(mb.sent) == 0 {
		t.Fatal("no mail sent")
	}
	m := codeRe.FindStringSubmatch(mb.sent[len(mb.sent)-1].body)
	if m == nil {
		t.Fatalf("no code in %q", mb.sent[len(mb.sent)-1].body)
	}
	return m[1]
}

// wrongCode returns a 6-digit code different from code.
func wrongCode(code string) string {
	if code == "000000" {
		return "000001"
	}
	return "000000"
}

func setConfig(e *testEnv, f func(*modules.CentralConfig)) {
	cfg := *e.c.conf()
	f(&cfg)
	e.c.cfg.Store(&cfg)
}

func cookieOf(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// loginStart sends the password step; verify is the value of the verify
// cookie to send ("" for none).
func loginStart(e *testEnv, email, pass, verify string, remember bool) *httptest.ResponseRecorder {
	hdr := map[string]string{csrfHeader: "1"}
	if verify != "" {
		hdr["Cookie"] = verifyCookie + "=" + verify
	}
	return e.do(e.web, "POST", "/api/v1/auth/login", map[string]any{"email": email, "password": pass, "remember": remember}, hdr, "192.0.2.1:1")
}

func loginVerify(e *testEnv, challenge, code string, trust bool, verify string) *httptest.ResponseRecorder {
	hdr := map[string]string{csrfHeader: "1"}
	if verify != "" {
		hdr["Cookie"] = verifyCookie + "=" + verify
	}
	return e.do(e.web, "POST", "/api/v1/auth/login/verify", map[string]any{"challenge": challenge, "code": code, "trust": trust}, hdr, "192.0.2.1:1")
}

// challengeOf checks a 202 answer and returns its challenge.
func challengeOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if rec.Code != http.StatusAccepted {
		t.Fatalf("password step: %d %s, want 202", rec.Code, rec.Body.String())
	}
	out := decode[map[string]any](t, rec)
	ch, _ := out["challenge"].(string)
	if out["two_step"] != true || ch == "" {
		t.Fatalf("202 body: %v", out)
	}
	return ch
}

// twoStepLogin logs in with the code and returns the session and verify
// cookies.
func twoStepLogin(t *testing.T, e *testEnv, mb *mailbox, email, pass string, trust bool) (session, verify *http.Cookie) {
	t.Helper()
	ch := challengeOf(t, loginStart(e, email, pass, "", false))
	rec := loginVerify(e, ch, mb.lastCode(t), trust, "")
	if rec.Code != 200 {
		t.Fatalf("verify: %d %s", rec.Code, rec.Body.String())
	}
	session, verify = cookieOf(rec, sessionCookie), cookieOf(rec, verifyCookie)
	if session == nil || verify == nil {
		t.Fatal("verify must set the session and verify cookies")
	}
	return session, verify
}

func countAll(t *testing.T, e *testEnv, table string) int {
	t.Helper()
	var n int
	if err := e.c.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// noFixedPeriods fails when a text a browser receives states the idle
// timeout or the verified-browser period.
func noFixedPeriods(t *testing.T, what, s string) {
	t.Helper()
	for _, bad := range []string{"3600", "10800", "hour", "1h", "3h"} {
		if strings.Contains(strings.ToLower(s), bad) {
			t.Fatalf("%s states a fixed period (%q): %s", what, bad, s)
		}
	}
}

func TestTwoStepLogin(t *testing.T) {
	e := newTestEnv(t)
	mb := useMailbox(e)

	rec := loginStart(e, testAdminEmail, "Admin-pass-123", "", false)
	ch := challengeOf(t, rec)
	if cookieOf(rec, sessionCookie) != nil {
		t.Fatal("the password step must not start a session")
	}
	if countAll(t, e, "sessions") != 0 || countAll(t, e, "auth_codes") != 1 {
		t.Fatal("the password step must store one code and no session")
	}
	// The mail goes to the account's address and states what it is for.
	m := mb.sent[0]
	code := mb.lastCode(t)
	if len(m.to) != 1 || m.to[0] != testAdminEmail || m.subject != "[mimamori] Verification code" {
		t.Fatalf("mail: %v %q", m.to, m.subject)
	}
	for _, want := range []string{"Purpose: login", "valid for 10 minutes", "192.0.2.1", " UTC"} {
		if !strings.Contains(m.body, want) {
			t.Fatalf("mail body lacks %q: %s", want, m.body)
		}
	}
	// The challenge and the code are stored only hashed.
	var chHash, codeHash string
	if err := e.c.db.QueryRow(`SELECT challenge_hash, code_hash FROM auth_codes`).Scan(&chHash, &codeHash); err != nil {
		t.Fatal(err)
	}
	if chHash != modules.HashSecret(ch) || strings.Contains(codeHash, code) || codeHash == modules.HashSecret(code) {
		t.Fatalf("stored values: %q %q", chHash, codeHash)
	}

	// A wrong code is a 401 and counts as a failed login.
	rec = loginVerify(e, ch, wrongCode(code), false, "")
	if rec.Code != 401 || !strings.Contains(rec.Body.String(), "invalid code") {
		t.Fatalf("wrong code: %d %s", rec.Code, rec.Body.String())
	}
	if a, _ := models.GetAccount(e.c.db, 1); a.FailedLogins != 1 {
		t.Fatalf("failed logins after a wrong code: %d", a.FailedLogins)
	}

	// The right code logs in; the counter is cleared and the code is used up.
	rec = loginVerify(e, ch, code, false, "")
	if rec.Code != 200 {
		t.Fatalf("verify: %d %s", rec.Code, rec.Body.String())
	}
	me := decode[map[string]any](t, rec)
	if me["email"] != testAdminEmail || me["display_name"] != "Admin" || me["role"] != "admin" {
		t.Fatalf("Me: %v", me)
	}
	ck := cookieOf(rec, sessionCookie)
	if ck == nil || ck.MaxAge != 0 {
		t.Fatalf("session cookie: %v", ck)
	}
	if code := e.do(e.web, "GET", "/api/v1/auth/me", nil, browserHeaders(ck.Value), "x:1").Code; code != 200 {
		t.Fatalf("session after verify: %d", code)
	}
	if a, _ := models.GetAccount(e.c.db, 1); a.FailedLogins != 0 {
		t.Fatalf("failed logins after login: %d", a.FailedLogins)
	}
	if countAll(t, e, "auth_codes") != 0 {
		t.Fatal("a used code must be deleted")
	}
	if rec := loginVerify(e, ch, code, false, ""); rec.Code != http.StatusGone {
		t.Fatalf("used challenge: %d", rec.Code)
	}
	items := e.admin("GET", "/history?category=account&limit=1", nil)["items"].([]any)
	if h := items[0].(map[string]any); h["action"] != "login" || h["detail"] != "from 192.0.2.1; code verified" {
		t.Fatalf("history: %v", h)
	}
}

func TestTwoStepLoginRememberAndReplace(t *testing.T) {
	e := newTestEnv(t)
	mb := useMailbox(e)
	// "Keep me logged in" chosen in the password step applies after the code.
	ch := challengeOf(t, loginStart(e, testAdminEmail, "Admin-pass-123", "", true))
	rec := loginVerify(e, ch, mb.lastCode(t), false, "")
	if ck := cookieOf(rec, sessionCookie); rec.Code != 200 || ck == nil || ck.MaxAge != modules.DefaultRememberDays*86400 {
		t.Fatalf("remembered login: %d %v", rec.Code, ck)
	}

	// A new password step replaces the code of the earlier one.
	first := challengeOf(t, loginStart(e, testAdminEmail, "Admin-pass-123", "", false))
	firstCode := mb.lastCode(t)
	second := challengeOf(t, loginStart(e, testAdminEmail, "Admin-pass-123", "", false))
	if countAll(t, e, "auth_codes") != 1 {
		t.Fatal("old login codes must be replaced")
	}
	if rec := loginVerify(e, first, firstCode, false, ""); rec.Code != http.StatusGone {
		t.Fatalf("replaced challenge: %d", rec.Code)
	}
	// Unknown and expired challenges are 410.
	if rec := loginVerify(e, "mmk_unknown", "123456", false, ""); rec.Code != http.StatusGone {
		t.Fatalf("unknown challenge: %d", rec.Code)
	}
	if rec := loginVerify(e, "garbage", "123456", false, ""); rec.Code != http.StatusGone {
		t.Fatalf("malformed challenge: %d", rec.Code)
	}
	if _, err := e.c.db.Exec(`UPDATE auth_codes SET expires_at = ?`, time.Now().Unix()-1); err != nil {
		t.Fatal(err)
	}
	rec = loginVerify(e, second, mb.lastCode(t), false, "")
	if rec.Code != http.StatusGone || !strings.Contains(rec.Body.String(), "log in again") {
		t.Fatalf("expired challenge: %d %s", rec.Code, rec.Body.String())
	}
	if countAll(t, e, "auth_codes") != 0 {
		t.Fatal("an expired code must be deleted")
	}
}

func TestTwoStepLoginLock(t *testing.T) {
	e := newTestEnv(t)
	mb := useMailbox(e)
	ch := challengeOf(t, loginStart(e, testAdminEmail, "Admin-pass-123", "", false))
	code := mb.lastCode(t)
	for i := 1; i < modules.LoginMaxFailures; i++ {
		if rec := loginVerify(e, ch, wrongCode(code), false, ""); rec.Code != 401 {
			t.Fatalf("wrong code %d: %d", i, rec.Code)
		}
	}
	if rec := loginVerify(e, ch, wrongCode(code), false, ""); rec.Code != http.StatusLocked {
		t.Fatalf("fifth wrong code: %d, want 423", rec.Code)
	}
	if rec := loginStart(e, testAdminEmail, "Admin-pass-123", "", false); rec.Code != http.StatusLocked {
		t.Fatalf("password step while locked: %d", rec.Code)
	}
	if countAll(t, e, "auth_codes") != 0 || countAll(t, e, "sessions") != 0 {
		t.Fatal("a lock must leave no code and no session")
	}

	// Wrong passwords and wrong codes share one counter.
	if err := models.ResetLoginFailures(e.c.db, 1); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < modules.LoginMaxFailures; i++ {
		if rec := loginStart(e, testAdminEmail, "wrong-password", "", false); rec.Code != 401 {
			t.Fatalf("wrong password %d: %d", i, rec.Code)
		}
	}
	ch = challengeOf(t, loginStart(e, testAdminEmail, "Admin-pass-123", "", false))
	if rec := loginVerify(e, ch, wrongCode(mb.lastCode(t)), false, ""); rec.Code != http.StatusLocked {
		t.Fatalf("wrong code after wrong passwords: %d, want 423", rec.Code)
	}
	items := e.admin("GET", "/history?category=account&limit=1", nil)["items"].([]any)
	if h := items[0].(map[string]any); h["action"] != "login_locked" {
		t.Fatalf("history: %v", h)
	}
}

func TestTwoStepLoginMailFailure(t *testing.T) {
	e := newTestEnv(t)
	mb := useMailbox(e)
	mb.fail = errors.New("connection refused")
	rec := loginStart(e, testAdminEmail, "Admin-pass-123", "", false)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "could not be sent; contact an administrator") {
		t.Fatalf("mail failure: %d %s", rec.Code, rec.Body.String())
	}
	if cookieOf(rec, sessionCookie) != nil || countAll(t, e, "sessions") != 0 || countAll(t, e, "auth_codes") != 0 {
		t.Fatal("a failed code mail must create nothing")
	}
	items := e.admin("GET", "/history?category=account&limit=1", nil)["items"].([]any)
	if h := items[0].(map[string]any); h["action"] != "login_code_failed" || h["target"] != testAdminEmail || !strings.Contains(h["detail"].(string), "connection refused") {
		t.Fatalf("history: %v", h)
	}

	// With a host but no sender the real mail function fails too.
	e.c.sendMail = modules.SendMailTo
	setConfig(e, func(c *modules.CentralConfig) { c.SMTP.From = "" })
	if rec := loginStart(e, testAdminEmail, "Admin-pass-123", "", false); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no sender: %d", rec.Code)
	}

	// Development mode (no host): no code, logged in at once.
	setConfig(e, func(c *modules.CentralConfig) { c.SMTP.Host = "" })
	rec = loginStart(e, testAdminEmail, "Admin-pass-123", "", false)
	if rec.Code != 200 || cookieOf(rec, sessionCookie) == nil || cookieOf(rec, verifyCookie) != nil {
		t.Fatalf("development mode: %d %s", rec.Code, rec.Body.String())
	}
	items = e.admin("GET", "/history?category=account&limit=1", nil)["items"].([]any)
	if h := items[0].(map[string]any); h["action"] != "login" || !strings.Contains(h["detail"].(string), "SMTP not configured") {
		t.Fatalf("history: %v", h)
	}
}

func TestCodeMailTimeLimit(t *testing.T) {
	e := newTestEnv(t)
	mb := useMailbox(e)
	setConfig(e, func(c *modules.CentralConfig) { c.SMTP.TimeoutSec = 300 })
	challengeOf(t, loginStart(e, testAdminEmail, "Admin-pass-123", "", false))
	setConfig(e, func(c *modules.CentralConfig) { c.SMTP.TimeoutSec = 10 })
	challengeOf(t, loginStart(e, testAdminEmail, "Admin-pass-123", "", false))
	if mb.sent[0].cfg.TimeoutSec != int(modules.AuthCodeMailTimeoutCap/time.Second) || mb.sent[1].cfg.TimeoutSec != 10 {
		t.Fatalf("time limits: %d %d", mb.sent[0].cfg.TimeoutSec, mb.sent[1].cfg.TimeoutSec)
	}
}

func TestVerifiedBrowser(t *testing.T) {
	e := newTestEnv(t)
	mb := useMailbox(e)
	e.admin("POST", "/accounts", map[string]any{"email": "bob@example.com", "display_name": "Bob", "role": "viewer", "password": "Viewer-pass-123"})

	// Not trusted: the cookie has the same Max-Age as a trusted one and
	// nothing the browser receives states the server-side period.
	ch := challengeOf(t, loginStart(e, testAdminEmail, "Admin-pass-123", "", false))
	rec := loginVerify(e, ch, mb.lastCode(t), false, "")
	vc := cookieOf(rec, verifyCookie)
	if rec.Code != 200 || vc == nil || vc.MaxAge != modules.DefaultTrustDays*86400 || !vc.HttpOnly || vc.SameSite != http.SameSiteStrictMode ||
		vc.Path != "/api/v1/auth/login" || !strings.HasPrefix(vc.Value, modules.VerifyPrefix) {
		t.Fatalf("verify cookie: %d %+v", rec.Code, vc)
	}
	noFixedPeriods(t, "verify response", rec.Body.String())
	for _, h := range rec.Result().Header.Values("Set-Cookie") {
		// The attributes after the (random) value.
		_, attrs, _ := strings.Cut(h, ";")
		noFixedPeriods(t, "Set-Cookie", strings.ReplaceAll(attrs, strconv.Itoa(modules.DefaultTrustDays*86400), ""))
	}
	var stored string
	if err := e.c.db.QueryRow(`SELECT id FROM browser_verifications`).Scan(&stored); err != nil || stored != modules.HashSecret(vc.Value) {
		t.Fatalf("stored verification: %q %v", stored, err)
	}
	// Within the period the same browser logs in without a code.
	sent := len(mb.sent)
	rec = loginStart(e, testAdminEmail, "Admin-pass-123", vc.Value, false)
	if rec.Code != 200 || cookieOf(rec, sessionCookie) == nil || len(mb.sent) != sent {
		t.Fatalf("verified browser: %d %s", rec.Code, rec.Body.String())
	}
	items := e.admin("GET", "/history?category=account&limit=1", nil)["items"].([]any)
	if h := items[0].(map[string]any); h["detail"] != "from 192.0.2.1; verified browser" {
		t.Fatalf("history: %v", h)
	}
	// Another account with the same cookie needs a code.
	challengeOf(t, loginStart(e, "bob@example.com", "Viewer-pass-123", vc.Value, false))
	// A wrong password is still refused.
	if rec := loginStart(e, testAdminEmail, "wrong-password", vc.Value, false); rec.Code != 401 {
		t.Fatalf("wrong password with a verified browser: %d", rec.Code)
	}
	// After the server-side period a code is needed again.
	if _, err := e.c.db.Exec(`UPDATE browser_verifications SET created_at = created_at - ?`, int64(modules.VerifiedBrowserTTL/time.Second)+1); err != nil {
		t.Fatal(err)
	}
	challengeOf(t, loginStart(e, testAdminEmail, "Admin-pass-123", vc.Value, false))

	// Trusted: valid for two_step_trust_days by the current setting.
	ch = challengeOf(t, loginStart(e, testAdminEmail, "Admin-pass-123", vc.Value, false))
	rec = loginVerify(e, ch, mb.lastCode(t), true, vc.Value)
	tc := cookieOf(rec, verifyCookie)
	if rec.Code != 200 || tc == nil || tc.MaxAge != modules.DefaultTrustDays*86400 {
		t.Fatalf("trusted cookie: %d %+v", rec.Code, tc)
	}
	if countAll(t, e, "browser_verifications") != 1 {
		t.Fatal("the browser's earlier verification must be replaced")
	}
	items = e.admin("GET", "/history?category=account&limit=1", nil)["items"].([]any)
	if h := items[0].(map[string]any); h["detail"] != "from 192.0.2.1; code verified, browser trusted" {
		t.Fatalf("history: %v", h)
	}
	if _, err := e.c.db.Exec(`UPDATE browser_verifications SET created_at = created_at - ?`, 2*86400); err != nil {
		t.Fatal(err)
	}
	if rec := loginStart(e, testAdminEmail, "Admin-pass-123", tc.Value, false); rec.Code != 200 {
		t.Fatalf("trusted browser after two days: %d", rec.Code)
	}
	setConfig(e, func(c *modules.CentralConfig) { c.TwoStepTrustDays = 1 })
	challengeOf(t, loginStart(e, testAdminEmail, "Admin-pass-123", tc.Value, false))

	// A password change revokes the verification.
	setConfig(e, func(c *modules.CentralConfig) { c.TwoStepTrustDays = 30 })
	_, tv := twoStepLogin(t, e, mb, testAdminEmail, "Admin-pass-123", true)
	if rec := loginStart(e, testAdminEmail, "Admin-pass-123", tv.Value, false); rec.Code != 200 {
		t.Fatalf("trusted browser: %d", rec.Code)
	}
	e.admin("PUT", "/accounts/1/password", map[string]any{"password": "Admin-pass-456"})
	challengeOf(t, loginStart(e, testAdminEmail, "Admin-pass-456", tv.Value, false))

	// The health answer gives the trust days only.
	rec = e.do(e.web, "GET", "/api/v1/health", nil, nil, "x:1")
	noFixedPeriods(t, "health", rec.Body.String())
}

func TestOwnPasswordChangeRevokesVerifications(t *testing.T) {
	e := newTestEnv(t)
	mb := useMailbox(e)
	sess, vc := twoStepLogin(t, e, mb, testAdminEmail, "Admin-pass-123", true)
	other, _ := twoStepLogin(t, e, mb, testAdminEmail, "Admin-pass-123", false)
	challengeOf(t, loginStart(e, testAdminEmail, "Admin-pass-123", "", false))
	rec := e.do(e.web, "POST", "/api/v1/auth/password", map[string]any{"current_password": "Admin-pass-123", "new_password": "Admin-pass-456"}, browserHeaders(sess.Value), "x:1")
	if rec.Code != 200 {
		t.Fatalf("own password: %d %s", rec.Code, rec.Body.String())
	}
	if countRows(t, e, "browser_verifications", 1) != 0 || countRows(t, e, "auth_codes", 1) != 0 {
		t.Fatal("an own password change must revoke the verifications and codes")
	}
	if e.do(e.web, "GET", "/api/v1/auth/me", nil, browserHeaders(sess.Value), "x:1").Code != 200 ||
		e.do(e.web, "GET", "/api/v1/auth/me", nil, browserHeaders(other.Value), "x:1").Code != 401 {
		t.Fatal("an own password change keeps only the own session")
	}
	challengeOf(t, loginStart(e, testAdminEmail, "Admin-pass-456", vc.Value, false))
}

func TestOwnDisplayName(t *testing.T) {
	e := newTestEnv(t)
	ck := login(t, e, testAdminEmail, "Admin-pass-123")
	addAuthState(t, e, 1, "x")
	rec := e.do(e.web, "PUT", "/api/v1/auth/display-name", map[string]any{"display_name": "  Admin Two "}, browserHeaders(ck), "x:1")
	if rec.Code != 200 {
		t.Fatalf("own display name: %d %s", rec.Code, rec.Body.String())
	}
	if me := decode[map[string]any](t, rec); me["display_name"] != "Admin Two" || me["email"] != testAdminEmail {
		t.Fatalf("Me: %v", me)
	}
	if me := decode[map[string]any](t, e.do(e.web, "GET", "/api/v1/auth/me", nil, browserHeaders(ck), "x:1")); me["display_name"] != "Admin Two" {
		t.Fatalf("me after change: %v", me)
	}
	if countRows(t, e, "browser_verifications", 1) != 1 || countRows(t, e, "auth_codes", 1) != 1 {
		t.Fatal("a display name change must not revoke anything")
	}
	if rec := e.do(e.web, "PUT", "/api/v1/auth/display-name", map[string]any{"display_name": " "}, browserHeaders(ck), "x:1"); rec.Code != 400 {
		t.Fatalf("empty display name: %d", rec.Code)
	}
	items := e.admin("GET", "/history?category=account&limit=1", nil)["items"].([]any)
	if h := items[0].(map[string]any); h["action"] != "display_name_changed" || h["account_name"] != testAdminEmail || h["detail"] != "Admin -> Admin Two (own change)" {
		t.Fatalf("history: %v", h)
	}
	// The management API has no own routes.
	if code := e.adminStatus("PUT", "/auth/display-name", map[string]any{"display_name": "x"}); code != 404 && code != 405 {
		t.Fatalf("CLI own display name: %d", code)
	}
}

func TestOwnEmailChange(t *testing.T) {
	e := newTestEnv(t)
	mb := useMailbox(e)
	e.admin("POST", "/accounts", map[string]any{"email": "bob@example.com", "display_name": "Bob", "role": "viewer", "password": "Viewer-pass-123"})
	sess, vc := twoStepLogin(t, e, mb, testAdminEmail, "Admin-pass-123", true)
	other, _ := twoStepLogin(t, e, mb, testAdminEmail, "Admin-pass-123", false)
	h := browserHeaders(sess.Value)
	post := func(path string, body any, hdr map[string]string) *httptest.ResponseRecorder {
		return e.do(e.web, "POST", "/api/v1"+path, body, hdr, "192.0.2.7:1")
	}

	// Invalid, unchanged and duplicate addresses.
	for _, c := range []struct {
		email string
		want  int
	}{{"not an address", 400}, {testAdminEmail, 400}, {"BOB@example.com", 409}} {
		if rec := post("/auth/email", map[string]any{"email": c.email}, h); rec.Code != c.want {
			t.Fatalf("%q: %d, want %d", c.email, rec.Code, c.want)
		}
	}
	// No current password is asked for.
	if rec := post("/auth/email", map[string]any{"email": "x@example.com", "current_password": "Admin-pass-123"}, h); rec.Code != 400 {
		t.Fatalf("current_password is not an input: %d", rec.Code)
	}
	if rec := post("/auth/email/verify", map[string]any{"code": "123456"}, h); rec.Code != http.StatusGone {
		t.Fatalf("verify without a request: %d", rec.Code)
	}

	// The code goes to the new address; nothing changes before it is entered.
	sent := len(mb.sent)
	rec := post("/auth/email", map[string]any{"email": " new@example.com "}, h)
	if rec.Code != http.StatusAccepted || decode[map[string]any](t, rec)["applied"] != false {
		t.Fatalf("request: %d %s", rec.Code, rec.Body.String())
	}
	m := mb.sent[sent]
	if len(m.to) != 1 || m.to[0] != "new@example.com" || !strings.Contains(m.body, "Purpose: e-mail address change") || !strings.Contains(m.body, "192.0.2.7") {
		t.Fatalf("mail: %v %s", m.to, m.body)
	}
	code := mb.lastCode(t)
	if me := decode[map[string]any](t, e.do(e.web, "GET", "/api/v1/auth/me", nil, h, "x:1")); me["email"] != testAdminEmail {
		t.Fatalf("address changed before the code: %v", me)
	}
	// A wrong code is a 400 (never 401) and the session stays.
	if rec := post("/auth/email/verify", map[string]any{"code": wrongCode(code)}, h); rec.Code != 400 || !strings.Contains(rec.Body.String(), "invalid code") {
		t.Fatalf("wrong code: %d %s", rec.Code, rec.Body.String())
	}
	if e.do(e.web, "GET", "/api/v1/auth/me", nil, h, "x:1").Code != 200 {
		t.Fatal("a wrong code must keep the session")
	}
	if a, _ := models.GetAccount(e.c.db, 1); a.FailedLogins != 0 {
		t.Fatal("a wrong address change code must not count toward the login lock")
	}
	// Another session of the same account cannot confirm it.
	if rec := post("/auth/email/verify", map[string]any{"code": code}, browserHeaders(other.Value)); rec.Code != http.StatusGone {
		t.Fatalf("verify from another session: %d", rec.Code)
	}

	// The right code changes the address and revokes everything but this
	// session.
	rec = post("/auth/email/verify", map[string]any{"code": code}, h)
	if rec.Code != 200 {
		t.Fatalf("verify: %d %s", rec.Code, rec.Body.String())
	}
	if me := decode[map[string]any](t, rec); me["email"] != "new@example.com" {
		t.Fatalf("Me: %v", me)
	}
	if e.do(e.web, "GET", "/api/v1/auth/me", nil, h, "x:1").Code != 200 || e.do(e.web, "GET", "/api/v1/auth/me", nil, browserHeaders(other.Value), "x:1").Code != 401 {
		t.Fatal("the confirmed change keeps only the own session")
	}
	if countRows(t, e, "browser_verifications", 1) != 0 || countRows(t, e, "auth_codes", 1) != 0 {
		t.Fatal("the confirmed change must revoke the verifications and codes")
	}
	items := e.admin("GET", "/history?category=account&limit=1", nil)["items"].([]any)
	if hi := items[0].(map[string]any); hi["action"] != "email_changed" || hi["account_name"] != testAdminEmail || hi["target"] != "new@example.com" ||
		hi["detail"] != testAdminEmail+" -> new@example.com (own change)" {
		t.Fatalf("history: %v", hi)
	}
	if rec := loginStart(e, testAdminEmail, "Admin-pass-123", "", false); rec.Code != 401 {
		t.Fatalf("old address: %d", rec.Code)
	}
	challengeOf(t, loginStart(e, "new@example.com", "Admin-pass-123", vc.Value, false))

	// Five wrong codes discard the request.
	post("/auth/email", map[string]any{"email": "third@example.com"}, h)
	code = mb.lastCode(t)
	for i := 1; i < modules.AuthCodeMaxAttempts; i++ {
		if rec := post("/auth/email/verify", map[string]any{"code": wrongCode(code)}, h); rec.Code != 400 {
			t.Fatalf("wrong code %d: %d", i, rec.Code)
		}
	}
	if rec := post("/auth/email/verify", map[string]any{"code": wrongCode(code)}, h); rec.Code != http.StatusGone {
		t.Fatalf("fifth wrong code: %d", rec.Code)
	}
	if rec := post("/auth/email/verify", map[string]any{"code": code}, h); rec.Code != http.StatusGone {
		t.Fatalf("code after the request was discarded: %d", rec.Code)
	}
	// An expired request is 410.
	post("/auth/email", map[string]any{"email": "third@example.com"}, h)
	code = mb.lastCode(t)
	if _, err := e.c.db.Exec(`UPDATE auth_codes SET expires_at = ?`, time.Now().Unix()-1); err != nil {
		t.Fatal(err)
	}
	if rec := post("/auth/email/verify", map[string]any{"code": code}, h); rec.Code != http.StatusGone {
		t.Fatalf("expired request: %d", rec.Code)
	}
	// A new request replaces an earlier one.
	post("/auth/email", map[string]any{"email": "fourth@example.com"}, h)
	firstCode := mb.lastCode(t)
	post("/auth/email", map[string]any{"email": "fifth@example.com"}, h)
	var pending int
	if err := e.c.db.QueryRow(`SELECT COUNT(*) FROM auth_codes WHERE purpose = 'email_change'`).Scan(&pending); err != nil || pending != 1 {
		t.Fatalf("a new request must replace the earlier one: %d %v", pending, err)
	}
	if firstCode != mb.lastCode(t) {
		if rec := post("/auth/email/verify", map[string]any{"code": firstCode}, h); rec.Code != 400 {
			t.Fatalf("code of the replaced request: %d", rec.Code)
		}
	}
	// A failed mail is a 503 and stores nothing.
	mb.fail = errors.New("mailbox unavailable")
	if rec := post("/auth/email", map[string]any{"email": "sixth@example.com"}, h); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("mail failure: %d", rec.Code)
	}
	if err := e.c.db.QueryRow(`SELECT COUNT(*) FROM auth_codes WHERE purpose = 'email_change'`).Scan(&pending); err != nil || pending != 0 {
		t.Fatal("a failed code mail must store nothing")
	}
	items = e.admin("GET", "/history?category=account&limit=1", nil)["items"].([]any)
	if hi := items[0].(map[string]any); hi["action"] != "email_code_failed" {
		t.Fatalf("history: %v", hi)
	}
}

func TestOwnEmailChangeDevelopmentMode(t *testing.T) {
	e := newTestEnv(t)
	sess := login(t, e, testAdminEmail, "Admin-pass-123")
	other := login(t, e, testAdminEmail, "Admin-pass-123")
	addAuthState(t, e, 1, "x")
	rec := e.do(e.web, "POST", "/api/v1/auth/email", map[string]any{"email": "dev@example.com"}, browserHeaders(sess), "x:1")
	if rec.Code != 200 || decode[map[string]any](t, rec)["applied"] != true {
		t.Fatalf("development mode: %d %s", rec.Code, rec.Body.String())
	}
	if me := decode[map[string]any](t, e.do(e.web, "GET", "/api/v1/auth/me", nil, browserHeaders(sess), "x:1")); me["email"] != "dev@example.com" {
		t.Fatalf("me: %v", me)
	}
	if e.do(e.web, "GET", "/api/v1/auth/me", nil, browserHeaders(other), "x:1").Code != 401 {
		t.Fatal("the change must end the other sessions")
	}
	if countRows(t, e, "browser_verifications", 1) != 0 || countRows(t, e, "auth_codes", 1) != 0 {
		t.Fatal("the change must revoke the verifications and codes")
	}
	login(t, e, "dev@example.com", "Admin-pass-123")
}
