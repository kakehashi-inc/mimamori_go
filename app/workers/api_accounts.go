package workers

import (
	"database/sql"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"mimamori/app/models"
	"mimamori/app/modules"
)

var accountNameRe = regexp.MustCompile(`^[A-Za-z0-9._@-]{1,64}$`)

// dummyPasswordHash is verified against for unknown account names so that a
// failed login takes the same time whether the name exists or not.
var dummyPasswordHash = sync.OnceValue(func() string {
	h, _ := modules.HashPassword("dummy-password-for-timing")
	return h
})

type accountJSON struct {
	ID          int64   `json:"id"`
	Name        string  `json:"name"`
	Role        string  `json:"role"`
	Language    string  `json:"language"`
	Disabled    bool    `json:"disabled"`
	LockedUntil *string `json:"locked_until"`
	CreatedAt   *string `json:"created_at"`
}

func (c *core) accountOut(a *models.Account) accountJSON {
	locked := a.LockedUntil
	if locked < time.Now().Unix() {
		locked = 0
	}
	return accountJSON{ID: a.ID, Name: a.Name, Role: a.Role, Language: a.Language, Disabled: a.Disabled, LockedUntil: ts(locked),
		CreatedAt: ts(a.CreatedAt)}
}

func (c *core) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "version": modules.AppVersion, "remember_days": c.conf().SessionRememberDays})
}

func (c *core) handleLogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name     string `json:"name"`
		Password string `json:"password"`
		Remember bool   `json:"remember"`
	}
	if err := readJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	now := time.Now().Unix()
	a, err := models.GetAccountByName(c.db, strings.TrimSpace(in.Name))
	if err != nil {
		// Spend the same time as a real check so names cannot be probed.
		modules.VerifyPassword(dummyPasswordHash(), in.Password)
		writeError(w, http.StatusUnauthorized, "invalid account name or password")
		return
	}
	if a.LockedUntil > now {
		writeError(w, http.StatusLocked, "login is temporarily locked after repeated failures")
		return
	}
	if a.Disabled || !modules.VerifyPassword(a.PasswordHash, in.Password) {
		failed := a.FailedLogins + 1
		var lock int64
		if failed >= modules.LoginMaxFailures {
			lock = now + int64(modules.LoginLockDuration/time.Second)
			failed = 0
			c.history(&principal{Name: a.Name}, models.HistAccount, "login_locked", a.Name, "too many failed logins from "+remoteHost(r.RemoteAddr))
		}
		_ = models.RecordLoginFailure(c.db, a.ID, failed, lock)
		writeError(w, http.StatusUnauthorized, "invalid account name or password")
		return
	}
	_ = models.ResetLoginFailures(c.db, a.ID)
	value, err := modules.GenerateSecret(modules.SessionPrefix)
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	expires := now + int64(c.conf().SessionHours)*3600
	cookie := &http.Cookie{Name: sessionCookie, Value: value, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode}
	if in.Remember {
		// "Keep me logged in": the cookie survives closing the browser and
		// the session lasts the configured period from login.
		maxAge := c.conf().SessionRememberDays * 86400
		expires = now + int64(maxAge)
		cookie.MaxAge = maxAge
	}
	if err := models.InsertSession(c.db, &models.Session{ID: modules.HashSecret(value), AccountID: a.ID, CreatedAt: now, ExpiresAt: expires, LastSeenAt: now, Remember: in.Remember}); err != nil {
		c.internalError(w, r, err)
		return
	}
	http.SetCookie(w, cookie)
	c.history(&principal{Name: a.Name}, models.HistAccount, "login", a.Name, "from "+remoteHost(r.RemoteAddr))
	writeJSON(w, http.StatusOK, map[string]any{"id": a.ID, "name": a.Name, "role": a.Role, "language": a.Language})
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
	writeJSON(w, http.StatusOK, map[string]any{"id": p.AccountID, "name": p.Name, "role": p.Role, "language": p.Language})
}

// handleOwnLanguage changes the display language of the signed-in account.
func (c *core) handleOwnLanguage(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r)
	var in struct {
		Language string `json:"language"`
	}
	if err := readJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !models.ValidLanguage(in.Language) {
		writeError(w, http.StatusBadRequest, "language must be auto, en or ja")
		return
	}
	if err := models.UpdateAccountLanguage(c.db, p.AccountID, in.Language); err != nil {
		c.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": p.AccountID, "name": p.Name, "role": p.Role, "language": in.Language})
}

func (c *core) handleOwnPassword(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r)
	var in struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if err := readJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	a, err := models.GetAccount(c.db, p.AccountID)
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	if !modules.VerifyPassword(a.PasswordHash, in.CurrentPassword) {
		writeError(w, http.StatusBadRequest, "the current password is wrong")
		return
	}
	if err := modules.ValidatePassword(in.NewPassword); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
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
	_ = models.DeleteAccountSessions(c.db, a.ID, p.SessionID)
	c.history(p, models.HistAccount, "password_changed", a.Name, "own password changed")
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
		Name     string `json:"name"`
		Role     string `json:"role"`
		Language string `json:"language"`
		Password string `json:"password"`
	}
	if err := readJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Language == "" {
		in.Language = models.LangAuto
	}
	if !models.ValidLanguage(in.Language) {
		writeError(w, http.StatusBadRequest, "language must be auto, en or ja")
		return
	}
	if !accountNameRe.MatchString(in.Name) {
		writeError(w, http.StatusBadRequest, "account name must be 1-64 characters of A-Z a-z 0-9 . _ @ -")
		return
	}
	if in.Role != models.RoleAdmin && in.Role != models.RoleViewer {
		writeError(w, http.StatusBadRequest, "role must be admin or viewer")
		return
	}
	if err := modules.ValidatePassword(in.Password); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	hash, err := modules.HashPassword(in.Password)
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	a := &models.Account{Name: in.Name, Role: in.Role, Language: in.Language, PasswordHash: hash}
	if err := models.InsertAccount(c.db, a); err != nil {
		if isUniqueErr(err) {
			writeError(w, http.StatusConflict, "an account with this name already exists")
			return
		}
		c.internalError(w, r, err)
		return
	}
	c.history(principalFrom(r), models.HistAccount, "created", a.Name, "role "+a.Role+", language "+a.Language)
	writeJSON(w, http.StatusOK, c.accountOut(a))
}

// loadAccount reads the {id} account or answers 404.
func (c *core) loadAccount(w http.ResponseWriter, r *http.Request) *models.Account {
	id, ok := pathID(r, "id")
	if !ok {
		writeError(w, http.StatusNotFound, "account not found")
		return nil
	}
	a, err := models.GetAccount(c.db, id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "account not found")
		return nil
	}
	if err != nil {
		c.internalError(w, r, err)
		return nil
	}
	return a
}

// isLastAdmin reports whether a is the only enabled administrator.
func (c *core) isLastAdmin(a *models.Account) bool {
	if a.Role != models.RoleAdmin || a.Disabled {
		return false
	}
	n, err := models.CountActiveAdmins(c.db)
	return err == nil && n <= 1
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
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.Role != models.RoleAdmin && in.Role != models.RoleViewer {
		writeError(w, http.StatusBadRequest, "role must be admin or viewer")
		return
	}
	if in.Role == a.Role {
		writeJSON(w, http.StatusOK, c.accountOut(a))
		return
	}
	if in.Role == models.RoleViewer && c.isLastAdmin(a) {
		writeError(w, http.StatusConflict, "the last enabled administrator cannot be changed to viewer")
		return
	}
	if err := models.UpdateAccountRole(c.db, a.ID, in.Role); err != nil {
		c.internalError(w, r, err)
		return
	}
	c.invalidateAccount(a.ID, "")
	c.history(principalFrom(r), models.HistAccount, "role_changed", a.Name, a.Role+" -> "+in.Role)
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
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := modules.ValidatePassword(in.Password); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
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
	c.invalidateAccount(a.ID, "")
	c.history(principalFrom(r), models.HistAccount, "password_reset", a.Name, "password reset by an administrator")
	a.LockedUntil = 0
	writeJSON(w, http.StatusOK, c.accountOut(a))
}

func (c *core) setDisabled(w http.ResponseWriter, r *http.Request, disabled bool) {
	a := c.loadAccount(w, r)
	if a == nil {
		return
	}
	if disabled && c.isLastAdmin(a) {
		writeError(w, http.StatusConflict, "the last enabled administrator cannot be disabled")
		return
	}
	if err := models.SetAccountDisabled(c.db, a.ID, disabled); err != nil {
		c.internalError(w, r, err)
		return
	}
	action := "enabled"
	if disabled {
		action = "disabled"
		c.invalidateAccount(a.ID, "")
	}
	c.history(principalFrom(r), models.HistAccount, action, a.Name, "")
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
	if c.isLastAdmin(a) {
		writeError(w, http.StatusConflict, "the last enabled administrator cannot be deleted")
		return
	}
	c.invalidateAccount(a.ID, "")
	if err := models.DeleteAccount(c.db, a.ID); err != nil {
		c.internalError(w, r, err)
		return
	}
	c.history(principalFrom(r), models.HistAccount, "deleted", a.Name, "")
	writeJSON(w, http.StatusOK, map[string]any{})
}
