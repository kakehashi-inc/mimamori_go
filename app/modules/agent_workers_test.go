package modules

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mimamori/app/models"
)

// workersOutput runs fn and returns what it printed on standard output.
func workersOutput(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	prev := os.Stdout
	os.Stdout = w
	runErr := fn()
	os.Stdout = prev
	w.Close()
	b, _ := io.ReadAll(r)
	r.Close()
	return string(b), runErr
}

// wantExit fails unless err is an ExitError with code.
func wantExit(t *testing.T, err error, code int) *ExitError {
	t.Helper()
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != code {
		t.Fatalf("want exit code %d, got %v", code, err)
	}
	return ee
}

// newLocalDB creates an empty local database at path.
func newLocalDB(t *testing.T, path string) {
	t.Helper()
	MigrationsFS = os.DirFS("../..")
	db, err := OpenSQLite(path, SchemaAgentLocal)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
}

// localState reads a state key of the local database at path ("" when
// absent) and whether it exists.
func localState(t *testing.T, path, key string) (string, bool) {
	t.Helper()
	db, err := OpenSQLite(path, SchemaAgentLocal)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	v, found, err := models.FindLocalState(db.Read(), key)
	if err != nil {
		t.Fatal(err)
	}
	return v, found
}

// agent set-workers checks its argument and the data directory, saves the
// count in agent.db and says which count a running agent uses; agent status
// shows the saved count, and the count in use when it differs.
func TestAgentSetWorkers(t *testing.T) {
	MigrationsFS = os.DirFS("../..")
	prevDir := dataDirOverride
	t.Cleanup(func() { SetDataDir(prevDir) })
	dir := t.TempDir()
	SetDataDir(dir)

	// The argument is a whole number of the digits 0-9 from 1 to 64
	// (leading zeros are decimal); every other value is refused with the
	// agent's own message and exit code 2, before the data directory is
	// looked at.
	const refusal = "the agent's worker count must be a whole number from 1 to 64"
	for _, arg := range []string{"0", "-1", "65", "abc", "1.5", "0x10", "", "+3", " 3", "3 ", "1_0", "1e1", "\uff13", "99999999999999999999"} {
		for _, builtin := range []bool{false, true} {
			ee := wantExit(t, AgentSetWorkers(dir, arg, builtin), ExitArgument)
			if ee.Error() != refusal {
				t.Fatalf("message for %q (builtin %v): %q, want %q", arg, builtin, ee.Error(), refusal)
			}
		}
	}
	for arg, want := range map[string]int{"064": 64, "010": 10, "1": 1} {
		if n, err := parseWorkersArg(arg); err != nil || n != want {
			t.Fatalf("%s: %d %v, want %d", arg, n, err, want)
		}
	}
	ee := wantExit(t, AgentSetWorkers(dir, "2", false), ExitConfig)
	if ee.Error() != ErrAgentNotInitialized.Error() {
		t.Fatalf("without agent.db: %q", ee.Error())
	}

	path := filepath.Join(dir, AgentLocalDBFileName)
	newLocalDB(t, path)
	out, err := workersOutput(t, func() error { return AgentSetWorkers(dir, "4", false) })
	if err != nil {
		t.Fatal(err)
	}
	if want := "Workers of the agent set to 4 (was 1). It takes effect when the agent starts.\n"; out != want {
		t.Fatalf("output %q, want %q", out, want)
	}
	if v, _ := localState(t, path, stateWorkers); v != "4" {
		t.Fatalf("stored %q", v)
	}
	out, err = workersOutput(t, (&AgentStatusCmd{}).Run)
	_ = wantExit(t, err, ExitNotRunning)
	if !strings.Contains(out, "\nagent workers:   4\n") {
		t.Fatalf("status of a stopped agent:\n%s", out)
	}

	// A running agent (holding agent.lock) that uses 1.
	lock, err := AcquireLock(filepath.Join(dir, AgentLockFileName))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	db, err := OpenSQLite(path, SchemaAgentLocal)
	if err != nil {
		t.Fatal(err)
	}
	if err := recordRunningWorkers(db, 1); err != nil {
		t.Fatal(err)
	}
	db.Close()
	out, err = workersOutput(t, (&AgentStatusCmd{}).Run)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "\nagent workers:   1 (saved: 4, takes effect after restart)\n") {
		t.Fatalf("status of a running agent:\n%s", out)
	}
	out, err = workersOutput(t, func() error { return AgentSetWorkers(dir, "3", false) })
	if err != nil {
		t.Fatal(err)
	}
	if want := "Workers of the agent set to 3 (was 4). The running agent uses 1 until it is restarted.\n"; out != want {
		t.Fatalf("output %q, want %q", out, want)
	}
	// A value left behind without the lock is not shown.
	lock.Release()
	out, _ = workersOutput(t, (&AgentStatusCmd{}).Run)
	if !strings.Contains(out, "\nagent workers:   3\n") {
		t.Fatalf("status after the agent ended:\n%s", out)
	}

	// An invalid stored value is shown once after the label, with the
	// command that repairs it; while the agent runs, after the count in
	// use.
	setLocalStateAt(t, path, stateWorkers, "abc")
	const stored = `stored value "abc" is not a whole number from 1 to 64; set it with agent set-workers N`
	out, _ = workersOutput(t, (&AgentStatusCmd{}).Run)
	if !strings.Contains(out, "\nagent workers:   "+stored+"\n") || strings.Count(out, "workers:") != 1 {
		t.Fatalf("status with an invalid stored value:\n%s", out)
	}
	lock, err = AcquireLock(filepath.Join(dir, AgentLockFileName))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	out, _ = workersOutput(t, (&AgentStatusCmd{}).Run)
	if !strings.Contains(out, "\nagent workers:   1 ("+stored+")\n") || strings.Count(out, "workers:") != 1 {
		t.Fatalf("status of a running agent with an invalid stored value:\n%s", out)
	}
}

// setLocalStateAt sets a state key of the local database at path.
func setLocalStateAt(t *testing.T, path, key, value string) {
	t.Helper()
	db, err := OpenSQLite(path, SchemaAgentLocal)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Write(func(tx models.Writer) error { return models.SetLocalState(tx, key, value) }); err != nil {
		t.Fatal(err)
	}
}

// agent set-workers --builtin needs a central in the data directory, creates
// builtin-agent.db when missing and leaves the central's settings (its own
// item workers included) as they are.
func TestAgentSetWorkersBuiltin(t *testing.T) {
	MigrationsFS = os.DirFS("../..")
	dir := t.TempDir()
	ee := wantExit(t, AgentSetWorkers(dir, "2", true), ExitConfig)
	if want := "the data directory " + dir + " holds no central (central.db not found); --builtin sets the built-in agent of a central"; ee.Error() != want {
		t.Fatalf("without central.db: %q", ee.Error())
	}
	if _, err := os.Stat(filepath.Join(dir, BuiltinAgentLocalDBName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("builtin-agent.db created without a central: %v", err)
	}

	central := filepath.Join(dir, CentralDBFileName)
	cdb, err := OpenSQLite(central, SchemaCentral)
	if err != nil {
		t.Fatal(err)
	}
	if err := cdb.Write(func(tx models.Writer) error {
		_, err := ApplyCentralConfigChanges(tx, map[string]string{"retention_days": "90"}, nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	settings := func() string {
		rows, err := cdb.Read().Query(`SELECT key || '=' || value FROM settings ORDER BY key`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var b strings.Builder
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			b.WriteString(s + "\n")
		}
		return b.String()
	}
	before := settings()
	out, err := workersOutput(t, func() error { return AgentSetWorkers(dir, "2", true) })
	if err != nil {
		t.Fatal(err)
	}
	if want := "Workers of the built-in agent set to 2 (was 1). It takes effect when the central starts.\n"; out != want {
		t.Fatalf("output %q, want %q", out, want)
	}
	builtin := filepath.Join(dir, BuiltinAgentLocalDBName)
	if v, _ := localState(t, builtin, stateWorkers); v != "2" {
		t.Fatalf("builtin-agent.db holds %q", v)
	}
	if after := settings(); after != before {
		t.Fatalf("central settings changed:\n%s\n%s", before, after)
	}
	if c, err := LoadCentralConfig(cdb.Read()); err != nil || c.RetentionDays != 90 {
		t.Fatalf("central configuration: %+v %v", c, err)
	}
	cdb.Close()
	// No agent.db came into being, and the agent's own count is not set.
	if _, err := os.Stat(filepath.Join(dir, AgentLocalDBFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("agent.db: %v", err)
	}

	// The central runs with the built-in agent using 2.
	lock, err := AcquireLock(filepath.Join(dir, CentralLockFileName))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	db, err := OpenSQLite(builtin, SchemaAgentLocal)
	if err != nil {
		t.Fatal(err)
	}
	if err := recordRunningWorkers(db, 2); err != nil {
		t.Fatal(err)
	}
	db.Close()
	out, err = workersOutput(t, func() error { return AgentSetWorkers(dir, "6", true) })
	if err != nil {
		t.Fatal(err)
	}
	if want := "Workers of the built-in agent set to 6 (was 2). The running built-in agent uses 2 until the central is restarted.\n"; out != want {
		t.Fatalf("output %q, want %q", out, want)
	}
}

// agent set-workers --builtin refuses a central.db of another build, as
// every other command does, and creates no builtin-agent.db then.
func TestAgentSetWorkersBuiltinOtherBuild(t *testing.T) {
	MigrationsFS = os.DirFS("../..")
	dir := t.TempDir()
	central := filepath.Join(dir, CentralDBFileName)
	cdb, err := OpenSQLite(central, SchemaCentral)
	if err != nil {
		t.Fatal(err)
	}
	if err := execWrite(cdb, `PRAGMA user_version = 0`); err != nil {
		t.Fatal(err)
	}
	cdb.Close()
	ee := wantExit(t, AgentSetWorkers(dir, "2", true), ExitConfig)
	if !strings.Contains(ee.Error(), central+" was created by another build") {
		t.Fatalf("message: %q", ee.Error())
	}
	if _, err := os.Stat(filepath.Join(dir, BuiltinAgentLocalDBName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("builtin-agent.db created in a refused directory: %v", err)
	}
}

// agent set-workers --builtin in a data directory whose central.db is an
// empty file: there is no central, so the command refuses with exit code 3
// and writes nothing (the empty file is not made a central database, and
// no other file comes into being).
func TestAgentSetWorkersBuiltinEmptyCentral(t *testing.T) {
	MigrationsFS = os.DirFS("../..")
	dir := t.TempDir()
	central := filepath.Join(dir, CentralDBFileName)
	if err := os.WriteFile(central, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	list := func() string {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, e := range entries {
			info, err := e.Info()
			if err != nil {
				t.Fatal(err)
			}
			names = append(names, fmt.Sprintf("%s %d", e.Name(), info.Size()))
		}
		return strings.Join(names, ", ")
	}
	before := list()
	ee := wantExit(t, AgentSetWorkers(dir, "2", true), ExitConfig)
	if want := "the data directory " + dir + " holds no central (central.db is empty); --builtin sets the built-in agent of a central"; ee.Error() != want {
		t.Fatalf("with an empty central.db: %q, want %q", ee.Error(), want)
	}
	if after := list(); after != before {
		t.Fatalf("the data directory changed: %s, before %s", after, before)
	}
}

// LoadAgentWorkers: a missing row is 1, a stored value that is not a whole
// number from 1 to 64 is an error that names it and the command that
// repairs it (with --builtin for the built-in agent); agent start refuses
// to start with it.
func TestAgentStartRejectsBadWorkers(t *testing.T) {
	rt, done := openAgentDB(t)
	if n, err := LoadAgentWorkers(rt.DB.Read(), false); err != nil || n != DefaultAgentWorkers {
		t.Fatalf("missing row: %d %v", n, err)
	}
	for _, v := range []string{"abc", "0", "65", "", "+3", " 3", "0x10"} {
		setLocalState(t, rt.DB, stateWorkers, v)
		for builtin, command := range map[bool]string{false: "agent set-workers N", true: "agent set-workers N --builtin"} {
			_, err := LoadAgentWorkers(rt.DB.Read(), builtin)
			want := `agent workers: stored value "` + v + `" is not a whole number from 1 to 64; set it with ` + command
			var bad *AgentWorkersInvalidError
			if err == nil || err.Error() != want || !errors.As(err, &bad) || bad.Value != v {
				t.Fatalf("stored %q (builtin %v): %v, want %q", v, builtin, err, want)
			}
		}
	}
	setLocalState(t, rt.DB, stateWorkers, "64")
	if n, err := LoadAgentWorkers(rt.DB.Read(), false); err != nil || n != 64 {
		t.Fatalf("64: %d %v", n, err)
	}
	done()

	// agent start with a stored "abc": exit 3 with the message, after the
	// checks of auditd (a fake auditctl and ausearch).
	fakeAuditctl(t, "1", nil)
	fakeTool(t, "ausearch", "exit 0\n")
	dir := t.TempDir()
	path := filepath.Join(dir, AgentLocalDBFileName)
	newLocalDB(t, path)
	db, err := OpenSQLite(path, SchemaAgentLocal)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &AgentConfig{AgentID: "id", CentralHost: "127.0.0.1", CentralPort: 1, Token: "t", TokenName: "n", CAPEMs: []string{"x"}}
	if err := db.Write(cfg.Save); err != nil {
		t.Fatal(err)
	}
	setLocalState(t, db, stateWorkers, "abc")
	db.Close()
	ee := wantExit(t, AgentStart(dir), ExitConfig)
	if want := `agent workers: stored value "abc" is not a whole number from 1 to 64; set it with agent set-workers N`; ee.Error() != want {
		t.Fatalf("agent start: %q", ee.Error())
	}
	// agent set-workers repairs it.
	out, err := workersOutput(t, func() error { return AgentSetWorkers(dir, "2", false) })
	if err != nil || out != "Workers of the agent set to 2 (was abc). It takes effect when the agent starts.\n" {
		t.Fatalf("repair: %q %v", out, err)
	}
}

// runningWorkersTransport records running_workers at the sync of the first
// monitoring step and then stops the agent.
type runningWorkersTransport struct {
	fakeTransport
	db    *DB
	stop  chan struct{}
	value string
	found bool
}

func (r *runningWorkersTransport) Sync(req *SyncRequest) (*SyncResponse, error) {
	if r.stop != nil {
		r.value, r.found, _ = models.FindLocalState(r.db.Read(), stateRunningWorkers)
		close(r.stop)
		r.stop = nil
	}
	return r.fakeTransport.Sync(req)
}

// The host information of a report carries the worker count in use, and
// running_workers holds it while Run runs and is gone afterwards.
func TestHostInfoWorkers(t *testing.T) {
	rt, done := openAgentDB(t)
	defer done()
	fakeAuditctl(t, "1", nil)
	tr := &fakeTransport{}
	rt.comm.transport = tr
	rt.SetWorkers(3)
	rt.report(nil)
	rt.flushOutbox()
	if len(tr.reports) != 1 || tr.reports[0].HostInfo == nil || tr.reports[0].HostInfo.Workers != 3 {
		t.Fatalf("reports: %+v", tr.reports)
	}
	b, _ := json.Marshal(tr.reports[0].HostInfo)
	if !strings.Contains(string(b), `"workers":3`) {
		t.Fatalf("host_info JSON: %s", b)
	}

	rwt := &runningWorkersTransport{db: rt.DB, stop: make(chan struct{})}
	rt.comm.transport = rwt
	rt.Run(rwt.stop)
	if !rwt.found || rwt.value != "3" {
		t.Fatalf("running_workers while running: %q %v", rwt.value, rwt.found)
	}
	if _, found, _ := models.FindLocalState(rt.DB.Read(), stateRunningWorkers); found {
		t.Fatal("running_workers left after Run")
	}
}

// agent start and agent set-central refuse an agent.db of another build
// before they write agent.lock: no lock file comes into being in the
// refused data directory.
func TestAgentCommandsOtherBuildLeaveNoLock(t *testing.T) {
	MigrationsFS = os.DirFS("../..")
	fakeAuditctl(t, "1", nil)
	fakeTool(t, "ausearch", "exit 0\n")
	dir := t.TempDir()
	path := filepath.Join(dir, AgentLocalDBFileName)
	newLocalDB(t, path)
	db, err := OpenSQLite(path, SchemaAgentLocal)
	if err != nil {
		t.Fatal(err)
	}
	if err := execWrite(db, `PRAGMA user_version = 0`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	for name, run := range map[string]func() error{
		"agent start":       func() error { return AgentStart(dir) },
		"agent set-central": func() error { return AgentSetCentral(dir, "127.0.0.1", 1) },
	} {
		ee := wantExit(t, run(), ExitConfig)
		if !strings.Contains(ee.Error(), path+" was created by another build") {
			t.Fatalf("%s: %q", name, ee.Error())
		}
		if _, err := os.Stat(filepath.Join(dir, AgentLockFileName)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s created agent.lock: %v", name, err)
		}
	}
}
