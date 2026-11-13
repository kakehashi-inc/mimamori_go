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
		_ = c.db.Write(func(tx models.Writer) error { return models.DeleteSession(tx, s.ID) })
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
	s, err := models.GetSession(c.db.Read(), modules.HashSecret(ck.Value))
	if err != nil || s.ExpiresAt < time.Now().Unix() {
		return nil, nil
	}
	a, err := models.GetAccount(c.db.Read(), s.AccountID)
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
			writeError(w, http.StatusForbidden, modules.M("auth.missing_header"))
			return
		}
		if publicAPIRoute(r) {
			viewer.ServeHTTP(w, r)
			return
		}
		p, s := c.sessionPrincipal(r)
		if p == nil {
			writeError(w, http.StatusUnauthorized, modules.M("auth.login_required"))
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
			_ = c.db.Write(func(tx models.Writer) error { return models.TouchSession(tx, s.ID, now, expires) })
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
			writeError(w, http.StatusUnauthorized, modules.M("auth.unauthorized"))
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

// isLocalPeer reports whether a connection comes from this host: its source
// address (never a forwarding header) is a loopback address, or equals the
// local address the connection was accepted on (a connection from this host
// to one of its own addresses). local is the local address of the accepted
// connection (nil when unknown: then only loopback counts).
func isLocalPeer(remoteAddr string, local net.Addr) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return false
	}
	zone := ""
	if i := strings.IndexByte(host, '%'); i >= 0 {
		host, zone = host[:i], host[i+1:] // the zone of a link-local address
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	ta, ok := local.(*net.TCPAddr)
	return ok && ta != nil && !ta.IP.IsUnspecified() && ta.IP.Equal(ip) && (zone == "" || ta.Zone == "" || zone == ta.Zone)
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
			writeError(w, http.StatusUnauthorized, modules.M("auth.login_required"))
			return
		}
		h(w, r)
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
		err := c.db.Write(func(tx models.Writer) error {
			return models.RecordRejected(tx, remoteHost(r.RemoteAddr), reason, target, 600)
		})
		if err != nil {
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
	// The token row is read, checked and its use recorded in one write
	// transaction, so the checks are made on the row as it is when it is
	// written: another request of the same agent, or a revoke, reissue or
	// unbind by an administrator, comes before or after it as a whole.
	var t *models.Token
	var reason string
	err := c.db.Write(func(tx models.Writer) error {
		var err error
		t, err = models.FindTokenByHash(tx, h)
		if err != nil {
			return err
		}
		var usedPending bool
		if reason, usedPending = tokenProblem(t, h, instance, now); reason != "" {
			return nil
		}
		prev := *t
		if usedPending {
			// First connection with the renewed value: the old value stops working.
			t.Hash, t.Value, t.ExpiresAt = t.PendingHash, v, t.PendingExpiresAt
			t.PendingHash, t.PendingExpiresAt = "", 0
			t.RenewedAt = now
		}
		bound := t.InstanceID == ""
		if bound {
			t.InstanceID, t.BoundAt = instance, now
		}
		if t.UsedAt == 0 {
			t.UsedAt = now
		}
		t.LastSeenAt = now
		// Only the columns changed here are written, and only while the
		// token is as it was read (which inside this transaction it is).
		if err := models.RecordTokenUse(tx, t, &prev); err != nil {
			return err
		}
		if usedPending {
			if err := models.InsertHistory(tx, c.historyRow(nil, models.HistToken, "renewed", modules.Raw(t.Name), modules.M("history.detail.token_renewed"))); err != nil {
				return err
			}
		}
		if bound {
			if err := models.InsertHistory(tx, c.historyRow(nil, models.HistToken, "bound", modules.Raw(t.Name), modules.M("history.detail.token_bound", "instance", instance))); err != nil {
				return err
			}
		}
		return nil
	})
	switch {
	case errors.Is(err, sql.ErrNoRows) && t == nil:
		return reject("invalid token", "")
	case errors.Is(err, sql.ErrNoRows):
		return reject("invalid token", t.Name)
	case err != nil:
		return nil, nil, err
	case reason != "":
		return reject(reason, t.Name)
	}
	a, err := models.GetAgentByToken(c.db.Read(), t.ID)
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

// The request gate asks these, on the connection's own flow, before it reads
// a body larger than modules.MaxHeadBytes (see requestGate.admit). They
// answer whether the request is authenticated for its route, with the same
// checks as the handlers, but only read: no token use is recorded, no
// instance bound, no session extended or deleted, nothing written to the
// history. A request they refuse goes to the handler without its body, and
// the handler answers it as usual (with those effects, once). They read the
// route tables (fixed once the handlers are built), the CLI secret (fixed at
// start) and the database (safe next to any number of handlers).

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
	t, err := models.FindTokenByHash(c.db.Read(), h)
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
