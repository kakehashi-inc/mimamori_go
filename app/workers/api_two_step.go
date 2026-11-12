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
const (
	codeMailFailed   = "the verification code could not be sent; contact an administrator"
	invalidCode      = "invalid code"
	loginCodeExpired = "the verification code has expired; log in again"
	emailCodeExpired = "the verification code has expired or was entered wrongly too often; request the change again"
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

// sendCodeMail mails a verification code synchronously within the request.
// The time limit is smtp.timeout_sec, at most AuthCodeMailTimeoutCap, so
// that the response is still written in time.
func (c *core) sendCodeMail(r *http.Request, to, code, purpose string) error {
	cfg := c.conf()
	smtp := cfg.SMTP
	limit := int(modules.AuthCodeMailTimeoutCap / time.Second)
	if smtp.TimeoutSec <= 0 || smtp.TimeoutSec > limit {
		smtp.TimeoutSec = limit
	}
	subject := strings.TrimSpace(cfg.Notify.SubjectPrefix + " Verification code")
	body := fmt.Sprintf("Your verification code: %s\n\nPurpose: %s\nThe code is valid for %d minutes.\nRequested from: %s\nTime: %s\n\nIf you did not request this code, ignore this mail and tell your administrator.\n",
		code, purpose, int(modules.AuthCodeLifetime/time.Minute), remoteHost(r.RemoteAddr), time.Now().UTC().Format("2006-01-02 15:04:05")+" UTC")
	return c.sendMail(smtp, []string{to}, subject, body)
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
	v, err := models.GetBrowserVerification(c.db, modules.HashSecret(ck.Value))
	if err != nil || v.AccountID != accountID || v.ExpiresAt <= now {
		return false
	}
	validFor := int64(modules.VerifiedBrowserTTL / time.Second)
	if v.Trusted {
		validFor = int64(cfg.TwoStepTrustDays) * 86400
	}
	return v.CreatedAt+validFor > now
}

// rememberBrowser records that a login code was entered in this browser and
// sets the verify cookie. The cookie has the same Max-Age whether the
// browser is trusted or not; how long the row counts is decided only here
// on the server, so the browser shows nothing about it.
func (c *core) rememberBrowser(w http.ResponseWriter, r *http.Request, accountID int64, trusted bool, now int64) error {
	if old, err := r.Cookie(verifyCookie); err == nil && old.Value != "" {
		_ = models.DeleteBrowserVerification(c.db, modules.HashSecret(old.Value))
	}
	value, err := modules.GenerateSecret(modules.VerifyPrefix)
	if err != nil {
		return err
	}
	// A trusted row is kept for the longest possible trust period so that a
	// longer two_step_trust_days also applies to it; browserVerified checks
	// the current setting.
	expires := now + int64(modules.VerifiedBrowserTTL/time.Second)
	if trusted {
		expires = now + int64(modules.MaxTrustDays)*86400
	}
	if err := models.InsertBrowserVerification(c.db, &models.BrowserVerification{ID: modules.HashSecret(value), AccountID: accountID, Trusted: trusted,
		CreatedAt: now, ExpiresAt: expires, RemoteAddr: remoteHost(r.RemoteAddr)}); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{Name: verifyCookie, Value: value, Path: verifyCookiePath, MaxAge: c.conf().TwoStepTrustDays * 86400,
		HttpOnly: true, SameSite: http.SameSiteStrictMode})
	return nil
}

// startLoginCode mails a login code after a correct password and answers
// 202 with the challenge the browser sends back with the code. When the
// mail cannot be sent the login fails (503) and nothing is stored.
func (c *core) startLoginCode(w http.ResponseWriter, r *http.Request, a *models.Account, remember bool, now int64) {
	// A new login replaces the codes of earlier attempts.
	_ = models.DeleteAccountAuthCodes(c.db, a.ID, models.CodePurposeLogin)
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
			"the login code for a login from "+remoteHost(r.RemoteAddr)+" could not be sent: "+err.Error())
		writeError(w, http.StatusServiceUnavailable, codeMailFailed)
		return
	}
	if err := models.InsertAuthCode(c.db, row); err != nil {
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
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	now := time.Now().Unix()
	challenge := strings.TrimSpace(in.Challenge)
	if !strings.HasPrefix(challenge, modules.ChallengePrefix) {
		writeError(w, http.StatusGone, loginCodeExpired)
		return
	}
	row, err := models.GetLoginCodeByChallenge(c.db, modules.HashSecret(challenge))
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusGone, loginCodeExpired)
		return
	}
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	a, err := models.GetAccount(c.db, row.AccountID)
	if row.ExpiresAt <= now || err != nil || a.Disabled {
		_ = models.DeleteAuthCode(c.db, row.ID)
		writeError(w, http.StatusGone, loginCodeExpired)
		return
	}
	if a.LockedUntil > now {
		writeError(w, http.StatusLocked, "login is temporarily locked after repeated failures")
		return
	}
	if !c.codeMatches(row, in.Code) {
		if n, err := models.RecordAuthCodeAttempt(c.db, row.ID); err != nil || n >= modules.AuthCodeMaxAttempts {
			_ = models.DeleteAuthCode(c.db, row.ID)
		}
		lock := now + int64(modules.LoginLockDuration/time.Second)
		if locked, _ := models.RecordLoginFailure(c.db, a.ID, modules.LoginMaxFailures, lock); locked {
			_ = models.DeleteAuthCode(c.db, row.ID)
			c.history(&principal{Email: a.Email}, models.HistAccount, "login_locked", a.Email, "too many failed logins from "+remoteHost(r.RemoteAddr))
			writeError(w, http.StatusLocked, "login is temporarily locked after repeated failures")
			return
		}
		writeError(w, http.StatusUnauthorized, invalidCode)
		return
	}
	_ = models.DeleteAuthCode(c.db, row.ID)
	if err := c.rememberBrowser(w, r, a.ID, in.Trust, now); err != nil {
		c.internalError(w, r, err)
		return
	}
	detail := "from " + remoteHost(r.RemoteAddr) + "; code verified"
	if in.Trust {
		detail += ", browser trusted"
	}
	c.completeLogin(w, r, a, row.Remember, detail)
}
