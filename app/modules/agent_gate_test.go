package modules

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"mimamori/app/models"
)

// A command of a run starts only after its process group is stored with the
// run, so that a set never runs next to a command of it that survived the
// agent: the command waits at its gate until then.

// TestCommandStartsAfterItsGroupIsStored: while the record of the command
// that starts and its process group cannot be written, the command does not
// run (its gate waits); the record is written again until it is stored, then
// the command runs, recorded as started when its record was stored, and its
// time limit counts from then on.
func TestCommandStartsAfterItsGroupIsStored(t *testing.T) {
	requireBash(t)
	rt := newCommandRuntime(t, &cmdTransport{})
	rt.recordRetry = 50 * time.Millisecond
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	set := testSet(1, PurposeRunOnly, fmt.Sprintf("date +%%s.%%N > %s; exec sleep 30", started))
	set.Commands[0].TimeoutSec = 1
	rt.storeCommandDefinitions(defsOf(set))
	receiveRequests(rt, manualRequests(1, 1))
	job := nextCommandJob(rt)
	execOnDB(t, rt.DB, `CREATE TRIGGER fail_group BEFORE UPDATE OF running_pgid ON command_executions WHEN NEW.running_pgid > 0
		BEGIN SELECT RAISE(ABORT, 'injected failure'); END`)
	done := runInBackground(rt, job)
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(started); err == nil {
		t.Fatal("the command ran before its process group was stored")
	}
	stored := time.Now()
	execOnDB(t, rt.DB, `DROP TRIGGER fail_group`)
	for i := 0; ; i++ {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if i == 500 {
			t.Fatal("the command did not run after its group was stored")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The start time of the running command is that of the attempt that
	// stored its record (the next start reports it as the start of an
	// interrupted command).
	runs, err := models.ListUnfinishedCommandExecutions(rt.DB.Read())
	if err != nil || len(runs) != 1 {
		t.Fatalf("unfinished runs %+v, %v", runs, err)
	}
	if runs[0].RunningStartedAt < stored.Unix() {
		t.Fatalf("the running command is recorded as started at %d, before its record was stored at %d",
			runs[0].RunningStartedAt, stored.Unix())
	}
	if !returned(t, done) {
		t.Fatal("not started")
	}
	took := time.Since(stored)
	b, err := os.ReadFile(started)
	if err != nil {
		t.Fatalf("the command did not run after its group was stored: %v", err)
	}
	at, _ := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
	if float64(stored.UnixNano())/1e9 > at {
		t.Fatalf("the command ran at %.3f, before its group was stored at %.3f", at, float64(stored.UnixNano())/1e9)
	}
	last, executed := lastRun(t, runReports(queuedReports(t, rt)))
	if executed != 1 || last.Results[len(last.Results)-1].Reason != ReasonTimeout {
		t.Fatalf("closing report: %+v", last)
	}
	// The time limit of 1 s and the grace of 300 ms count from the release.
	if took < time.Second || took > 4*time.Second {
		t.Fatalf("the run ended %s after the group was stored, want the time limit (1 s) and the grace", took)
	}
}

// gateHelperEnv names the database of the helper process of
// TestAgentKilledBeforeTheRelease.
const gateHelperEnv = "MIMAMORI_TEST_GATE_HELPER_DB"

// TestAgentKilledBeforeTheReleaseHelper is the agent killed by
// TestAgentKilledBeforeTheRelease: it runs the set of the database whose
// path the environment gives, whose process group cannot be stored.
func TestAgentKilledBeforeTheReleaseHelper(t *testing.T) {
	path := os.Getenv(gateHelperEnv)
	if path == "" {
		t.Skip("the helper process of TestAgentKilledBeforeTheRelease")
	}
	MigrationsFS = os.DirFS("../..")
	db, err := OpenSQLite(path, SchemaAgentLocal)
	if err != nil {
		t.Fatal(err)
	}
	rt := NewAgentRuntime(db, &cmdTransport{}, nil)
	rt.recordRetry = 50 * time.Millisecond
	receiveRequests(rt, manualRequests(1, 1))
	rt.runCommandJob(nextCommandJob(rt))
}

// TestAgentKilledBeforeTheRelease: the agent is killed with SIGKILL while it
// writes the process group of a command again and again (the command waits
// at its gate). Nothing of the command line runs, no process of the command
// is left, and the next start reports the run as interrupted and keeps
// nothing from starting.
func TestAgentKilledBeforeTheRelease(t *testing.T) {
	requireBash(t)
	setSubreaper(t)
	dir := t.TempDir()
	ran := filepath.Join(dir, "ran")
	MigrationsFS = os.DirFS("../..")
	dbPath := filepath.Join(dir, "agent.db")
	db, err := OpenSQLite(dbPath, SchemaAgentLocal)
	if err != nil {
		t.Fatal(err)
	}
	rt := NewAgentRuntime(db, &cmdTransport{}, nil)
	rt.storeCommandDefinitions(defsOf(testSet(1, PurposeRunOnly, fmt.Sprintf("touch %s; exec sleep 30", ran))))
	execOnDB(t, db, `CREATE TRIGGER fail_group BEFORE UPDATE OF running_pgid ON command_executions WHEN NEW.running_pgid > 0
		BEGIN SELECT RAISE(ABORT, 'injected failure'); END`)
	db.Close()
	helper := exec.Command(os.Args[0], "-test.run=^TestAgentKilledBeforeTheReleaseHelper$")
	helper.Env = append(os.Environ(), gateHelperEnv+"="+dbPath)
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	// The helper's command waits at its gate: the bash of the gate, a child
	// of the helper in a session of its own.
	var gate int
	waitFor(t, "the gate of the helper's command", func() bool {
		gate = childOf(helper.Process.Pid)
		return gate > 0
	})
	t.Cleanup(func() { _ = syscall.Kill(-gate, syscall.SIGKILL) })
	time.Sleep(300 * time.Millisecond)
	if err := helper.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = helper.Wait()
	// The gate (an orphan of this test process now) ends by itself.
	waitFor(t, "the gate to end", func() bool { return !groupExists(gate) })
	var ws syscall.WaitStatus
	_, _ = syscall.Wait4(gate, &ws, 0, nil)
	if _, err := os.Stat(ran); err == nil {
		t.Fatal("the command line ran although its process group was never stored")
	}
	db, err = OpenSQLite(dbPath, SchemaAgentLocal)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rt = NewAgentRuntime(db, &cmdTransport{}, nil)
	rt.recoverInterruptedRuns()
	last, _ := lastRun(t, runReports(queuedReports(t, rt)))
	if last.Reason != ReasonInterrupted {
		t.Fatalf("the run of the killed agent: %+v", last)
	}
	receiveRequests(rt, manualRequests(2, 1))
	if job := nextCommandJob(rt); job == nil {
		t.Fatal("the set is kept from starting although nothing of it runs")
	}
	if open, _ := models.ListUnfinishedCommandExecutions(db.Read()); len(open) != 0 {
		t.Fatalf("unfinished runs: %d", len(open))
	}
}

// childOf returns a child process of pid (0 when it has none).
func childOf(pid int) int {
	if f := strings.Fields(childrenOf(pid)); len(f) > 0 {
		n, _ := strconv.Atoi(f[0])
		return n
	}
	return 0
}
