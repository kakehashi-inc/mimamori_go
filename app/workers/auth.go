package workers

import (
	"crypto/subtle"
	"database/sql"
	"errors"
	"net"
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

// sessionPrincipal validates the session cookie of a browser request and
// returns the account it belongs to, or nil when the request has no valid
// session. A session of a disabled account is deleted. It never extends the
// session: the API middleware (sessionAuth) does that for operations, and
// the static file handler uses it only to decide what it may serve.
func (c *core) sessionPrincipal(r *http.Request) (*principal, *models.Session) {
	s, a := c.findSession(r)
	if s == nil {
		return nil, nil
	}
	if a == nil || a.Disabled {
		_ = models.DeleteSession(c.db, s.ID)
		return nil, nil
	}
	return &principal{AccountID: a.ID, Email: a.Email, DisplayName: a.DisplayName, Role: a.Role, Language: a.Language, SessionID: s.ID, StepUpAt: s.StepUpAt}, s
}

// findSession returns the unexpired session of the request's session
// cookie and its account (nil when it cannot be read), or nil, nil. It
// changes nothing.
func (c *core) findSession(r *http.Request) (*models.Session, *models.Account) {
	ck, err := r.Cookie(sessionCookie)
	if err != nil || !strings.HasPrefix(ck.Value, modules.SessionPrefix) {
		return nil, nil
	}
	s, err := models.GetSession(c.db, modules.HashSecret(ck.Value))
	if err != nil || s.ExpiresAt < time.Now().Unix() {
		return nil, nil
	}
	a, err := models.GetAccount(c.db, s.AccountID)
	if err != nil {
		return s, nil
	}
	return s, a
}

// publicAPIRoute reports whether a browser API request needs no session.
func publicAPIRoute(r *http.Request) bool {
	return (r.Method == http.MethodGet && r.URL.Path == "/api/v1/health") ||
		(r.Method == http.MethodPost && (r.URL.Path == "/api/v1/auth/login" || r.URL.Path == "/api/v1/auth/login/verify"))
}

// sessionAuth authenticates browser requests by the session cookie and
// passes them to one of two route tables: admin (every route) for an
// administrator, viewer (the routes any account may use) for everybody
// else. A viewer who calls an administrator route therefore gets exactly
// what a route that does not exist gives, for every method (404, or 405
// with the Allow header of the routes a viewer has on that path).
func (c *core) sessionAuth(admin, viewer http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get(csrfHeader) != "1" {
			writeError(w, http.StatusForbidden, "missing request header")
			return
		}
		if publicAPIRoute(r) {
			viewer.ServeHTTP(w, r)
			return
		}
		p, s := c.sessionPrincipal(r)
		if p == nil {
			writeError(w, http.StatusUnauthorized, "login required")
			return
		}
		now := time.Now().Unix()
		if r.Header.Get(backgroundHeader) != "1" && now-s.LastSeenAt > 60 {
			// A session without "keep me logged in" ends SessionIdleTimeout
			// after the last operation, so every operation extends it; a
			// remembered session keeps its fixed expiry from login (like its
			// cookie).
			expires := now + int64(modules.SessionIdleTimeout/time.Second)
			if s.Remember {
				expires = s.ExpiresAt
			}
			_ = models.TouchSession(c.db, s.ID, now, expires)
		}
		next := viewer
		if p.isAdmin() {
			next = admin
		}
		next.ServeHTTP(w, withPrincipal(r, p))
	})
}

// adminAPIAuth guards the management API: only a connection from this host
// (a loopback source address, or a source address equal to the local
// address of the connection; otherwise it does not exist), and the CLI
// secret the central wrote into its data directory at start. The CLI is not
// tied to an account and has full authority.
func (c *core) adminAPIAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !managementPeer(r) {
			http.NotFound(w, r)
			return
		}
		if !c.cliCredential(r) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, withPrincipal(r, &principal{DisplayName: cliOperator, Role: models.RoleAdmin, ViaCLI: true}))
	})
}

// managementPeer reports whether a request may use the management API: it
// comes from this host (isLocalPeer).
func managementPeer(r *http.Request) bool {
	local, _ := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	return isLocalPeer(r.RemoteAddr, local)
}

// cliCredential reports whether a request carries the CLI secret.
func (c *core) cliCredential(r *http.Request) bool {
	v := bearer(r)
	return c.cliSecret != "" && subtle.ConstantTimeCompare([]byte(v), []byte(c.cliSecret)) == 1
}

// requireAdmin wraps a handler that only administrators may use. Browser
// requests of other accounts never reach it (sessionAuth routes them to the
// viewer route table); this check is kept as a second line and answers like
// a route that does not exist.
func requireAdmin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !principalFrom(r).isAdmin() {
			http.NotFound(w, r)
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
		var reason string
		if reason, usedPending = tokenProblem(t, h, instance, now); reason != "" {
			return reject(reason, t.Name)
		}
		prev := *t
		if usedPending {
			// First connection with the renewed value: the old value stops working.
			t.Hash, t.Value, t.ExpiresAt = t.PendingHash, v, t.PendingExpiresAt
			t.PendingHash, t.PendingExpiresAt = "", 0
			t.RenewedAt = now
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

// tokenProblem checks the token row t for a request that presents the
// value whose hash is h from the agent instance instance. It returns why the
// request is rejected ("" when it is not) and whether it presents the
// pending (renewed) value. It changes nothing.
func tokenProblem(t *models.Token, h, instance string, now int64) (reason string, usedPending bool) {
	switch {
	case t.RevokedAt > 0:
		return "revoked token", false
	case instance == "" || len(instance) > 128:
		return "missing instance id", false
	case t.InstanceID != "" && t.InstanceID != instance:
		return "instance id mismatch", false
	}
	if t.PendingHash != "" && h == t.PendingHash {
		if t.PendingExpiresAt <= now {
			return "expired token", true
		}
		return "", true
	}
	if t.ExpiresAt <= now {
		return "expired token", false
	}
	return "", false
}

// --- large bodies ---

// The connection server asks these, in its accepting flow, before it reads
// a body larger than modules.MaxHeadBytes (see listenerSpec.admit). They
// answer whether the request is authenticated for its route, with the same
// checks as the handlers, but only read: no token use is recorded, no
// instance bound, no session extended or deleted, nothing written to the
// history. A request they refuse goes to the handler without its body, and
// the handler answers it as usual (with those effects, once). They read the
// route tables (fixed once the handlers are built), the CLI secret (fixed at
// start) and the database (safe next to the workers).

// admitWebBody is the check of the browser listener: an API route of the
// session's account with the CSRF header. Routes without a session (login)
// never take a large body.
func (c *core) admitWebBody(r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, "/api/") || r.Header.Get(csrfHeader) != "1" || publicAPIRoute(r) {
		return false
	}
	s, a := c.findSession(r)
	if s == nil || a == nil || a.Disabled {
		return false
	}
	routes := c.webViewerRoutes
	if a.Role == models.RoleAdmin {
		routes = c.webAdminRoutes
	}
	return hasRoute(routes, r)
}

// admitAgentBody is the check of the agent listener: an agent route with a
// valid token, or a management route from this host with the CLI secret.
func (c *core) admitAgentBody(r *http.Request) bool {
	_, pattern := c.agentRoutes.Handler(r)
	switch pattern {
	case agentSyncRoute, agentReportRoute:
		return c.agentTokenValid(r)
	case adminRoutePrefix:
		return managementPeer(r) && c.cliCredential(r) && hasRoute(c.adminRoutes, r)
	}
	return false
}

// hasRoute reports whether a route table has a route for the request.
func hasRoute(routes *http.ServeMux, r *http.Request) bool {
	if routes == nil {
		return false
	}
	_, pattern := routes.Handler(r)
	return pattern != ""
}

// agentTokenValid reports whether authenticateAgent would accept the
// request's token and instance, without recording anything.
func (c *core) agentTokenValid(r *http.Request) bool {
	v := bearer(r)
	if !strings.HasPrefix(v, modules.TokenPrefix) {
		return false
	}
	h := modules.HashSecret(v)
	t, err := models.FindTokenByHash(c.db, h)
	if err != nil {
		return false
	}
	reason, _ := tokenProblem(t, h, strings.TrimSpace(r.Header.Get(modules.HeaderInstanceID)), time.Now().Unix())
	return reason == ""
}

func remoteHost(addr string) string {
	if i := strings.LastIndexByte(addr, ':'); i > 0 {
		return strings.Trim(addr[:i], "[]")
	}
	return addr
}
