package modules

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"mimamori/app/models"
)

// Exit code of "status" when the service is not running (as in LSB init
// scripts), so scripts and supervisors can test it.
const ExitNotRunning = 3

func stopService(name, lockPath string, timeout time.Duration) error {
	pid, running, err := LockHolder(lockPath)
	if err != nil {
		return NewExitError(ExitFileIO, err.Error())
	}
	if !running {
		fmt.Printf("%s is not running\n", name)
		return nil
	}
	fmt.Printf("Stopping %s (pid %d)...\n", name, pid)
	if _, err := StopProcess(lockPath, timeout); err != nil {
		return err
	}
	fmt.Printf("%s stopped\n", name)
	return nil
}

// printRunning prints the common status lines and reports whether the
// service is running.
func printRunning(name, lockPath string) (bool, error) {
	pid, running, err := LockHolder(lockPath)
	if err != nil {
		return false, NewExitError(ExitFileIO, err.Error())
	}
	if !running {
		fmt.Printf("%-16s not running\n", name+":")
		return false, nil
	}
	fmt.Printf("%-16s running\n", name+":")
	fmt.Printf("%-16s %d\n", "pid:", pid)
	return true, nil
}

// --- central ---

// CentralStopCmd stops the running central.
type CentralStopCmd struct {
	Timeout time.Duration `help:"How long to wait for the central to stop" default:"120s"`
}

func (c *CentralStopCmd) Run() error {
	return stopService("central", filepath.Join(DataDir(), CentralLockFileName), c.Timeout)
}

// CentralStatusCmd shows whether the central is running.
type CentralStatusCmd struct{}

func (c *CentralStatusCmd) Run() error {
	dir := DataDir()
	running, err := printRunning("central", filepath.Join(dir, CentralLockFileName))
	if err != nil {
		return err
	}
	fmt.Printf("%-16s %s\n", "data dir:", dir)
	dbPath := filepath.Join(dir, CentralDBFileName)
	if _, err := os.Stat(dbPath); err == nil {
		if db, err := OpenSQLite(dbPath, SchemaCentral); err == nil {
			cfg, cfgErr := LoadCentralConfig(db)
			// While the central runs, the values it started with are shown;
			// a saved value that differs takes effect after a restart.
			var runningValues map[string]string
			if running {
				runningValues = LoadRunningConfig(db)
			}
			db.Close()
			if cfgErr != nil {
				fmt.Printf("%-16s %v\n", "configuration:", cfgErr)
			}
			for _, line := range []struct{ label, item, prefix string }{
				{"web:", "web_listen", "http://"},
				{"agents/admin:", "agent_listen", "https://"},
				{"workers:", "workers", ""},
			} {
				if text, ok := statusValue(line.item, line.prefix, cfg, runningValues); ok {
					fmt.Printf("%-16s %s\n", line.label, text)
				}
			}
		}
	}
	if !running {
		return &ExitError{Code: ExitNotRunning}
	}
	return nil
}

// statusValue returns the text central status prints for a restart item:
// the running value when one is recorded (runningValues), marked when the
// saved value differs, otherwise the saved value. ok is false when neither
// is known.
func statusValue(item, prefix string, saved *CentralConfig, runningValues map[string]string) (string, bool) {
	var savedValue string
	if saved != nil {
		f, err := FindConfigField(item)
		if err != nil {
			return "", false
		}
		savedValue = f.get(saved)
	}
	runValue, isRunning := runningValues[item]
	switch {
	case isRunning && saved != nil && runValue != savedValue:
		return prefix + runValue + " (saved: " + savedValue + ", takes effect after restart)", true
	case isRunning:
		return prefix + runValue, true
	case saved != nil:
		return prefix + savedValue, true
	}
	return "", false
}

// --- agent ---

// AgentStopCmd stops the running agent.
type AgentStopCmd struct {
	Timeout time.Duration `help:"How long to wait for the agent to finish its current cycle and stop" default:"300s"`
}

func (c *AgentStopCmd) Run() error {
	return stopService("agent", filepath.Join(DataDir(), AgentLockFileName), c.Timeout)
}

// AgentStatusCmd shows whether the agent is running and its state.
type AgentStatusCmd struct{}

func (c *AgentStatusCmd) Run() error {
	dir := DataDir()
	running, err := printRunning("agent", filepath.Join(dir, AgentLockFileName))
	if err != nil {
		return err
	}
	dbPath := filepath.Join(dir, AgentLocalDBFileName)
	if _, err := os.Stat(dbPath); err != nil {
		fmt.Printf("%-16s not initialized (run agent init)\n", "configuration:")
	} else if db, err := OpenSQLite(dbPath, SchemaAgentLocal); err != nil {
		fmt.Printf("%-16s %v\n", "local database:", err)
	} else {
		if cfg, err := LoadAgentConfig(db); err == nil {
			fmt.Printf("%-16s %s\n", "central:", cfg.CentralAddress())
			fmt.Printf("%-16s %s\n", "token name:", cfg.TokenName)
			if cfg.TokenExpiresAt > 0 {
				fmt.Printf("%-16s %s\n", "token expires:", time.Unix(cfg.TokenExpiresAt, 0).Local().Format("2006-01-02 15:04:05"))
			}
		} else {
			fmt.Printf("%-16s %v\n", "configuration:", err)
		}
		var rule RuleAssignment
		if json.Unmarshal([]byte(models.GetLocalState(db, stateRule)), &rule) == nil && rule.ID > 0 {
			fmt.Printf("%-16s %s (version %d)\n", "rule:", rule.Name, rule.Version)
		} else {
			fmt.Printf("%-16s none\n", "rule:")
		}
		var ap ApplyResult
		if json.Unmarshal([]byte(models.GetLocalState(db, stateApply)), &ap) == nil && ap.Status != "" {
			fmt.Printf("%-16s %s: %s\n", "last apply:", ap.Status, ap.Message)
		}
		fmt.Printf("%-16s %d report(s), %d detection(s)\n", "unsent:", outboxRows(db), models.OutboxDetectionCount(db))
		db.Close()
	}
	if !running {
		return &ExitError{Code: ExitNotRunning}
	}
	return nil
}

func outboxRows(db models.Execer) int { return models.CountOutbox(db) }
