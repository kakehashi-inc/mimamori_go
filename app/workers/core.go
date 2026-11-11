package workers

import (
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
	logger    *log.Logger
	serverKey []byte
	hostname  string
	// cliSecret authenticates the management CLI (see writeCLISecret).
	cliSecret string

	targetsMu sync.Mutex
	targets   map[int64]*sql.DB

	serverCert atomic.Pointer[tls.Certificate]

	builtinMu      sync.Mutex
	builtinRunning bool
	builtinMessage string
}

func newCore(db *sql.DB, cfg *modules.CentralConfig, dataDir string, logger *log.Logger) *core {
	h, _ := os.Hostname()
	c := &core{db: db, started: cfg, dataDir: dataDir, logger: logger, hostname: h, targets: map[int64]*sql.DB{}}
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
		name = p.Name
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
	AccountID int64
	Name      string
	Role      string
	Language  string
	SessionID string // hashed session id (browser only)
	ViaCLI    bool
}

func (p *principal) isAdmin() bool { return p != nil && p.Role == models.RoleAdmin }

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
	return nil
}

func pathID(r *http.Request, name string) (int64, bool) {
	v, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	return v, err == nil && v > 0
}

func queryInt(r *http.Request, name string, def, min, max int) int {
	v, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil {
		return def
	}
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

func isUniqueErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// internalError logs err and answers 500 without details.
func (c *core) internalError(w http.ResponseWriter, r *http.Request, err error) {
	c.logger.Printf("%s %s: %v", r.Method, r.URL.Path, err)
	writeError(w, http.StatusInternalServerError, "internal error")
}
