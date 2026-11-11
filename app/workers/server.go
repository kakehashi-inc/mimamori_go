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
func startCentral(configPath string) error {
	logger := log.New(os.Stderr, "", log.LstdFlags)
	cfg, warnings, err := modules.LoadCentralConfig(configPath)
	if err != nil {
		return modules.NewExitError(modules.ExitConfig, err.Error())
	}
	for _, w := range warnings {
		logger.Printf("warning: %s", w)
	}
	dataDir := modules.DataDir(modules.DefaultCentralDataDir)
	if err := modules.EnsureSecureDir(dataDir); err != nil {
		return modules.NewExitErrorf(modules.ExitFileIO, "data directory %s: %v", dataDir, err)
	}
	if err := modules.EnsureSecureDir(filepath.Join(dataDir, modules.TargetDBDirName)); err != nil {
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

	c := newCore(db, cfg, dataDir, logger)
	defer c.closeTargets()
	if err := c.ensureServerKey(); err != nil {
		return err
	}
	builtin, err := c.ensureBuiltinAgent()
	if err != nil {
		return err
	}
	if err := c.manageCertificates(); err != nil {
		return fmt.Errorf("certificates: %w", err)
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
	logger.Printf("%s central %s started: web http://%s, agents and management https://%s, workers %d",
		modules.AppName, modules.AppVersion, cfg.WebListen, cfg.AgentListen, cfg.Workers)
	if running, msg := c.builtinStatus(); !running {
		logger.Printf("built-in agent is not running: %s", msg)
	}
	srv.run(stopping, tick)
	logger.Printf("shutting down; waiting for the built-in agent to finish its cycle")
	close(stopBuiltin)
	waitBuiltin()
	logger.Printf("stopped")
	return nil
}
