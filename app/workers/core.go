package workers

import (
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"mimamori/app/models"
	"mimamori/app/modules"
)

// core holds the central's shared state. Request handlers may run on several
// worker flows and the built-in agent on its own flow, so everything shared
// is either in the databases or guarded here.
type core struct {
	db *sql.DB
	// cfg is the configuration in effect; it is replaced when the stored
	// configuration changes (see reloadConfig). started is the configuration
	// read at start, which keeps the items that apply only at start.
	cfg       atomic.Pointer[modules.CentralConfig]
	started   *modules.CentralConfig
	dataDir   string
	spaFS     fs.FS
	assets    *webAssets // from the frontend build manifest (webHandler)
	logger    *log.Logger
	serverKey []byte
	hostname  string
	// cliSecret authenticates the management CLI (see writeCLISecret).
	cliSecret string
	// sendMail delivers one mail synchronously (verification codes). It is
	// modules.SendMailTo; tests replace it.
	sendMail func(cfg modules.SMTPConfig, to []string, subject, body string) error

	targetsMu sync.Mutex
	targets   map[int64]*sql.DB

	serverCert atomic.Pointer[tls.Certificate]

	builtinMu      sync.Mutex
	builtinRunning bool
	builtinMessage string
}

func newCore(db *sql.DB, cfg *modules.CentralConfig, dataDir string, logger *log.Logger) *core {
	h, _ := os.Hostname()
	c := &core{db: db, started: cfg, dataDir: dataDir, logger: logger, hostname: h, targets: map[int64]*sql.DB{}, sendMail: modules.SendMailTo}
	c.cfg.Store(cfg)
	return c
}

// conf returns the configuration in effect.
func (c *core) conf() *modules.CentralConfig { return c.cfg.Load() }

// reloadConfig reads the stored configuration again so changes made on the
// settings screen or with "central config" take effect without a restart
// (except the items that apply only at start).
func (c *core) reloadConfig() {
	cfg, err := modules.LoadCentralConfig(c.db)
	if err != nil {
		c.logger.Printf("configuration not reloaded: %v", err)
		return
	}
	cfg.KeepRestartItems(c.started)
	c.cfg.Store(cfg)
}

func (c *core) setBuiltinStatus(running bool, msg string) {
	c.builtinMu.Lock()
	c.builtinRunning, c.builtinMessage = running, msg
	c.builtinMu.Unlock()
}

func (c *core) builtinStatus() (bool, string) {
	c.builtinMu.Lock()
	defer c.builtinMu.Unlock()
	return c.builtinRunning, c.builtinMessage
}

// targetDB opens (once) and returns the per-target database of an agent.
func (c *core) targetDB(a *models.Agent) (*sql.DB, error) {
	c.targetsMu.Lock()
	defer c.targetsMu.Unlock()
	if db, ok := c.targets[a.ID]; ok {
		return db, nil
	}
	file := a.DBFile
	if file == "" {
		file = fmt.Sprintf("agent-%d.db", a.ID)
		if err := models.SetAgentDBFile(c.db, a.ID, file); err != nil {
			return nil, err
		}
		a.DBFile = file
	}
	db, err := modules.OpenSQLite(modules.TargetDBPath(c.dataDir, file), modules.SchemaAgentDB)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2)
	c.targets[a.ID] = db
	return db, nil
}

// openAllTargets opens (and migrates) the database of every monitored target.
func (c *core) openAllTargets() error {
	agents, err := models.ListAgents(c.db)
	if err != nil {
		return err
	}
	for _, a := range agents {
		if _, err := c.targetDB(a); err != nil {
			return fmt.Errorf("agent %d: %w", a.ID, err)
		}
	}
	return nil
}

func (c *core) closeTargets() {
	c.targetsMu.Lock()
	defer c.targetsMu.Unlock()
	for id, db := range c.targets {
		db.Close()
		delete(c.targets, id)
	}
}

// createAgent inserts an agent row and its per-target database file.
func (c *core) createAgent(a *models.Agent) error {
	if err := models.InsertAgent(c.db, a); err != nil {
		return err
	}
	a.DBFile = fmt.Sprintf("agent-%d.db", a.ID)
	if err := models.SetAgentDBFile(c.db, a.ID, a.DBFile); err != nil {
		return err
	}
	_, err := c.targetDB(a)
	return err
}

// ensureBuiltinAgent creates the built-in agent row on first start.
func (c *core) ensureBuiltinAgent() (*models.Agent, error) {
	a, err := models.GetBuiltinAgent(c.db)
	if err == nil {
		return a, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	a = &models.Agent{Kind: models.AgentBuiltin}
	if err := c.createAgent(a); err != nil {
		return nil, err
	}
	return a, nil
}

// writeCLISecret creates a new secret for the management CLI at every start
// and writes it into the data directory (readable by the running user only).
func (c *core) writeCLISecret() error {
	v, err := modules.RandomHex(32)
	if err != nil {
		return err
	}
	if err := modules.WriteFileAtomic(filepath.Join(c.dataDir, modules.CLISecretFileName), []byte(v+"\n"), 0o600); err != nil {
		return err
	}
	c.cliSecret = v
	return nil
}

// ensureServerKey loads or creates the secret used to derive renewed tokens.
func (c *core) ensureServerKey() error {
	v, found, err := models.GetSettingStrict(c.db, settingServerKey)
	if err != nil {
		return err
	}
	if !found {
		v, err = modules.RandomHex(32)
		if err != nil {
			return err
		}
		if err := models.InsertSettingIfAbsent(c.db, settingServerKey, v); err != nil {
			return err
		}
		v = models.GetSetting(c.db, settingServerKey)
	}
	c.serverKey = []byte(v)
	return nil
}

// agentDisplayName returns the name shown for an agent.
func (c *core) agentDisplayName(a *models.Agent) string {
	if a.Kind == models.AgentBuiltin {
		return modules.BuiltinAgentName
	}
	if a.TokenID.Valid {
		if t, err := models.GetToken(c.db, a.TokenID.Int64); err == nil {
			return t.Name
		}
	}
	return fmt.Sprintf("agent-%d", a.ID)
}

// history records an operation.
func (c *core) history(p *principal, category, action, target, detail string) {
	name := "system"
	if p != nil {
		name = p.operator()
	}
	if err := models.InsertHistory(c.db, &models.HistoryItem{AccountName: name, Category: category, Action: action, Target: target, Detail: detail}); err != nil {
		c.logger.Printf("history: %v", err)
	}
}

// Settings keys (internal state in the settings table).
const (
	settingServerKey       = "server_key"
	settingLastNotifyAt    = "last_notify_compose_at"
	settingLastTokenCheck  = "last_token_expiry_check"
	settingLastRetentionAt = "last_retention_at"
)

// --- request helpers ---

type ctxKey int

const principalKey ctxKey = 1

// principal is the authenticated account of a request.
type principal struct {
	AccountID   int64
	Email       string // the login ID at the time of the request
	DisplayName string
	Role        string
	Language    string
	SessionID   string // hashed session id (browser only)
	StepUpAt    int64  // when a re-authentication code was entered in the session (browser only)
	ViaCLI      bool
}

func (p *principal) isAdmin() bool { return p != nil && p.Role == models.RoleAdmin }

// operator is the name recorded as the operator (history, created_by,
// acknowledged_by): the e-mail address of the account, or "cli" for the
// management CLI.
func (p *principal) operator() string {
	if p.ViaCLI {
		return cliOperator
	}
	return p.Email
}

// foldASCII lowers ASCII letters only, like the NOCASE collation of the
// accounts.email column.
func foldASCII(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}, s)
}

// operatorShown returns the function that converts a recorded operator for
// the response to r. Administrators and the CLI see the recorded value. A
// viewer sees, for a recorded e-mail address, the current display name of
// that account, or an empty string when no account has the address any
// more; values that are not addresses ("cli", "system", "local:<user>") are
// shown as they are.
func (c *core) operatorShown(r *http.Request) func(string) string {
	p := principalFrom(r)
	if p == nil || p.isAdmin() {
		return func(v string) string { return v }
	}
	var names map[string]string
	return func(v string) string {
		if !strings.Contains(v, "@") {
			return v
		}
		if names == nil {
			names = map[string]string{}
			if list, err := models.ListAccounts(c.db); err == nil {
				for _, a := range list {
					names[foldASCII(a.Email)] = a.DisplayName
				}
			}
		}
		return names[foldASCII(v)]
	}
}

func withPrincipal(r *http.Request, p *principal) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), principalKey, p))
}

func principalFrom(r *http.Request) *principal {
	p, _ := r.Context().Value(principalKey).(*principal)
	return p
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid request body: %v", err)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return errors.New("invalid request body: unexpected data after the JSON value")
	}
	return nil
}

func pathID(r *http.Request, name string) (int64, bool) {
	v, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	return v, err == nil && v > 0
}

// queryInt reads an optional integer query parameter, clamped to [min, max].
// An empty or absent value gives def; a value that is not an integer is an
// error.
func queryInt(r *http.Request, name string, def, min, max int) (int, error) {
	s := r.URL.Query().Get(name)
	if s == "" {
		return def, nil
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: not an integer", name, s)
	}
	if v < min {
		return min, nil
	}
	if v > max {
		return max, nil
	}
	return v, nil
}

func isUniqueErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// internalError logs err and answers 500 without details.
func (c *core) internalError(w http.ResponseWriter, r *http.Request, err error) {
	c.logger.Printf("%s %s: %v", r.Method, r.URL.Path, err)
	writeError(w, http.StatusInternalServerError, "internal error")
}
