package workers

import (
	"log"
	"os"
	"path/filepath"
	"time"

	"mimamori/app/models"
	"mimamori/app/modules"
)

// inProcessTransport hands the built-in agent's requests directly to the
// central without HTTP, tokens or certificates.
type inProcessTransport struct {
	c       *core
	agentID int64
}

func (t *inProcessTransport) agent() (*models.Agent, error) {
	return models.GetAgent(t.c.db.Read(), t.agentID)
}

func (t *inProcessTransport) Sync(req *modules.SyncRequest) (*modules.SyncResponse, error) {
	entered := time.Now().Unix()
	a, err := t.agent()
	if err != nil {
		return nil, err
	}
	return t.c.centralSync(a, nil, req, entered)
}

func (t *inProcessTransport) Report(req *modules.ReportRequest) error {
	a, err := t.agent()
	if err != nil {
		return err
	}
	return t.c.centralReport(a, req)
}

// clearDisabledBuiltin removes the audit rules and the rules file once when
// the built-in agent is disabled but its database records an applied rule,
// clears that record and records on the agent that nothing is applied.
// Without the database or the record nothing is touched (a separate agent
// may use the audit rules of this host). A failure is logged and tried again
// at the next start.
func (c *core) clearDisabledBuiltin(agent *models.Agent) {
	path := filepath.Join(c.dataDir, modules.BuiltinAgentLocalDBName)
	if _, err := os.Stat(path); err != nil {
		return
	}
	db, err := modules.OpenSQLite(path, modules.SchemaAgentLocal)
	if err != nil {
		c.logger.Printf("built-in agent: local database: %v", err)
		return
	}
	defer db.Close()
	removed, err := modules.ClearStoredRule(db)
	if err != nil {
		c.logger.Printf("built-in agent is disabled, but removing its audit rules failed: %v", err)
		return
	}
	if !removed {
		return
	}
	m := modules.M("apply.builtin_disabled")
	msg := m.Text()
	c.logger.Printf("%s (key %s, %s)", msg, modules.AuditKey, modules.AuditRulesFile)
	err = c.db.Write(func(tx models.Writer) error {
		return models.SetAgentApplyNone(tx, agent.ID, msg, applyI18nText(&m, nil), time.Now().Unix())
	})
	if err != nil {
		c.logger.Printf("built-in agent: %v", err)
	}
	c.history(nil, models.HistRule, "apply_result_"+modules.ApplyStatusNone, c.agentDisplayName(agent), m)
}

// startBuiltinAgent checks the prerequisites and, when they hold, starts the
// built-in agent on its own flows, as many as its own worker count in
// builtin-agent.db (default 1). It returns a function that waits for them to
// finish after stop was closed. When the prerequisites fail, only the
// central functions run and the reason is shown on screen (the caller logs
// it once).
func (c *core) startBuiltinAgent(agent *models.Agent, stop <-chan struct{}) func() {
	none := func() {}
	if !*c.conf().BuiltinAgent {
		// Nothing to report: disabling it is the administrator's choice.
		c.setBuiltinStatus(false, modules.Msg{})
		c.clearDisabledBuiltin(agent)
		return none
	}
	if err := modules.CheckAuditd(); err != nil {
		c.setBuiltinStatus(false, modules.M("builtin.auditd_unusable", "cause", modules.AsMsg(err)))
		return none
	}
	db, err := modules.OpenSQLite(filepath.Join(c.dataDir, modules.BuiltinAgentLocalDBName), modules.SchemaAgentLocal)
	if err != nil {
		c.setBuiltinStatus(false, modules.M("builtin.local_db", "cause", modules.AsMsg(err)))
		return none
	}
	// The built-in agent's own worker count, from its own database; the
	// central's item workers has nothing to do with it.
	workers, err := modules.LoadAgentWorkers(db.Read())
	if err != nil {
		db.Close()
		c.setBuiltinStatus(false, modules.M("builtin.local_db", "cause", modules.AsMsg(err)))
		return none
	}
	logger := log.New(c.logger.Writer(), "[builtin-agent] ", log.LstdFlags)
	rt := modules.NewAgentRuntime(db, &inProcessTransport{c: c, agentID: agent.ID}, logger)
	rt.SetWorkers(workers)
	done := make(chan struct{})
	c.setBuiltinStatus(true, modules.Msg{})
	logger.Printf("started (workers %d)", workers)
	go func() {
		defer close(done)
		defer db.Close()
		rt.Run(stop)
		c.setBuiltinStatus(false, modules.M("builtin.stopped"))
	}()
	return func() { <-done }
}
