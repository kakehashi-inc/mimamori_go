package workers

import (
	"database/sql"
	"errors"
	"net/http"
	"time"

	"mimamori/app/models"
	"mimamori/app/modules"
)

// handleOwnDisplayName changes the display name of the signed-in account.
// Nothing is invalidated.
func (c *core) handleOwnDisplayName(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r)
	var in struct {
		DisplayName string `json:"display_name"`
	}
	if err := readJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	display, err := modules.ValidateDisplayName(in.DisplayName)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if display != p.DisplayName {
		if err := models.UpdateAccountDisplayName(c.db, p.AccountID, display); err != nil {
			c.internalError(w, r, err)
			return
		}
		c.history(p, models.HistAccount, "display_name_changed", p.Email, p.DisplayName+" -> "+display+" (own change)")
	}
	writeJSON(w, http.StatusOK, meJSON(p.AccountID, p.Email, display, p.Role, p.Language))
}

// applyOwnEmail changes the address of the signed-in account and ends its
// other sessions, its browser verifications and its pending codes.
// It reports false after answering an error.
func (c *core) applyOwnEmail(w http.ResponseWriter, r *http.Request, p *principal, email string) bool {
	if err := models.UpdateAccountEmail(c.db, p.AccountID, email); err != nil {
		if isUniqueErr(err) {
			writeError(w, http.StatusConflict, duplicateEmail)
			return false
		}
		c.internalError(w, r, err)
		return false
	}
	c.revokeAccountAuth(p.AccountID, p.SessionID)
	c.history(p, models.HistAccount, "email_changed", email, p.Email+" -> "+email+" (own change)")
	return true
}

// handleOwnEmail starts the change of the own e-mail address: a code is
// mailed to the new address, and the address changes only when that code is
// entered (handleOwnEmailVerify). In development mode (no SMTP server) the
// address changes at once.
func (c *core) handleOwnEmail(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r)
	var in struct {
		Email string `json:"email"`
	}
	if err := readJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	email, err := modules.ValidateEmail(in.Email)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if email == p.Email {
		writeError(w, http.StatusBadRequest, "the e-mail address is unchanged")
		return
	}
	other, err := models.GetAccountByEmail(c.db, email)
	if err == nil && other.ID != p.AccountID {
		writeError(w, http.StatusConflict, duplicateEmail)
		return
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		c.internalError(w, r, err)
		return
	}
	if c.conf().SMTP.DevelopmentMode() {
		if c.applyOwnEmail(w, r, p, email) {
			writeJSON(w, http.StatusOK, map[string]any{"applied": true})
		}
		return
	}
	// A new request replaces an earlier one that was not confirmed.
	_ = models.DeleteAccountAuthCodes(c.db, p.AccountID, models.CodePurposeEmailChange)
	row := newAuthCode(r, models.CodePurposeEmailChange, p.AccountID, time.Now().Unix())
	row.SessionID = p.SessionID
	row.NewEmail = email
	code, err := c.seal(row)
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	if err := c.sendCodeMail(r, email, code, "e-mail address change"); err != nil {
		c.history(p, models.HistAccount, "email_code_failed", p.Email, "the code for the change to "+email+" could not be sent: "+err.Error())
		writeError(w, http.StatusServiceUnavailable, codeMailFailed)
		return
	}
	if err := models.InsertAuthCode(c.db, row); err != nil {
		c.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"applied": false})
}

// handleOwnEmailVerify confirms the own e-mail address change with the
// mailed code. Only the session that asked for the change can confirm it.
// A wrong code is a 400 (a 401 would mean "logged out" to the browser) and
// does not count toward the login lock; after AuthCodeMaxAttempts wrong
// codes the request is discarded.
func (c *core) handleOwnEmailVerify(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r)
	var in struct {
		Code string `json:"code"`
	}
	if err := readJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	row, err := models.GetEmailChangeCode(c.db, p.AccountID, p.SessionID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusGone, emailCodeExpired)
		return
	}
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	if row.ExpiresAt <= time.Now().Unix() {
		_ = models.DeleteAuthCode(c.db, row.ID)
		writeError(w, http.StatusGone, emailCodeExpired)
		return
	}
	if !c.codeMatches(row, in.Code) {
		if n, err := models.RecordAuthCodeAttempt(c.db, row.ID); err != nil || n >= modules.AuthCodeMaxAttempts {
			_ = models.DeleteAuthCode(c.db, row.ID)
			writeError(w, http.StatusGone, emailCodeExpired)
			return
		}
		writeError(w, http.StatusBadRequest, invalidCode)
		return
	}
	// Used up atomically, so a code admits one confirmation and concurrent
	// guesses cannot pass the attempt limit.
	consumed, err := models.ConsumeAuthCode(c.db, row.ID, modules.AuthCodeMaxAttempts, time.Now().Unix())
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	if !consumed {
		writeError(w, http.StatusGone, emailCodeExpired)
		return
	}
	if !c.applyOwnEmail(w, r, p, row.NewEmail) {
		return
	}
	writeJSON(w, http.StatusOK, meJSON(p.AccountID, row.NewEmail, p.DisplayName, p.Role, p.Language))
}
