package workers

import (
	"log"
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

// startBuiltinAgent checks the prerequisites and, when they hold, starts the
// built-in agent on its own flow. It returns a function that waits for the
// flow to finish after stop was closed. When the prerequisites fail, only the
// central functions run and the reason is shown in the log and on screen.
func (c *core) startBuiltinAgent(agent *models.Agent, stop <-chan struct{}) func() {
	none := func() {}
	if !*c.conf().BuiltinAgent {
		c.setBuiltinStatus(false, "the built-in agent is disabled in the configuration")
		return none
	}
	if err := modules.CheckAuditd(); err != nil {
		c.setBuiltinStatus(false, "auditd is not usable: "+err.Error())
		c.logger.Printf("built-in agent stopped: %v", err)
		return none
	}
	db, err := modules.OpenSQLite(filepath.Join(c.dataDir, modules.BuiltinAgentLocalDBName), modules.SchemaAgentLocal)
	if err != nil {
		c.setBuiltinStatus(false, "local database: "+err.Error())
		c.logger.Printf("built-in agent stopped: %v", err)
		return none
	}
	logger := log.New(c.logger.Writer(), "[builtin-agent] ", log.LstdFlags)
	rt := modules.NewAgentRuntime(db, &inProcessTransport{c: c, agentID: agent.ID}, c.dataDir, logger)
	rt.Builtin = true
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
