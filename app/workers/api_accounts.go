package workers

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"mimamori/app/models"
	"mimamori/app/modules"
)

// dummyPasswordHash is verified against for unknown e-mail addresses so that a
// failed login takes the same time whether the name exists or not.
var dummyPasswordHash = sync.OnceValue(func() string {
	h, _ := modules.HashPassword("dummy-password-for-timing")
	return h
})

type accountJSON struct {
	ID          int64   `json:"id"`
	Email       string  `json:"email"`
	DisplayName string  `json:"display_name"`
	Role        string  `json:"role"`
	Language    string  `json:"language"`
	Disabled    bool    `json:"disabled"`
	Notify      bool    `json:"notify"`
	LockedUntil *string `json:"locked_until"`
	CreatedAt   *string `json:"created_at"`
}

func (c *core) accountOut(a *models.Account) accountJSON {
	locked := a.LockedUntil
	if locked < time.Now().Unix() {
		locked = 0
	}
	return accountJSON{ID: a.ID, Email: a.Email, DisplayName: a.DisplayName, Role: a.Role, Language: a.Language, Disabled: a.Disabled, Notify: a.Notify, LockedUntil: ts(locked),
		CreatedAt: ts(a.CreatedAt)}
}

func (c *core) handleHealth(w http.ResponseWriter, r *http.Request) {
	cfg := c.conf()
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "version": modules.AppVersion, "remember_days": cfg.SessionRememberDays,
		"trust_days": cfg.TwoStepTrustDays})
}

// meJSON is the signed-in account as the browser screens see it ("Me").
func meJSON(id int64, email, displayName, role, language string) map[string]any {
	return map[string]any{"id": id, "email": email, "display_name": displayName, "role": role, "language": language}
}

// duplicateEmail is the answer to an e-mail address another account has.
var duplicateEmail = modules.M("account.email_duplicate")

func (c *core) handleLogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email    string `json:"email"`
		Password string `json:"password"`
		Remember bool   `json:"remember"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	now := time.Now().Unix()
	a, err := models.GetAccountByEmail(c.db, strings.TrimSpace(in.Email))
	if err != nil {
		// Spend the same time as a real check so addresses cannot be probed.
		modules.VerifyPassword(dummyPasswordHash(), in.Password)
		writeError(w, http.StatusUnauthorized, modules.M("auth.login_failed"))
		return
	}
	if a.LockedUntil > now {
		writeError(w, http.StatusLocked, modules.M("auth.locked"))
		return
	}
	if a.Disabled || !modules.VerifyPassword(a.PasswordHash, in.Password) {
		lock := now + int64(modules.LoginLockDuration/time.Second)
		if locked, _ := models.RecordLoginFailure(c.db, a.ID, modules.LoginMaxFailures, lock); locked {
			c.history(&principal{Email: a.Email}, models.HistAccount, "login_locked", a.Email, "too many failed logins from "+remoteHost(r.RemoteAddr))
		}
		writeError(w, http.StatusUnauthorized, modules.M("auth.login_failed"))
		return
	}
	cfg := c.conf()
	host := remoteHost(r.RemoteAddr)
	switch {
	case cfg.SMTP.DevelopmentMode():
		// No SMTP server: no code could be delivered, so none is asked for.
		c.completeLogin(w, r, a, in.Remember, "from "+host+"; two-step verification skipped (SMTP not configured)")
	case c.browserVerified(r, a.ID, cfg, now):
		c.completeLogin(w, r, a, in.Remember, "from "+host+"; verified browser")
	default:
		c.startLoginCode(w, r, a, in.Remember, now)
	}
}

// completeLogin ends a successful login: it clears the failure counter,
// starts a session, records the login and answers Me.
func (c *core) completeLogin(w http.ResponseWriter, r *http.Request, a *models.Account, remember bool, detail string) {
	_ = models.ResetLoginFailures(c.db, a.ID)
	if err := c.startSession(w, a.ID, remember); err != nil {
		c.internalError(w, r, err)
		return
	}
	c.history(&principal{Email: a.Email}, models.HistAccount, "login", a.Email, detail)
	writeJSON(w, http.StatusOK, meJSON(a.ID, a.Email, a.DisplayName, a.Role, a.Language))
}

// startSession creates a login session and sets its cookie.
func (c *core) startSession(w http.ResponseWriter, accountID int64, remember bool) error {
	now := time.Now().Unix()
	value, err := modules.GenerateSecret(modules.SessionPrefix)
	if err != nil {
		return err
	}
	// Without "keep me logged in" the session ends SessionIdleTimeout after
	// the last operation (sessionAuth extends it); the cookie has no Max-Age.
	expires := now + int64(modules.SessionIdleTimeout/time.Second)
	cookie := &http.Cookie{Name: sessionCookie, Value: value, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode}
	if remember {
		// "Keep me logged in": the cookie survives closing the browser and
		// the session lasts the configured period from login.
		maxAge := c.conf().SessionRememberDays * 86400
		expires = now + int64(maxAge)
		cookie.MaxAge = maxAge
	}
	if err := models.InsertSession(c.db, &models.Session{ID: modules.HashSecret(value), AccountID: accountID, CreatedAt: now, ExpiresAt: expires, LastSeenAt: now, Remember: remember}); err != nil {
		return err
	}
	http.SetCookie(w, cookie)
	return nil
}

func (c *core) handleLogout(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r)
	if p.SessionID != "" {
		_ = models.DeleteSession(c.db, p.SessionID)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (c *core) handleMe(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r)
	writeJSON(w, http.StatusOK, meJSON(p.AccountID, p.Email, p.DisplayName, p.Role, p.Language))
}

// handleOwnLanguage changes the display language of the signed-in account.
func (c *core) handleOwnLanguage(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r)
	var in struct {
		Language string `json:"language"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if !models.ValidLanguage(in.Language) {
		writeError(w, http.StatusBadRequest, modules.M("account.language_invalid"))
		return
	}
	if err := models.UpdateAccountLanguage(c.db, p.AccountID, in.Language); err != nil {
		c.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, meJSON(p.AccountID, p.Email, p.DisplayName, p.Role, in.Language))
}

func (c *core) handleOwnPassword(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r)
	var in struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	a, err := models.GetAccount(c.db, p.AccountID)
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	if !modules.VerifyPassword(a.PasswordHash, in.CurrentPassword) {
		writeError(w, http.StatusBadRequest, modules.M("account.current_password_wrong"))
		return
	}
	if err := modules.ValidatePassword(in.NewPassword); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	hash, err := modules.HashPassword(in.NewPassword)
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	if err := models.UpdateAccountPassword(c.db, a.ID, hash); err != nil {
		c.internalError(w, r, err)
		return
	}
	c.revokeAccountAuth(a.ID, p.SessionID)
	c.history(p, models.HistAccount, "password_changed", a.Email, "own password changed")
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (c *core) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	list, err := models.ListAccounts(c.db)
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	out := make([]accountJSON, 0, len(list))
	for _, a := range list {
		out = append(out, c.accountOut(a))
	}
	writeJSON(w, http.StatusOK, out)
}

func (c *core) handleCreateAccount(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email       string `json:"email"`
		DisplayName string `json:"display_name"`
		Role        string `json:"role"`
		Language    string `json:"language"`
		Password    string `json:"password"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	email, err := modules.ValidateEmail(in.Email)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	display, err := modules.ValidateDisplayName(in.DisplayName)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if in.Language == "" {
		in.Language = models.LangAuto
	}
	if !models.ValidLanguage(in.Language) {
		writeError(w, http.StatusBadRequest, modules.M("account.language_invalid"))
		return
	}
	if in.Role != models.RoleAdmin && in.Role != models.RoleViewer {
		writeError(w, http.StatusBadRequest, modules.M("account.role_invalid"))
		return
	}
	if err := modules.ValidatePassword(in.Password); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	hash, err := modules.HashPassword(in.Password)
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	a := &models.Account{Email: email, DisplayName: display, Role: in.Role, Language: in.Language, PasswordHash: hash}
	if err := models.InsertAccount(c.db, a); err != nil {
		if isUniqueErr(err) {
			writeError(w, http.StatusConflict, duplicateEmail)
			return
		}
		c.internalError(w, r, err)
		return
	}
	c.history(principalFrom(r), models.HistAccount, "created", a.Email, "role "+a.Role+", language "+a.Language)
	writeJSON(w, http.StatusOK, c.accountOut(a))
}

// loadAccount reads the {id} account or answers 404.
func (c *core) loadAccount(w http.ResponseWriter, r *http.Request) *models.Account {
	id, ok := pathID(r, "id")
	if !ok {
		writeError(w, http.StatusNotFound, modules.M("account.not_found"))
		return nil
	}
	a, err := models.GetAccount(c.db, id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, modules.M("account.not_found"))
		return nil
	}
	if err != nil {
		c.internalError(w, r, err)
		return nil
	}
	return a
}

// changeKeepingAdmin applies change to account a in one write transaction
// and undoes it (lastAdmin true) when a was an enabled administrator and no
// enabled administrator would remain. The change is written before the
// count, so the transaction already holds the write lock while it counts and
// concurrent changes of two administrators cannot both pass the check.
func (c *core) changeKeepingAdmin(a *models.Account, change func(models.Execer) error) (lastAdmin bool, err error) {
	tx, err := c.db.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := change(tx); err != nil {
		return false, err
	}
	if a.Role == models.RoleAdmin && !a.Disabled {
		n, err := models.CountActiveAdmins(tx)
		if err != nil {
			return false, err
		}
		if n == 0 {
			return true, nil
		}
	}
	return false, tx.Commit()
}

func (c *core) handleAccountRole(w http.ResponseWriter, r *http.Request) {
	a := c.loadAccount(w, r)
	if a == nil {
		return
	}
	var in struct {
		Role string `json:"role"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if in.Role != models.RoleAdmin && in.Role != models.RoleViewer {
		writeError(w, http.StatusBadRequest, modules.M("account.role_invalid"))
		return
	}
	if in.Role == a.Role {
		writeJSON(w, http.StatusOK, c.accountOut(a))
		return
	}
	last, err := c.changeKeepingAdmin(a, func(db models.Execer) error { return models.UpdateAccountRole(db, a.ID, in.Role) })
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	if last {
		writeError(w, http.StatusConflict, modules.M("account.last_admin_role"))
		return
	}
	c.invalidateAccount(a.ID)
	c.history(principalFrom(r), models.HistAccount, "role_changed", a.Email, a.Role+" -> "+in.Role)
	a.Role = in.Role
	writeJSON(w, http.StatusOK, c.accountOut(a))
}

func (c *core) handleAccountPassword(w http.ResponseWriter, r *http.Request) {
	a := c.loadAccount(w, r)
	if a == nil {
		return
	}
	var in struct {
		Password string `json:"password"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := modules.ValidatePassword(in.Password); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	hash, err := modules.HashPassword(in.Password)
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	if err := models.UpdateAccountPassword(c.db, a.ID, hash); err != nil {
		c.internalError(w, r, err)
		return
	}
	c.revokeAccountAuth(a.ID, "")
	c.history(principalFrom(r), models.HistAccount, "password_reset", a.Email, "password reset by an administrator")
	a.LockedUntil = 0
	writeJSON(w, http.StatusOK, c.accountOut(a))
}

// handleAccountEmail changes the e-mail address (login ID) of an account at
// once, without a verification code (administrators and the CLI). Every
// session, browser verification and pending code of the account ends.
func (c *core) handleAccountEmail(w http.ResponseWriter, r *http.Request) {
	a := c.loadAccount(w, r)
	if a == nil {
		return
	}
	var in struct {
		Email string `json:"email"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	email, err := modules.ValidateEmail(in.Email)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if email == a.Email {
		writeJSON(w, http.StatusOK, c.accountOut(a))
		return
	}
	if err := models.UpdateAccountEmail(c.db, a.ID, email); err != nil {
		if isUniqueErr(err) {
			writeError(w, http.StatusConflict, duplicateEmail)
			return
		}
		c.internalError(w, r, err)
		return
	}
	c.revokeAccountAuth(a.ID, "")
	c.history(principalFrom(r), models.HistAccount, "email_changed", email, a.Email+" -> "+email+" (changed by an administrator)")
	a.Email = email
	writeJSON(w, http.StatusOK, c.accountOut(a))
}

// handleAccountDisplayName changes the display name of an account
// (administrators and the CLI). Sessions are kept.
func (c *core) handleAccountDisplayName(w http.ResponseWriter, r *http.Request) {
	a := c.loadAccount(w, r)
	if a == nil {
		return
	}
	var in struct {
		DisplayName string `json:"display_name"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	display, err := modules.ValidateDisplayName(in.DisplayName)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if display == a.DisplayName {
		writeJSON(w, http.StatusOK, c.accountOut(a))
		return
	}
	if err := models.UpdateAccountDisplayName(c.db, a.ID, display); err != nil {
		c.internalError(w, r, err)
		return
	}
	c.history(principalFrom(r), models.HistAccount, "display_name_changed", a.Email, a.DisplayName+" -> "+display)
	a.DisplayName = display
	writeJSON(w, http.StatusOK, c.accountOut(a))
}

// handleAccountNotify chooses whether an account receives notification mails
// (administrators and the CLI). A disabled account keeps its choice but
// receives nothing while it is disabled.
func (c *core) handleAccountNotify(w http.ResponseWriter, r *http.Request) {
	a := c.loadAccount(w, r)
	if a == nil {
		return
	}
	var in struct {
		Notify *bool `json:"notify"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if in.Notify == nil {
		writeError(w, http.StatusBadRequest, modules.M("account.notify_invalid"))
		return
	}
	if *in.Notify == a.Notify {
		writeJSON(w, http.StatusOK, c.accountOut(a))
		return
	}
	if err := models.SetAccountNotify(c.db, a.ID, *in.Notify); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, modules.M("account.not_found"))
			return
		}
		c.internalError(w, r, err)
		return
	}
	detail := "notification mails off"
	if *in.Notify {
		detail = "notification mails on"
	}
	c.history(principalFrom(r), models.HistAccount, "notify_changed", a.Email, detail)
	a.Notify = *in.Notify
	writeJSON(w, http.StatusOK, c.accountOut(a))
}

func (c *core) setDisabled(w http.ResponseWriter, r *http.Request, disabled bool) {
	a := c.loadAccount(w, r)
	if a == nil {
		return
	}
	last, err := c.changeKeepingAdmin(a, func(db models.Execer) error { return models.SetAccountDisabled(db, a.ID, disabled) })
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	if last {
		writeError(w, http.StatusConflict, modules.M("account.last_admin_disable"))
		return
	}
	action := "enabled"
	if disabled {
		action = "disabled"
		c.revokeAccountAuth(a.ID, "")
	}
	c.history(principalFrom(r), models.HistAccount, action, a.Email, "")
	a.Disabled = disabled
	writeJSON(w, http.StatusOK, c.accountOut(a))
}

func (c *core) handleAccountDisable(w http.ResponseWriter, r *http.Request) {
	c.setDisabled(w, r, true)
}
func (c *core) handleAccountEnable(w http.ResponseWriter, r *http.Request) {
	c.setDisabled(w, r, false)
}

func (c *core) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	a := c.loadAccount(w, r)
	if a == nil {
		return
	}
	last, err := c.changeKeepingAdmin(a, func(db models.Execer) error { return models.DeleteAccount(db, a.ID) })
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	if last {
		writeError(w, http.StatusConflict, modules.M("account.last_admin_delete"))
		return
	}
	c.history(principalFrom(r), models.HistAccount, "deleted", a.Email, "")
	writeJSON(w, http.StatusOK, map[string]any{})
}
