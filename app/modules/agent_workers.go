package modules

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"mimamori/app/models"
)

// The worker count of an agent: how many of its units (the monitoring step,
// runs of command sets) it processes at the same time. Every agent, the
// built-in agent included, keeps its own count in its own local database
// (agent.db, builtin-agent.db) and nowhere else; the central's item workers
// is a different setting and is never read here.
const (
	// stateWorkers is the saved count (absent: DefaultAgentWorkers). It is
	// changed by agent set-workers and read when the agent starts.
	stateWorkers = "workers"
	// stateRunningWorkers is the count the running agent uses: written by Run
	// when it starts, deleted when it ends. It is read only while the
	// process that runs the agent holds its lock (agent.lock, or
	// central.lock for the built-in agent), so a value a crash left behind
	// is never shown.
	stateRunningWorkers = "running_workers"
)

// parseAgentWorkers returns the count a stored text names, or false when it
// is not an integer 1 to MaxAgentWorkers.
func parseAgentWorkers(v string) (int, bool) {
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > MaxAgentWorkers {
		return 0, false
	}
	return n, true
}

// LoadAgentWorkers returns the saved worker count of the agent whose local
// database r reads: DefaultAgentWorkers when none is saved, an error when
// the stored value is not an integer 1 to MaxAgentWorkers.
func LoadAgentWorkers(r models.Reader) (int, error) {
	v, found, err := models.FindLocalState(r, stateWorkers)
	if err != nil {
		return 0, fmt.Errorf("workers: %w", err)
	}
	if !found {
		return DefaultAgentWorkers, nil
	}
	n, ok := parseAgentWorkers(v)
	if !ok {
		return 0, fmt.Errorf("workers: stored value %q is not 1-%d; set it with agent set-workers", v, MaxAgentWorkers)
	}
	return n, nil
}

// saveAgentWorkers saves the worker count n.
func saveAgentWorkers(w models.Writer, n int) error {
	return models.SetLocalState(w, stateWorkers, strconv.Itoa(n))
}

// loadRunningWorkers returns the count the running agent recorded, and false
// when none (or no valid one) is recorded. The caller must know that the
// process that runs the agent holds its lock.
func loadRunningWorkers(r models.Reader) (int, bool) {
	v, found, err := models.FindLocalState(r, stateRunningWorkers)
	if err != nil || !found {
		return 0, false
	}
	return parseAgentWorkers(v)
}

// recordRunningWorkers records n as the count in use (Run at its start).
func recordRunningWorkers(db *DB, n int) error {
	return db.Write(func(tx models.Writer) error {
		return models.SetLocalState(tx, stateRunningWorkers, strconv.Itoa(n))
	})
}

// clearRunningWorkers removes the record of recordRunningWorkers (Run at
// its end).
func clearRunningWorkers(db *DB) error {
	return db.Write(func(tx models.Writer) error {
		return models.DeleteLocalState(tx, stateRunningWorkers)
	})
}

// AgentSetWorkers saves the worker count n of the agent in dataDir (agent
// set-workers), or with builtin that of the built-in agent of the central in
// dataDir (builtin-agent.db, created when missing). The agent or the central
// may be running: the count takes effect at its next start, and the line
// printed says which count the running process uses until then.
func AgentSetWorkers(dataDir string, n int, builtin bool) error {
	if n < 1 || n > MaxAgentWorkers {
		return NewExitErrorf(ExitArgument, "workers %d must be 1-%d", n, MaxAgentWorkers)
	}
	var dbPath, lockPath string
	if builtin {
		if _, err := os.Stat(filepath.Join(dataDir, CentralDBFileName)); err != nil {
			return NewExitErrorf(ExitConfig, "the data directory %s holds no central (%s not found); --builtin sets the built-in agent of a central", dataDir, CentralDBFileName)
		}
		dbPath, lockPath = filepath.Join(dataDir, BuiltinAgentLocalDBName), filepath.Join(dataDir, CentralLockFileName)
	} else {
		dbPath, lockPath = filepath.Join(dataDir, AgentLocalDBFileName), filepath.Join(dataDir, AgentLockFileName)
		if _, err := os.Stat(dbPath); err != nil {
			return NewExitError(ExitConfig, ErrAgentNotInitialized.Error())
		}
	}
	db, err := OpenSQLite(dbPath, SchemaAgentLocal)
	if err != nil {
		return DBOpenFailed(err, ExitConfig, "local database")
	}
	defer db.Close()
	// The value before the change: the saved count, the default when none
	// is saved, or the stored text when it is not a valid count (which this
	// command replaces).
	was := strconv.Itoa(DefaultAgentWorkers)
	inUse, hasInUse := 0, false
	err = db.Write(func(tx models.Writer) error {
		v, found, err := models.FindLocalState(tx, stateWorkers)
		if err != nil {
			return err
		}
		if found {
			was = v
			if old, ok := parseAgentWorkers(v); ok {
				was = strconv.Itoa(old)
			}
		}
		inUse, hasInUse = loadRunningWorkers(tx)
		return saveAgentWorkers(tx, n)
	})
	if err != nil {
		return NewExitErrorf(ExitConfig, "local database: %v", err)
	}
	// running_workers is meaningful only while its process holds the lock.
	if _, running, err := LockHolder(lockPath); err != nil || !running {
		hasInUse = false
	}
	who, starts, running, restarted := "the agent", "the agent", "The running agent", "it is"
	if builtin {
		who, starts, running, restarted = "the built-in agent", "the central", "The running built-in agent", "the central is"
	}
	if hasInUse {
		fmt.Printf("Workers of %s set to %d (was %s). %s uses %d until %s restarted.\n", who, n, was, running, inUse, restarted)
	} else {
		fmt.Printf("Workers of %s set to %d (was %s). It takes effect when %s starts.\n", who, n, was, starts)
	}
	return nil
}

// agentWorkersStatus returns the text of the workers: line of agent status:
// the saved count, or the count in use followed by the saved one when the
// running agent uses another count. running tells that the agent's process
// holds its lock.
func agentWorkersStatus(r models.Reader, running bool) string {
	saved, err := LoadAgentWorkers(r)
	inUse, hasInUse := 0, false
	if running {
		inUse, hasInUse = loadRunningWorkers(r)
	}
	switch {
	case err != nil && hasInUse:
		return fmt.Sprintf("%d (saved: %v)", inUse, err)
	case err != nil:
		return err.Error()
	case hasInUse && inUse != saved:
		return fmt.Sprintf("%d (saved: %d, takes effect after restart)", inUse, saved)
	}
	return strconv.Itoa(saved)
}
