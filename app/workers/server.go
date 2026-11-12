package workers

import (
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"mimamori/app/modules"
)

func init() {
	// Register the entry point so the CLI (modules) can start the central.
	modules.StartCentral = startCentral
}

// startCentral runs the central until SIGINT or SIGTERM. The central flow
// accepts and processes requests and periodic tasks (within the worker
// limit); the built-in agent runs on a second flow.
func startCentral() error {
	logger := log.New(os.Stderr, "", log.LstdFlags)
	dataDir := modules.DataDir()
	if err := modules.EnsureDataDir(dataDir); err != nil {
		return modules.NewExitErrorf(modules.ExitFileIO, "data directory %s: %v", dataDir, err)
	}
	if err := modules.EnsureDataDir(filepath.Join(dataDir, modules.TargetDBDirName)); err != nil {
		return modules.NewExitErrorf(modules.ExitFileIO, "data directory: %v", err)
	}
	lock, err := modules.AcquireLock(filepath.Join(dataDir, modules.CentralLockFileName))
	if err != nil {
		return modules.NewExitError(modules.ExitConfig, err.Error())
	}
	defer lock.Release()
	db, err := modules.OpenSQLite(filepath.Join(dataDir, modules.CentralDBFileName), modules.SchemaCentral)
	if err != nil {
		return modules.NewExitErrorf(modules.ExitConfig, "database: %v", err)
	}
	defer db.Close()
	cfg, err := modules.LoadCentralConfig(db)
	if err != nil {
		return modules.NewExitError(modules.ExitConfig, err.Error()+" (correct it with central config)")
	}

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
	specs := []*listenerSpec{
		{name: "web", ln: webLn, handler: c.webHandler()},
		{name: "agent", ln: agentLn, tls: c.tlsConfig(), handler: c.agentHandler()},
	}
	srv := newConnServer(specs, cfg.Workers, time.Duration(cfg.QueueTimeoutSec)*time.Second, logger)

	stopBuiltin := make(chan struct{})
	waitBuiltin := c.startBuiltinAgent(builtin, stopBuiltin)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	stopping := func() bool {
		select {
		case <-sigCh:
			return true
		default:
			return false
		}
	}
	nextTick := time.Now().Add(modules.PeriodicTick)
	tick := func() func() {
		if time.Now().Before(nextTick) {
			return nil
		}
		nextTick = time.Now().Add(modules.PeriodicTick)
		return c.periodicTasks
	}
	// central status shows these values while a saved change waits for a
	// restart.
	if err := modules.SaveRunningConfig(db, cfg); err != nil {
		logger.Printf("recording the start values: %v", err)
	}
	defer func() {
		if err := modules.ClearRunningConfig(db); err != nil {
			logger.Printf("clearing the start values: %v", err)
		}
	}()
	logger.Printf("%s central %s started: web http://%s, agents and management https://%s, workers %d",
		modules.AppName, modules.AppVersion, cfg.WebListen, cfg.AgentListen, cfg.Workers)
	// Nothing about the built-in agent is logged while it is disabled.
	builtinEnabled := *c.conf().BuiltinAgent
	builtinRunning, msg := c.builtinStatus()
	if builtinEnabled && !builtinRunning {
		logger.Printf("built-in agent is not running: %s", msg)
	}
	srv.run(stopping, tick)
	if builtinRunning {
		logger.Printf("shutting down; waiting for the built-in agent to finish its cycle")
	} else {
		logger.Printf("shutting down")
	}
	close(stopBuiltin)
	waitBuiltin()
	logger.Printf("stopped")
	return nil
}
