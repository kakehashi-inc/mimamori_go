package workers

import (
	"context"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"

	"mimamori/app/models"
	"mimamori/app/modules"
)

func init() {
	// Register the entry point so the CLI (modules) can start the central.
	modules.StartCentral = startCentral
}

// startCentral runs the central until SIGINT or SIGTERM. Its flows: one
// accepting flow per listener (net/http.Server.Serve), one flow per open
// connection that reads its requests, runs their handlers and writes the
// answers (no request waits for a worker), one flow per periodic step
// (startPeriodic), the built-in agent's own flows, and this main flow,
// which starts everything, waits for the stop signal and stops everything.
func startCentral() error {
	logger := log.New(os.Stderr, "", log.LstdFlags)
	dataDir := modules.DataDir()
	if err := modules.EnsureDataDir(dataDir); err != nil {
		return modules.NewExitErrorf(modules.ExitFileIO, "data directory %s: %v", dataDir, err)
	}
	if err := modules.EnsureDataDir(filepath.Join(dataDir, modules.TargetDBDirName)); err != nil {
		return modules.NewExitErrorf(modules.ExitFileIO, "data directory: %v", err)
	}
	dbPath := filepath.Join(dataDir, modules.CentralDBFileName)
	// A central.db of another build is refused before central.lock is
	// written, so that the refused data directory is left as it is.
	if _, err := os.Stat(dbPath); err == nil {
		if err := modules.CheckSQLiteBuild(dbPath, modules.SchemaCentral); modules.IsOtherBuildDB(err) {
			return modules.DBOpenFailed(err, modules.ExitConfig, "database")
		}
	}
	lock, err := modules.AcquireLock(filepath.Join(dataDir, modules.CentralLockFileName))
	if err != nil {
		return modules.NewExitError(modules.ExitConfig, err.Error())
	}
	defer lock.Release()
	db, err := modules.OpenSQLite(dbPath, modules.SchemaCentral)
	if err != nil {
		return modules.DBOpenFailed(err, modules.ExitConfig, "database")
	}
	defer db.Close()
	cfg, err := modules.LoadCentralConfig(db.Read())
	if err != nil {
		return modules.NewExitError(modules.ExitConfig, err.Error()+" (correct it with central config)")
	}
	// central status shows these values while a saved change waits for a
	// restart. They are recorded at once (under the lock), so that a record
	// left by a central that was killed is never shown for this one.
	if err := db.Write(func(tx models.Writer) error { return modules.SaveRunningConfig(tx, cfg) }); err != nil {
		logger.Printf("recording the start values: %v", err)
	}
	defer func() {
		if err := db.Write(func(tx models.Writer) error { return modules.ClearRunningConfig(tx) }); err != nil {
			logger.Printf("clearing the start values: %v", err)
		}
	}()

	c := newCore(db, cfg, dataDir, logger)
	defer c.closeTargets()
	if err := c.ensureServerKey(); err != nil {
		return err
	}
	if err := c.writeCLISecret(); err != nil {
		return fmt.Errorf("CLI secret: %w", err)
	}
	builtin, err := c.ensureBuiltinAgent()
	if err != nil {
		return err
	}
	if err := c.manageCertificates(); err != nil {
		return fmt.Errorf("certificates: %w", err)
	}
	// Open every per-target database now so pending migrations are applied
	// at start rather than on first use.
	if err := c.openAllTargets(); err != nil {
		return fmt.Errorf("target databases: %w", err)
	}
	spaFS, err := fs.Sub(modules.FrontendFS, "frontend/dist")
	if err != nil {
		return fmt.Errorf("embedded frontend: %w", err)
	}
	c.spaFS = spaFS

	webLn, err := listenTCP(cfg.WebListen)
	if err != nil {
		return modules.NewExitErrorf(modules.ExitConfig, "web listener %s: %v", cfg.WebListen, err)
	}
	agentLn, err := listenTCP(cfg.AgentListen)
	if err != nil {
		webLn.Close()
		return modules.NewExitErrorf(modules.ExitConfig, "agent listener %s: %v", cfg.AgentListen, err)
	}
	// Both servers serve every request at once, each connection on a flow
	// of its own; a connection is cut only after modules.IdleTimeout of
	// silence. One bound on open connections and one budget for large
	// bodies hold for both listeners.
	limit := newProcessConnLimit(logger)
	budget := newBodyBudget(modules.MaxBufferedBytes)
	web := newHTTPServer(c.webHandler(), c.admitWebBody, nil, budget, logger)
	agent := newHTTPServer(c.agentHandler(), c.admitAgentBody, c.tlsConfig(), budget, logger)
	serveErr := make(chan error, 2)
	go func() { serveErr <- web.Serve(limit.listener(webLn, modules.IdleTimeout)) }()
	go func() { serveErr <- agent.ServeTLS(limit.listener(agentLn, modules.IdleTimeout), "", "") }()

	stopBuiltin := make(chan struct{})
	waitBuiltin := c.startBuiltinAgent(builtin, stopBuiltin)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	stopPeriodic := make(chan struct{})
	waitPeriodic := c.startPeriodic(c.periodicSteps(), stopPeriodic)
	logger.Printf("%s central %s started: web http://%s, agents and management https://%s, central workers %d",
		modules.AppName, modules.AppVersion, cfg.WebListen, cfg.AgentListen, cfg.Workers)
	// Nothing about the built-in agent is logged while it is disabled.
	builtinEnabled := *c.conf().BuiltinAgent
	builtinRunning, msg := c.builtinStatus()
	if builtinEnabled && !builtinRunning {
		logger.Printf("built-in agent is not running: %s", msg.Text())
	}
	// Run until a signal, or until a listener fails for good (the library
	// retries temporary accept errors itself).
	var failed error
	select {
	case <-sigCh:
	case err := <-serveErr:
		failed = err
		logger.Printf("listener failed: %v", err)
	}
	if running, _ := c.builtinStatus(); running {
		logger.Printf("shutting down; the built-in agent ends its running commands and waits for all its workers to finish their units")
	} else {
		logger.Printf("shutting down")
	}
	// Everything is told to stop at the same moment and waited for without
	// a time limit: the servers stop accepting, close the connections that
	// have no request in work and let the requests in work finish and be
	// answered (each bounded only by its peer's idle time); the periodic
	// step in work finishes; the built-in agent ends its running commands
	// (SIGTERM, then SIGKILL after the grace) and waits for all its workers,
	// whose monitoring step in work completes. A further signal
	// changes nothing (the signal channel stays registered).
	var stopped sync.WaitGroup
	for _, srv := range []*http.Server{web, agent} {
		stopped.Add(1)
		go func() {
			defer stopped.Done()
			if err := srv.Shutdown(context.Background()); err != nil {
				logger.Printf("stopping a server: %v", err)
			}
		}()
	}
	close(stopPeriodic)
	close(stopBuiltin)
	stopped.Wait()
	waitPeriodic()
	waitBuiltin()
	logger.Printf("stopped")
	if failed != nil {
		return modules.NewExitErrorf(modules.ExitGeneral, "listener failed: %v", failed)
	}
	return nil
}
