package workers

import (
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"mimamori/app/models"
	"mimamori/app/modules"
)

// verifyCookie remembers a browser in which a login code was entered. Its
// value is stored only hashed (browser_verifications.id). It is sent only to
// the login routes.
const verifyCookie = "mimamori_verify"

const verifyCookiePath = "/api/v1/auth/login"

// Answers of the code steps.
var (
	codeMailFailed   = modules.M("auth.code_mail_failed")
	invalidCode      = modules.M("auth.invalid_code")
	loginCodeExpired = modules.M("auth.login_code_expired")
	emailCodeExpired = modules.M("auth.email_code_expired")
)

// codeBinding is what a code hash is tied to (see modules.HashAuthCode).
func codeBinding(a *models.AuthCode) string {
	return a.Purpose + ":" + strconv.FormatInt(a.AccountID, 10) + ":" + a.ChallengeHash + ":" + a.SessionID + ":" + a.NewEmail
}

// codeMatches compares an entered code with the stored keyed hash.
func (c *core) codeMatches(a *models.AuthCode, code string) bool {
	got := modules.HashAuthCode(c.serverKey, codeBinding(a), strings.TrimSpace(code))
	return subtle.ConstantTimeCompare([]byte(got), []byte(a.CodeHash)) == 1
}

// sendCodeMail mails a verification code synchronously within the request,
// with the configuration's SMTP values unchanged: smtp.timeout_sec is the
// longest silence of the mail server, and the whole mail has no limit.
func (c *core) sendCodeMail(r *http.Request, to, code, purpose string) error {
	cfg := c.conf()
	subject := strings.TrimSpace(cfg.Notify.SubjectPrefix + " Verification code")
	body := fmt.Sprintf("Your verification code: %s\n\nPurpose: %s\nThe code is valid for %d minutes.\nRequested from: %s\nTime: %s\n\nIf you did not request this code, ignore this mail and tell your administrator.\n",
		code, purpose, int(modules.AuthCodeLifetime/time.Minute), remoteHost(r.RemoteAddr), time.Now().UTC().Format("2006-01-02 15:04:05")+" UTC")
	return c.sendMail(cfg.SMTP, []string{to}, subject, body)
}

// newAuthCode builds a code row with a fresh code; the caller sets the
// purpose-specific fields before calling seal.
func newAuthCode(r *http.Request, purpose string, accountID int64, now int64) *models.AuthCode {
	return &models.AuthCode{Purpose: purpose, AccountID: accountID, RemoteAddr: remoteHost(r.RemoteAddr), CreatedAt: now,
		ExpiresAt: now + int64(modules.AuthCodeLifetime/time.Second)}
}

// seal generates the code of a row and stores its keyed hash in the row.
func (c *core) seal(a *models.AuthCode) (string, error) {
	code, err := modules.GenerateNumericCode(modules.AuthCodeDigits)
	if err != nil {
		return "", err
	}
	a.CodeHash = modules.HashAuthCode(c.serverKey, codeBinding(a), code)
	return code, nil
}

// browserVerified reports whether the request comes from a browser in
// which the account entered a login code recently enough: within
// two_step_trust_days (the current setting) when the browser was trusted,
// within VerifiedBrowserTTL otherwise.
func (c *core) browserVerified(r *http.Request, accountID int64, cfg *modules.CentralConfig, now int64) bool {
	ck, err := r.Cookie(verifyCookie)
	if err != nil || !strings.HasPrefix(ck.Value, modules.VerifyPrefix) {
		return false
	}
	v, err := models.GetBrowserVerification(c.db.Read(), modules.HashSecret(ck.Value))
	if err != nil || v.AccountID != accountID || v.ExpiresAt <= now {
		return false
	}
	validFor := int64(modules.VerifiedBrowserTTL / time.Second)
	if v.Trusted {
		validFor = int64(cfg.TwoStepTrustDays) * 86400
	}
	return v.CreatedAt+validFor > now
}

// browserMemory is the record that a login code was entered in a browser:
// the row to insert, the hashed value of the browser's earlier verify
// cookie to delete ("" for none) and the new cookie.
type browserMemory struct {
	row    *models.BrowserVerification
	oldID  string
	cookie *http.Cookie
}

// newBrowserMemory builds the record that a login code was entered in
// this browser; write stores it, and the cookie is set once it is written.
// The cookie has the same Max-Age whether the browser is trusted or not;
// how long the row counts is decided only here on the server, so the
// browser shows nothing about it.
func (c *core) newBrowserMemory(r *http.Request, accountID int64, trusted bool, now int64) (*browserMemory, error) {
	m := &browserMemory{}
	if old, err := r.Cookie(verifyCookie); err == nil && old.Value != "" {
		m.oldID = modules.HashSecret(old.Value)
	}
	value, err := modules.GenerateSecret(modules.VerifyPrefix)
	if err != nil {
		return nil, err
	}
	// A trusted row is kept for the longest possible trust period so that a
	// longer two_step_trust_days also applies to it; browserVerified checks
	// the current setting.
	expires := now + int64(modules.VerifiedBrowserTTL/time.Second)
	if trusted {
		expires = now + int64(modules.MaxTrustDays)*86400
	}
	m.row = &models.BrowserVerification{ID: modules.HashSecret(value), AccountID: accountID, Trusted: trusted,
		CreatedAt: now, ExpiresAt: expires, RemoteAddr: remoteHost(r.RemoteAddr)}
	m.cookie = &http.Cookie{Name: verifyCookie, Value: value, Path: verifyCookiePath, MaxAge: c.conf().TwoStepTrustDays * 86400,
		HttpOnly: true, SameSite: http.SameSiteStrictMode}
	return m, nil
}

// write replaces the browser's earlier verification by the new one.
func (m *browserMemory) write(tx models.Writer) error {
	if m.oldID != "" {
		if err := models.DeleteBrowserVerification(tx, m.oldID); err != nil {
			return err
		}
	}
	return models.InsertBrowserVerification(tx, m.row)
}

// startLoginCode mails a login code after a correct password and answers
// 202 with the challenge the browser sends back with the code. When the
// mail cannot be sent the login fails (503) and nothing is stored.
func (c *core) startLoginCode(w http.ResponseWriter, r *http.Request, a *models.Account, remember bool, now int64) {
	// A new login replaces the codes of earlier attempts.
	_ = c.db.Write(func(tx models.Writer) error { return models.DeleteAccountAuthCodes(tx, a.ID, models.CodePurposeLogin) })
	challenge, err := modules.GenerateSecret(modules.ChallengePrefix)
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	row := newAuthCode(r, models.CodePurposeLogin, a.ID, now)
	row.ChallengeHash = modules.HashSecret(challenge)
	row.Remember = remember
	code, err := c.seal(row)
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	if err := c.sendCodeMail(r, a.Email, code, "login"); err != nil {
		c.history(&principal{Email: a.Email}, models.HistAccount, "login_code_failed", a.Email,
			modules.M("history.detail.login_code_failed", "host", remoteHost(r.RemoteAddr), "cause", modules.AsMsg(err)))
		writeError(w, http.StatusServiceUnavailable, codeMailFailed)
		return
	}
	if err := c.db.Write(func(tx models.Writer) error { return models.InsertAuthCode(tx, row) }); err != nil {
		c.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"two_step": true, "challenge": challenge})
}

// handleLoginVerify is the second login step: the mailed code. A wrong code
// counts toward the same failure counter and lock as a wrong password.
func (c *core) handleLoginVerify(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Challenge string `json:"challenge"`
		Code      string `json:"code"`
		Trust     bool   `json:"trust"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	now := time.Now().Unix()
	challenge := strings.TrimSpace(in.Challenge)
	if !strings.HasPrefix(challenge, modules.ChallengePrefix) {
		writeError(w, http.StatusGone, loginCodeExpired)
		return
	}
	row, err := models.GetLoginCodeByChallenge(c.db.Read(), modules.HashSecret(challenge))
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusGone, loginCodeExpired)
		return
	}
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	// The code row and the account are read on the read side: the code is
	// compared with its keyed hash before anything is written, and each
	// write below carries its own condition or reads the account again.
	a, err := models.GetAccount(c.db.Read(), row.AccountID)
	if row.ExpiresAt <= now || err != nil || a.Disabled {
		_ = c.db.Write(func(tx models.Writer) error { return models.DeleteAuthCode(tx, row.ID) })
		writeError(w, http.StatusGone, loginCodeExpired)
		return
	}
	if a.LockedUntil > now {
		writeError(w, http.StatusLocked, modules.M("auth.locked"))
		return
	}
	if !c.codeMatches(row, in.Code) {
		if n, err := c.recordCodeAttempt(row.ID); err != nil || n >= modules.AuthCodeMaxAttempts {
			_ = c.db.Write(func(tx models.Writer) error { return models.DeleteAuthCode(tx, row.ID) })
		}
		lock := now + int64(modules.LoginLockDuration/time.Second)
		var locked bool
		_ = c.db.Write(func(tx models.Writer) error {
			var err error
			locked, err = models.RecordLoginFailure(tx, a.ID, modules.LoginMaxFailures, lock)
			return err
		})
		if locked {
			_ = c.db.Write(func(tx models.Writer) error { return models.DeleteAuthCode(tx, row.ID) })
			c.history(&principal{Email: a.Email}, models.HistAccount, "login_locked", a.Email, modules.M("history.detail.login_locked", "host", remoteHost(r.RemoteAddr)))
			writeError(w, http.StatusLocked, modules.M("auth.locked"))
			return
		}
		writeError(w, http.StatusUnauthorized, invalidCode)
		return
	}
	// The code is used up atomically: a concurrent request with the same
	// code, or one that read the row before concurrent wrong entries
	// discarded it, gets 410.
	consumed, err := c.consumeCode(row.ID, now)
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	if !consumed {
		writeError(w, http.StatusGone, loginCodeExpired)
		return
	}
	memory, err := c.newBrowserMemory(r, row.AccountID, in.Trust, now)
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	s, cookie, err := c.newSession(row.AccountID, row.Remember)
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	// The login is completed in one write transaction that reads the
	// account again: concurrent wrong entries may have locked it (or an
	// administrator disabled it) after it was read above.
	err = c.db.Write(func(tx models.Writer) error {
		var err error
		if a, err = models.GetAccount(tx, row.AccountID); err != nil || a.Disabled {
			return &refusal{http.StatusGone, loginCodeExpired}
		}
		if a.LockedUntil > now {
			return &refusal{http.StatusLocked, modules.M("auth.locked")}
		}
		if err := memory.write(tx); err != nil {
			return err
		}
		return startSession(tx, s)
	})
	if c.failed(w, r, err) {
		return
	}
	detail := loginDetail(remoteHost(r.RemoteAddr), "history.detail.login_how.code")
	if in.Trust {
		detail = loginDetail(remoteHost(r.RemoteAddr), "history.detail.login_how.code_trust")
	}
	c.loggedIn(w, a, detail, memory.cookie, cookie)
}

// recordCodeAttempt counts a wrong entry of a code and returns the number
// of wrong entries (an error when the code no longer exists).
func (c *core) recordCodeAttempt(id int64) (int, error) {
	var n int
	err := c.db.Write(func(tx models.Writer) error {
		var err error
		n, err = models.RecordAuthCodeAttempt(tx, id)
		return err
	})
	return n, err
}

// consumeCode uses a code up; it reports false when the code was used up
// or discarded meanwhile. The statement carries its own condition, so a
// concurrent request with the same code, or one that read the row before
// concurrent wrong entries discarded it, gets false.
func (c *core) consumeCode(id, now int64) (bool, error) {
	var consumed bool
	err := c.db.Write(func(tx models.Writer) error {
		var err error
		consumed, err = models.ConsumeAuthCode(tx, id, modules.AuthCodeMaxAttempts, now)
		return err
	})
	return consumed, err
}
