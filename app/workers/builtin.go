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
	return models.GetAgent(t.c.db, t.agentID)
}

func (t *inProcessTransport) Sync(req *modules.SyncRequest) (*modules.SyncResponse, error) {
	a, err := t.agent()
	if err != nil {
		return nil, err
	}
	return t.c.centralSync(a, nil, req)
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
	msg := "the built-in agent is disabled; its audit rules were removed"
	c.logger.Printf("%s (key %s, %s)", msg, modules.AuditKey, modules.AuditRulesFile)
	if err := models.SetAgentApplyNone(c.db, agent.ID, msg, time.Now().Unix()); err != nil {
		c.logger.Printf("built-in agent: %v", err)
	}
	c.history(nil, models.HistRule, "apply_result_"+modules.ApplyStatusNone, c.agentDisplayName(agent), msg)
}

// startBuiltinAgent checks the prerequisites and, when they hold, starts the
// built-in agent on its own flow. It returns a function that waits for the
// flow to finish after stop was closed. When the prerequisites fail, only the
// central functions run and the reason is shown on screen (the caller logs
// it once).
func (c *core) startBuiltinAgent(agent *models.Agent, stop <-chan struct{}) func() {
	none := func() {}
	if !*c.conf().BuiltinAgent {
		// Nothing to report: disabling it is the administrator's choice.
		c.setBuiltinStatus(false, "")
		c.clearDisabledBuiltin(agent)
		return none
	}
	if err := modules.CheckAuditd(); err != nil {
		c.setBuiltinStatus(false, "auditd is not usable: "+err.Error())
		return none
	}
	db, err := modules.OpenSQLite(filepath.Join(c.dataDir, modules.BuiltinAgentLocalDBName), modules.SchemaAgentLocal)
	if err != nil {
		c.setBuiltinStatus(false, "local database: "+err.Error())
		return none
	}
	logger := log.New(c.logger.Writer(), "[builtin-agent] ", log.LstdFlags)
	rt := modules.NewAgentRuntime(db, &inProcessTransport{c: c, agentID: agent.ID}, logger)
	done := make(chan struct{})
	c.setBuiltinStatus(true, "")
	go func() {
		defer close(done)
		defer db.Close()
		stopped := false
		stopping := func() bool {
			if stopped {
				return true
			}
			select {
			case <-stop:
				stopped = true
			default:
			}
			return stopped
		}
		wait := func(d time.Duration) bool {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-stop:
				stopped = true
				return false
			case <-t.C:
				return true
			}
		}
		rt.Run(wait, stopping)
		c.setBuiltinStatus(false, "stopped")
	}()
	return func() { <-done }
}
