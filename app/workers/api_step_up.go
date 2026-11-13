package workers

import (
	"database/sql"
	"errors"
	"net/http"
	"time"

	"mimamori/app/models"
	"mimamori/app/modules"
)

// Re-authentication (step-up) for changes to command sets: a browser session
// must have entered a mailed code within StepUpValidity before it may
// create, update, delete or import command sets. The code entered at login
// does not count, and no trusted browser spares it. The management API (CLI)
// is never asked, and development mode (no smtp.host) skips it because no
// code could be delivered.

// Answers of the re-authentication.
var (
	stepUpRequired    = modules.M("auth.step_up_required")
	stepUpCodeExpired = modules.M("auth.step_up_code_expired")
)

// stepUpValid reports whether the principal may make a change that needs
// re-authentication at the time now.
func (c *core) stepUpValid(p *principal, now int64) bool {
	if p == nil || p.ViaCLI || c.conf().SMTP.DevelopmentMode() {
		return true
	}
	return p.StepUpAt > 0 && now < p.StepUpAt+int64(modules.StepUpValidity/time.Second)
}

// requireCommandStepUp wraps the changing operations on command sets
// (create, update, delete, import). Without a valid re-authentication the
// request is answered 428 and nothing is changed; the browser then asks for
// the code and repeats the request. Run requests and their discarding are
// not wrapped.
func (c *core) requireCommandStepUp(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !c.stepUpValid(principalFrom(r), time.Now().Unix()) {
			writeJSON(w, http.StatusPreconditionRequired, map[string]any{"error": stepUpRequired.Text(), "error_i18n": stepUpRequired, "step_up_required": true})
			return
		}
		h(w, r)
	}
}

// handleStepUpRequest mails a re-authentication code to the address of the
// signed-in account, synchronously, and answers 202. The code is bound to
// this session; a new request replaces an earlier code of the session. In
// development mode nothing is sent and the answer says that no code is
// needed. When the mail cannot be sent the answer is 503 and the failure is
// recorded in the history.
func (c *core) handleStepUpRequest(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r)
	if c.conf().SMTP.DevelopmentMode() || p.ViaCLI {
		writeJSON(w, http.StatusOK, map[string]any{"required": false})
		return
	}
	_ = models.DeleteSessionAuthCodes(c.db, models.CodePurposeStepUp, p.AccountID, p.SessionID)
	row := newAuthCode(r, models.CodePurposeStepUp, p.AccountID, time.Now().Unix())
	row.SessionID = p.SessionID
	code, err := c.seal(row)
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	if err := c.sendCodeMail(r, p.Email, code, "re-authentication for changes to command sets"); err != nil {
		c.history(p, models.HistAccount, "step_up_code_failed", p.Email,
			modules.M("history.detail.step_up_code_failed", "host", remoteHost(r.RemoteAddr), "cause", modules.AsMsg(err)))
		writeError(w, http.StatusServiceUnavailable, codeMailFailed)
		return
	}
	if err := models.InsertAuthCode(c.db, row); err != nil {
		c.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"required": true})
}

// handleStepUpVerify checks a re-authentication code. Only the session that
// requested the code can enter it. A wrong code is a 400 (a 401 would mean
// "logged out" to the browser) and does not count toward the login lock;
// after AuthCodeMaxAttempts wrong codes, or when the code has expired, it
// is discarded (410). A correct code is used up atomically and records the
// time on the session.
func (c *core) handleStepUpVerify(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r)
	var in struct {
		Code string `json:"code"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	now := time.Now().Unix()
	if c.conf().SMTP.DevelopmentMode() || p.ViaCLI {
		writeJSON(w, http.StatusOK, map[string]any{"required": false})
		return
	}
	row, err := models.GetSessionAuthCode(c.db, models.CodePurposeStepUp, p.AccountID, p.SessionID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusGone, stepUpCodeExpired)
		return
	}
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	if row.ExpiresAt <= now {
		_ = models.DeleteAuthCode(c.db, row.ID)
		writeError(w, http.StatusGone, stepUpCodeExpired)
		return
	}
	if !c.codeMatches(row, in.Code) {
		if n, err := models.RecordAuthCodeAttempt(c.db, row.ID); err != nil || n >= modules.AuthCodeMaxAttempts {
			_ = models.DeleteAuthCode(c.db, row.ID)
			writeError(w, http.StatusGone, stepUpCodeExpired)
			return
		}
		writeError(w, http.StatusBadRequest, invalidCode)
		return
	}
	consumed, err := models.ConsumeAuthCode(c.db, row.ID, modules.AuthCodeMaxAttempts, now)
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	if !consumed {
		writeError(w, http.StatusGone, stepUpCodeExpired)
		return
	}
	// The session may have ended meanwhile (logout, a password change).
	if err := models.SetSessionStepUp(c.db, p.SessionID, now); errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusUnauthorized, modules.M("auth.login_required"))
		return
	} else if err != nil {
		c.internalError(w, r, err)
		return
	}
	// How long the re-authentication lasts is not told to the browser.
	writeJSON(w, http.StatusOK, map[string]any{"required": true})
}
