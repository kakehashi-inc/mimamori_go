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
	// db is the handle of central.db. pool is the connection pool the
	// central used before the handle; the code moves from it to db.
	db   *modules.DB
	pool *sql.DB
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
	// sendMail delivers one mail synchronously (verification codes and
	// notifications). It is modules.SendMailTo; tests replace it.
	sendMail func(cfg modules.SMTPConfig, to []string, subject, body string) error

	// targets is the set of the agents' own databases (see targetDB).
	// targetsMu guards the two maps only; it is never held while a file
	// is opened.
	targetsMu sync.Mutex
	targets   map[int64]*targetEntry
	// refusedTargets holds the agents whose target database was refused as
	// one of another build, so that the refusal is logged once.
	refusedTargets map[int64]bool

	serverCert atomic.Pointer[tls.Certificate]

	// Route tables the large-body checks (admitWebBody, admitAgentBody) look
	// requests up in; set when the handlers are built, read-only afterwards.
	webAdminRoutes, webViewerRoutes *http.ServeMux
	agentRoutes, adminRoutes        *http.ServeMux

	builtinMu      sync.Mutex
	builtinRunning bool
	builtinMessage modules.Msg
}

// targetEntry is an agent's database in the set of the central. ready is
// closed when the open has ended; db and err are set before that and not
// changed afterwards.
type targetEntry struct {
	ready chan struct{}
	db    *modules.DB
	err   error
}

func newCore(db *modules.DB, pool *sql.DB, cfg *modules.CentralConfig, dataDir string, logger *log.Logger) *core {
	h, _ := os.Hostname()
	c := &core{db: db, pool: pool, started: cfg, dataDir: dataDir, logger: logger, hostname: h, targets: map[int64]*targetEntry{}, refusedTargets: map[int64]bool{}, sendMail: modules.SendMailTo}
	c.cfg.Store(cfg)
	return c
}

// conf returns the configuration in effect.
func (c *core) conf() *modules.CentralConfig { return c.cfg.Load() }

// reloadConfig reads the stored configuration again so changes made on the
// settings screen or with "central config" take effect without a restart
// (except the items that apply only at start).
func (c *core) reloadConfig() {
	cfg, err := modules.LoadCentralConfig(c.pool)
	if err != nil {
		c.logger.Printf("configuration not reloaded: %v", err)
		return
	}
	cfg.KeepRestartItems(c.started)
	c.cfg.Store(cfg)
}

// setBuiltinStatus records whether the built-in agent runs and why not (an
// empty Msg when there is nothing to say).
func (c *core) setBuiltinStatus(running bool, msg modules.Msg) {
	c.builtinMu.Lock()
	c.builtinRunning, c.builtinMessage = running, msg
	c.builtinMu.Unlock()
}

func (c *core) builtinStatus() (bool, modules.Msg) {
	c.builtinMu.Lock()
	defer c.builtinMu.Unlock()
	return c.builtinRunning, c.builtinMessage
}

// targetDB returns the handle of an agent's own database and opens it when
// it is not open yet. Only flows that ask for the same agent wait for its
// open: the lock of the set is held while the set is looked up or changed,
// never while a file is opened or migrated, so flows that use other agents
// go on. A failed open is not remembered; the next call tries again.
func (c *core) targetDB(a *models.Agent) (*modules.DB, error) {
	c.targetsMu.Lock()
	if t, ok := c.targets[a.ID]; ok {
		c.targetsMu.Unlock()
		<-t.ready
		return t.db, t.err
	}
	t := &targetEntry{ready: make(chan struct{})}
	c.targets[a.ID] = t
	c.targetsMu.Unlock()

	// The flows waiting for this entry are released however the open
	// ends, also when it panics.
	defer func() {
		if t.db == nil {
			if t.err == nil {
				t.err = fmt.Errorf("agent %d: database not opened", a.ID)
			}
			c.targetsMu.Lock()
			delete(c.targets, a.ID)
			c.targetsMu.Unlock()
		}
		close(t.ready)
	}()
	t.db, t.err = c.openTarget(a)
	if t.err != nil {
		// A database of another build is logged here once, also for the
		// callers that only skip the target; a request answers 500.
		c.targetsMu.Lock()
		logIt := modules.IsOtherBuildDB(t.err) && !c.refusedTargets[a.ID]
		if logIt {
			c.refusedTargets[a.ID] = true
		}
		c.targetsMu.Unlock()
		if logIt {
			c.logger.Print(t.err)
		}
	}
	return t.db, t.err
}

// openTarget opens the database of an agent, giving the agent its file name
// first when it has none yet. It is called by the one flow that put the
// agent's entry into the set.
func (c *core) openTarget(a *models.Agent) (*modules.DB, error) {
	file := a.DBFile
	if file == "" {
		file = fmt.Sprintf("agent-%d.db", a.ID)
		if err := models.SetAgentDBFile(c.pool, a.ID, file); err != nil {
			return nil, err
		}
		a.DBFile = file
	}
	db, err := modules.OpenSQLite(modules.TargetDBPath(c.dataDir, file), modules.SchemaAgentDB)
	if err != nil {
		return nil, err
	}
	// The central holds hundreds of these and reads each about once per
	// check interval: one idle read connection is kept, further ones are
	// opened only while several flows read the same agent at once.
	db.KeepReadConns(1)
	return db, nil
}

// openAllTargets opens (and migrates) the database of every monitored target.
func (c *core) openAllTargets() error {
	agents, err := models.ListAgents(c.pool)
	if err != nil {
		return err
	}
	for _, a := range agents {
		if _, err := c.targetDB(a); err != nil {
			// A database of another build is logged by targetDB; the
			// central runs and the requests about that target fail.
			if modules.IsOtherBuildDB(err) {
				continue
			}
			return fmt.Errorf("agent %d: %w", a.ID, err)
		}
	}
	return nil
}

// closeTargets closes every agent database the central has open. It is
// called at stop, after the last request and periodic step have ended; an
// open still in progress is waited for. A later targetDB opens the file
// again.
func (c *core) closeTargets() {
	c.targetsMu.Lock()
	open := c.targets
	c.targets = map[int64]*targetEntry{}
	c.targetsMu.Unlock()
	for _, t := range open {
		<-t.ready
		if t.db != nil {
			t.db.Close()
		}
	}
}

// createAgent inserts an agent row and its per-target database file.
func (c *core) createAgent(a *models.Agent) error {
	if err := models.InsertAgent(c.pool, a); err != nil {
		return err
	}
	a.DBFile = fmt.Sprintf("agent-%d.db", a.ID)
	if err := models.SetAgentDBFile(c.pool, a.ID, a.DBFile); err != nil {
		return err
	}
	_, err := c.targetDB(a)
	return err
}

// ensureBuiltinAgent creates the built-in agent row on first start.
func (c *core) ensureBuiltinAgent() (*models.Agent, error) {
	a, err := models.GetBuiltinAgent(c.pool)
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
	v, found, err := models.GetSettingStrict(c.pool, settingServerKey)
	if err != nil {
		return err
	}
	if !found {
		v, err = modules.RandomHex(32)
		if err != nil {
			return err
		}
		if err := models.InsertSettingIfAbsent(c.pool, settingServerKey, v); err != nil {
			return err
		}
		v = models.GetSetting(c.pool, settingServerKey)
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
		if t, err := models.GetToken(c.pool, a.TokenID.Int64); err == nil {
			return t.Name
		}
	}
	return fmt.Sprintf("agent-%d", a.ID)
}

// history records an operation on the target (a value: a name, an
// address). The detail is a coded message (modules.Msg{} for none): its
// English rendering is the recorded detail, and the code is kept next to it
// for the screens.
func (c *core) history(p *principal, category, action, target string, detail modules.Msg) {
	c.historyMsg(p, category, action, modules.Raw(target), detail)
}

// historyParts is the history detail of several parts recorded as one text
// joined with "; " (one part is recorded as it is).
func historyParts(parts []modules.Msg) modules.Msg {
	if len(parts) == 1 {
		return parts[0]
	}
	return modules.M("common.problems", "problems", parts)
}

// historyMsg records an operation whose target is a coded message too.
func (c *core) historyMsg(p *principal, category, action string, target, detail modules.Msg) {
	if err := models.InsertHistory(c.pool, c.historyRow(p, category, action, target, detail)); err != nil {
		c.logger.Printf("history: %v", err)
	}
}

// historyRow builds the history row of an operation without writing it: a
// write transaction inserts it with models.InsertHistory, so that the row
// is written together with the change it records or not at all.
func (c *core) historyRow(p *principal, category, action string, target, detail modules.Msg) *models.HistoryItem {
	name := "system"
	if p != nil {
		name = p.operator()
	}
	return &models.HistoryItem{AccountName: name, Category: category, Action: action, Target: target.Text(), Detail: detail.Text(),
		I18n: modules.HistoryTextsJSON(target, detail)}
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
			if list, err := models.ListAccounts(c.pool); err == nil {
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

// errorJSON is the body of every error answer: the English text in "error"
// (what the CLI shows) and the coded message in "error_i18n" (what the
// browser translates, falling back to "error").
type errorJSON struct {
	Error     string      `json:"error"`
	ErrorI18n modules.Msg `json:"error_i18n"`
}

// errorBody returns the error answer of a message.
func errorBody(m modules.Msg) errorJSON {
	return errorJSON{Error: m.Text(), ErrorI18n: m}
}

// writeError answers with the message: {"error": English, "error_i18n": m}.
func writeError(w http.ResponseWriter, status int, m modules.Msg) {
	writeJSON(w, status, errorBody(m))
}

// msgOrNil returns the message for an optional "<field>_i18n" answer field:
// nil when its text is empty.
func msgOrNil(m modules.Msg) *modules.Msg {
	if m.Text() == "" {
		return nil
	}
	return &m
}

// writeErr answers with the text of err (a validator's error, a request
// helper's Msg): its message when it is one, otherwise the text as data.
func writeErr(w http.ResponseWriter, status int, err error) {
	writeError(w, status, modules.AsMsg(err))
}

func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return invalidBody(err)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return modules.M("common.invalid_body_trailing")
	}
	return nil
}

// problemsBody returns the answer fields of a list of validation problems:
// "errors" with their English texts and "errors_i18n" with the coded
// problems in the same order (both empty lists when there is none).
func problemsBody(ps []modules.Problem) map[string]any {
	return map[string]any{"errors": modules.ProblemTexts(ps), "errors_i18n": nonNil(ps)}
}

// writeProblems answers an error that carries a list of problems: the
// error message m and the problems as problemsBody gives them.
func writeProblems(w http.ResponseWriter, status int, m modules.Msg, ps []modules.Problem) {
	out := problemsBody(ps)
	out["error"], out["error_i18n"] = m.Text(), m
	writeJSON(w, status, out)
}

// invalidBody returns the answer to a request body that could not be
// decoded: the decoder's text is data, except the refusal of the connection
// server to read a large body of an unauthenticated request.
func invalidBody(err error) modules.Msg {
	detail := modules.AsMsg(err)
	if errors.Is(err, errBodyRefused) {
		detail = modules.M("common.body_refused")
	}
	return modules.M("common.invalid_body", "detail", detail)
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
		return 0, modules.M("common.query_not_integer", "name", name, "value", strconv.Quote(s))
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

// refusal is what a Write callback returns when it decides that the
// request is answered with an error: the transaction is rolled back and the
// handler answers status with msg after the Write (failed does it), never
// from inside the callback.
type refusal struct {
	status int
	msg    modules.Msg
}

func (e *refusal) Error() string { return e.msg.Text() }

// failed answers the error of a Write and reports whether there was one: a
// refusal with its status and message, any other error as an internal
// error.
func (c *core) failed(w http.ResponseWriter, r *http.Request, err error) bool {
	if err == nil {
		return false
	}
	var ref *refusal
	if errors.As(err, &ref) {
		writeError(w, ref.status, ref.msg)
	} else {
		c.internalError(w, r, err)
	}
	return true
}

// internalError logs err and answers 500 without details.
func (c *core) internalError(w http.ResponseWriter, r *http.Request, err error) {
	c.logger.Printf("%s %s: %v", r.Method, r.URL.Path, err)
	writeError(w, http.StatusInternalServerError, modules.M("common.internal_error"))
}
