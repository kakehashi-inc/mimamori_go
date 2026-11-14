package workers

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"mimamori/app/models"
	"mimamori/app/modules"
)

// fakeAuditTools puts an auditctl that reports a running, enabled auditd
// without rules, and an ausearch that finds nothing, first in PATH.
func fakeAuditTools(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	tools := map[string]string{
		"auditctl": "#!/bin/sh\ncase \"$1\" in\n-s) printf 'enabled 1\\npid 123\\n' ;;\n-l) echo 'No rules' ;;\nesac\nexit 0\n",
		"ausearch": "#!/bin/sh\nexit 0\n",
	}
	for name, script := range tools {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// builtinWorkersStored reads the saved worker count of builtin-agent.db.
func builtinWorkersStored(t *testing.T, dir string) string {
	t.Helper()
	db, err := modules.OpenSQLite(filepath.Join(dir, modules.BuiltinAgentLocalDBName), modules.SchemaAgentLocal)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	return models.GetLocalState(db.Read(), "workers")
}

// The built-in agent runs with its own worker count from builtin-agent.db
// and reports it; the central's item workers is a different setting: setting
// one changes nothing of the other.
func TestBuiltinAgentWorkersIndependent(t *testing.T) {
	e := newTestEnv(t)
	b, _ := models.GetBuiltinAgent(e.c.db.Read())
	fakeAuditTools(t)
	dir := e.c.dataDir

	// The central's workers 5, saved and in use.
	if _, err := e.c.applyConfigChanges(map[string]string{"workers": "5"}, nil); err != nil {
		t.Fatal(err)
	}
	cfg := *e.c.conf()
	cfg.Workers = 5
	enabled := true
	cfg.BuiltinAgent = &enabled
	e.c.cfg.Store(&cfg)
	centralWorkers := func() string {
		var v string
		_ = e.c.db.Read().QueryRow(`SELECT value FROM settings WHERE key = 'config.workers'`).Scan(&v)
		return v
	}
	before := centralWorkers()

	// The built-in agent's 2, set on the central's host.
	if err := modules.AgentSetWorkers(dir, "2", true); err != nil {
		t.Fatal(err)
	}
	if got := centralWorkers(); got != before || e.c.conf().Workers != 5 {
		t.Fatalf("central workers changed by agent set-workers --builtin: %q -> %q", before, got)
	}

	stop := make(chan struct{})
	wait := e.c.startBuiltinAgent(b, stop)
	reported := 0
	deadline := time.Now().Add(30 * time.Second)
	for reported == 0 && time.Now().Before(deadline) {
		if tdb, err := e.c.targetDB(b); err == nil {
			if rep, err := models.LatestHostReport(tdb.Read()); err == nil {
				var hi modules.HostInfo
				if json.Unmarshal([]byte(rep.HostInfo), &hi) == nil {
					reported = hi.Workers
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	close(stop)
	wait()
	if reported != 2 {
		t.Fatalf("the built-in agent reported workers %d, want 2", reported)
	}
	// Its host information on the agent's screen (and in ctl agents show).
	detail := e.admin("GET", "/agents/"+itoa64(b.ID), nil)
	if hi, _ := detail["host_info"].(map[string]any); hi == nil || hi["workers"] != float64(2) {
		t.Fatalf("agent detail host_info: %v", detail["host_info"])
	}

	// Changing the central's item leaves the built-in agent's count.
	if _, err := e.c.applyConfigChanges(map[string]string{"workers": "7"}, nil); err != nil {
		t.Fatal(err)
	}
	if v := builtinWorkersStored(t, dir); v != "2" {
		t.Fatalf("builtin-agent.db workers after changing the central's: %q", v)
	}
	if _, err := os.Stat(filepath.Join(dir, modules.AgentLocalDBFileName)); err == nil {
		t.Fatal("agent.db created on the central's host")
	}
}

// A panic of a worker of the built-in agent is logged with its stack under
// the built-in agent's prefix and ends the built-in agent: the central keeps
// running and the screen shows that the built-in agent stopped.
func TestBuiltinAgentPanicStopsOnlyIt(t *testing.T) {
	e := newTestEnv(t)
	b, _ := models.GetBuiltinAgent(e.c.db.Read())
	fakeAuditTools(t)
	var logged lockedLog
	e.c.logger = log.New(&logged, "", 0)
	enabled := true
	e.c.conf().BuiltinAgent = &enabled
	builtinSyncHook = func() { panic("test panic in a sync") }
	t.Cleanup(func() { builtinSyncHook = nil })
	stop := make(chan struct{})
	defer close(stop)
	wait := e.c.startBuiltinAgent(b, stop)
	ended := make(chan struct{})
	go func() {
		wait()
		close(ended)
	}()
	select {
	case <-ended:
	case <-time.After(30 * time.Second):
		t.Fatal("the built-in agent did not end after the panic")
	}
	if running, msg := e.c.builtinStatus(); running || msg.Code != "builtin.stopped" {
		t.Fatalf("built-in agent status: running %v, %+v", running, msg)
	}
	out := logged.String()
	for _, want := range []string{"worker panic: test panic in a sync", "(*AgentRuntime).safeWork", modules.ErrAgentPanic.Error()} {
		i := strings.Index(out, want)
		if i < 0 {
			t.Fatalf("log lacks %q:\n%s", want, out)
		}
		if line := out[strings.LastIndex(out[:i], "\n")+1:]; !strings.HasPrefix(line, "[builtin-agent] ") && want != "(*AgentRuntime).safeWork" {
			t.Fatalf("%q is not logged by the built-in agent:\n%s", want, out)
		}
	}
	// The central goes on answering.
	if code := e.adminStatus("GET", "/agents", nil); code != http.StatusOK {
		t.Fatalf("GET /agents after the panic: %d", code)
	}
}

// lockedLog is a buffer that several flows write log lines to.
type lockedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}
