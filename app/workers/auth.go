package workers

import (
	"crypto/subtle"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"mimamori/app/models"
	"mimamori/app/modules"
)

const sessionCookie = "mimamori_session"

// csrfHeader must accompany every non-GET browser request. A cross-site page
// cannot add a custom header without a CORS preflight, which is never allowed.
const csrfHeader = "X-Mimamori-Request"

// backgroundHeader marks a request the open page sends by itself (the
// periodic refresh). It is authenticated like any other request but is not
// an operation of the user, so it does not extend the session.
const backgroundHeader = "X-Mimamori-Background"

func bearer(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if v, ok := strings.CutPrefix(auth, "Bearer "); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// sessionAuth authenticates browser requests by the session cookie.
func (c *core) sessionAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get(csrfHeader) != "1" {
			writeError(w, http.StatusForbidden, "missing request header")
			return
		}
		public := (r.Method == http.MethodGet && r.URL.Path == "/api/v1/health") ||
			(r.Method == http.MethodPost && (r.URL.Path == "/api/v1/auth/login" || r.URL.Path == "/api/v1/auth/login/verify"))
		if public {
			next.ServeHTTP(w, r)
			return
		}
		ck, err := r.Cookie(sessionCookie)
		if err != nil || !strings.HasPrefix(ck.Value, modules.SessionPrefix) {
			writeError(w, http.StatusUnauthorized, "login required")
			return
		}
		id := modules.HashSecret(ck.Value)
		s, err := models.GetSession(c.db, id)
		now := time.Now().Unix()
		if err != nil || s.ExpiresAt < now {
			writeError(w, http.StatusUnauthorized, "login required")
			return
		}
		a, err := models.GetAccount(c.db, s.AccountID)
		if err != nil || a.Disabled {
			_ = models.DeleteSession(c.db, id)
			writeError(w, http.StatusUnauthorized, "login required")
			return
		}
		if r.Header.Get(backgroundHeader) != "1" && now-s.LastSeenAt > 60 {
			// A session without "keep me logged in" ends SessionIdleTimeout
			// after the last operation, so every operation extends it; a
			// remembered session keeps its fixed expiry from login (like its
			// cookie).
			expires := now + int64(modules.SessionIdleTimeout/time.Second)
			if s.Remember {
				expires = s.ExpiresAt
			}
			_ = models.TouchSession(c.db, id, now, expires)
		}
		next.ServeHTTP(w, withPrincipal(r, &principal{AccountID: a.ID, Email: a.Email, DisplayName: a.DisplayName, Role: a.Role, Language: a.Language, SessionID: id}))
	})
}

// adminAPIAuth guards the management API: loopback source address only
// (otherwise it does not exist), and the CLI secret the central wrote into
// its data directory at start. The CLI is not tied to an account and has
// full authority.
func (c *core) adminAPIAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLoopback(r.RemoteAddr) {
			http.NotFound(w, r)
			return
		}
		v := bearer(r)
		if c.cliSecret == "" || subtle.ConstantTimeCompare([]byte(v), []byte(c.cliSecret)) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, withPrincipal(r, &principal{DisplayName: cliOperator, Role: models.RoleAdmin, ViaCLI: true}))
	})
}

// requireAdmin wraps a handler that only administrators may use.
func requireAdmin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !principalFrom(r).isAdmin() {
			writeError(w, http.StatusForbidden, "administrator role required")
			return
		}
		h(w, r)
	}
}

// requireAny wraps a handler any logged-in account may use.
func requireAny(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if principalFrom(r) == nil {
			writeError(w, http.StatusUnauthorized, "login required")
			return
		}
		h(w, r)
	}
}

// invalidateAccount ends every session of an account (a role change).
func (c *core) invalidateAccount(id int64) {
	_ = models.DeleteAccountSessions(c.db, id, "")
}

// revokeAccountAuth ends the sessions (all but keepSession), the browser
// verifications and the pending codes of an account (a password or e-mail
// address change, disabling).
func (c *core) revokeAccountAuth(id int64, keepSession string) {
	if err := modules.RevokeAccountAuth(c.db, id, keepSession); err != nil {
		c.logger.Printf("revoke account %d: %v", id, err)
	}
}

// cliOperator is the name recorded in the history for CLI operations.
const cliOperator = "cli"

// --- agent authentication ---

var errAgentAuth = errors.New("unauthorized")

// authenticateAgent checks the token and instance ID of an agent request.
// Every failure is answered without details and recorded in the history.
func (c *core) authenticateAgent(r *http.Request) (*models.Token, *models.Agent, error) {
	reject := func(reason, target string) (*models.Token, *models.Agent, error) {
		if err := models.RecordRejected(c.db, remoteHost(r.RemoteAddr), reason, target, 600); err != nil {
			c.logger.Printf("history: %v", err)
		}
		return nil, nil, errAgentAuth
	}
	v := bearer(r)
	if v == "" {
		return reject("missing token", "")
	}
	if !strings.HasPrefix(v, modules.TokenPrefix) {
		return reject("invalid token", "")
	}
	h := modules.HashSecret(v)
	now := time.Now().Unix()
	instance := strings.TrimSpace(r.Header.Get(modules.HeaderInstanceID))
	var t *models.Token
	var usedPending, bound bool
	// The token row can change between reading and writing it (another
	// request of the same agent, or an administrator). The checks are then
	// made again on the new row, so every request gets the answer that fits
	// the row as it is.
	for attempt := 1; ; attempt++ {
		var err error
		t, err = models.FindTokenByHash(c.db, h)
		if errors.Is(err, sql.ErrNoRows) {
			return reject("invalid token", "")
		}
		if err != nil {
			return nil, nil, err
		}
		if t.RevokedAt > 0 {
			return reject("revoked token", t.Name)
		}
		if instance == "" || len(instance) > 128 {
			return reject("missing instance id", t.Name)
		}
		if t.InstanceID != "" && t.InstanceID != instance {
			return reject("instance id mismatch", t.Name)
		}
		prev := *t
		usedPending = t.PendingHash != "" && h == t.PendingHash
		if usedPending {
			if t.PendingExpiresAt <= now {
				return reject("expired token", t.Name)
			}
			// First connection with the renewed value: the old value stops working.
			t.Hash, t.Value, t.ExpiresAt = t.PendingHash, v, t.PendingExpiresAt
			t.PendingHash, t.PendingExpiresAt = "", 0
			t.RenewedAt = now
		} else if t.ExpiresAt <= now {
			return reject("expired token", t.Name)
		}
		bound = t.InstanceID == ""
		if bound {
			t.InstanceID, t.BoundAt = instance, now
		}
		if t.UsedAt == 0 {
			t.UsedAt = now
		}
		t.LastSeenAt = now
		// Only the columns changed here are written, and only while the token
		// is unchanged since it was read: a concurrent revoke, reissue or
		// unbind wins.
		err = models.RecordTokenUse(c.db, t, &prev)
		if err == nil {
			break
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, nil, err
		}
		if attempt >= 3 {
			return reject("invalid token", t.Name)
		}
	}
	if usedPending {
		c.history(nil, models.HistToken, "renewed", t.Name, "agent switched to the renewed token; the previous value is no longer accepted")
	}
	if bound {
		c.history(nil, models.HistToken, "bound", t.Name, "bound to agent instance "+instance)
	}
	a, err := models.GetAgentByToken(c.db, t.ID)
	if err != nil {
		return nil, nil, err
	}
	return t, a, nil
}

func remoteHost(addr string) string {
	if i := strings.LastIndexByte(addr, ':'); i > 0 {
		return strings.Trim(addr[:i], "[]")
	}
	return addr
}
