package modules

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The stop of the agent at each point of its work: nothing starts after the
// stop, what runs is ended, and what was started is recorded.

// sentSignals records the signals the agent sends to process groups
// (killProcessGroup), signal 0 left out.
type sentSignals struct {
	mu   sync.Mutex
	sent []sentSignal
}

type sentSignal struct {
	at   time.Time
	pgid int
	sig  syscall.Signal
}

// recordSignals replaces killProcessGroup for the test with one that records
// what it sends and then sends it.
func recordSignals(t *testing.T) *sentSignals {
	t.Helper()
	s := &sentSignals{}
	orig := killProcessGroup
	killProcessGroup = func(pid int, sig syscall.Signal) error {
		if sig != 0 {
			s.mu.Lock()
			s.sent = append(s.sent, sentSignal{at: time.Now(), pgid: -pid, sig: sig})
			s.mu.Unlock()
		}
		return orig(pid, sig)
	}
	t.Cleanup(func() { killProcessGroup = orig })
	return s
}

func (s *sentSignals) list() []sentSignal {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sentSignal(nil), s.sent...)
}

func (s *sentSignals) String() string {
	var b strings.Builder
	for _, x := range s.list() {
		fmt.Fprintf(&b, "%v to group %d; ", x.sig, x.pgid)
	}
	return b.String()
}

// stopRuntime returns a runtime with one manual request of set 1 (lines)
// queued, its stop channel and the recorder of the signals it sends.
func stopRuntime(t *testing.T, logs *lockedLog, lines ...string) (*AgentRuntime, chan struct{}, *sentSignals) {
	t.Helper()
	requireBash(t)
	rt := newCommandRuntime(t, &cmdTransport{})
	rt.Logger = logs.logger()
	stop := make(chan struct{})
	rt.stop, rt.sched.stop = stop, stop
	sig := recordSignals(t)
	rt.storeCommandDefinitions(defsOf(testSet(1, PurposeRunOnly, lines...)))
	receiveRequests(rt, manualRequests(1, 1))
	return rt, stop, sig
}

// TestStopBetweenPickAndStart: a worker took the entry of a run; the stop
// comes before the start of the run is recorded. Nothing is recorded and
// nothing starts: the manual request stays for the next delivery.
func TestStopBetweenPickAndStart(t *testing.T) {
	var logs lockedLog
	rt, stop, sig := stopRuntime(t, &logs, "sleep 30")
	u, acks := rt.sched.next()
	if u == nil || u.job == nil {
		t.Fatalf("the entry is taken: %+v", u)
	}
	close(stop)
	rt.queueAcks(acks)
	rt.process(u)
	if all, _ := countRuns(rt.DB); all != 0 || len(sig.list()) > 0 {
		t.Fatalf("after the stop a run was recorded (%d) and its command started (signals %s); log:\n%s", all, sig, logs.String())
	}
	if acks := acksOf(queuedReports(t, rt)); len(acks) != 0 {
		t.Fatalf("the request was answered: %+v", acks)
	}
	if !strings.Contains(logs.String(), `command set "setb" not started: the agent stops`) {
		t.Fatalf("log: %q", logs.String())
	}
}

// TestStopWhileStartRecordRetried: the start of a run cannot be written and
// the worker waits to write it again; the stop comes meanwhile (the database
// works again by then). The start is not written once more after the stop:
// nothing is recorded and nothing starts.
func TestStopWhileStartRecordRetried(t *testing.T) {
	var logs lockedLog
	rt, stop, sig := stopRuntime(t, &logs, "sleep 30")
	rt.recordRetry = 2 * time.Second
	job := nextCommandJob(rt)
	execOnDB(t, rt.DB, `CREATE TRIGGER fail_run BEFORE INSERT ON command_executions BEGIN SELECT RAISE(ABORT, 'injected failure'); END`)
	done := runInBackground(rt, job)
	waitFor(t, "the first failed attempt", func() bool { return strings.Contains(logs.String(), "storing it failed, tried again in 2s") })
	execOnDB(t, rt.DB, `DROP TRIGGER fail_run`)
	close(stop)
	if returned(t, done) {
		t.Fatal("the run counts as started")
	}
	if all, _ := countRuns(rt.DB); all != 0 || len(sig.list()) > 0 {
		t.Fatalf("after the stop the run was recorded (%d) and its command started (signals %s)", all, sig)
	}
	if !strings.Contains(logs.String(), `command set "setb" not started: the agent stops (recording its start failed: `) {
		t.Fatalf("log: %q", logs.String())
	}
}

// TestStopBeforeFirstCommand: the stop comes after the start of the run was
// recorded and before its first command. The command does not start; the
// run is closed as interrupted without results.
func TestStopBeforeFirstCommand(t *testing.T) {
	var logs lockedLog
	rt, stop, sig := stopRuntime(t, &logs, "sleep 30")
	rt.Logger = log.New(writerFunc(func(p []byte) (int, error) {
		if strings.Contains(string(p), "started (manual") {
			close(stop)
		}
		return logs.Write(p)
	}), "", 0)
	job := nextCommandJob(rt)
	if !rt.runCommandJob(job) {
		t.Fatal("the run counts as not started")
	}
	if len(sig.list()) > 0 {
		t.Fatalf("a command was started after the stop: signals %s", sig)
	}
	last, executed := lastRun(t, runReports(queuedReports(t, rt)))
	if executed != 0 || last.Reason != ReasonInterrupted || last.State != ResultError {
		t.Fatalf("closing report: %+v (%d results)", last, executed)
	}
}

// TestStopWhileCommandMarkRetried: the record of the command that starts
// (with its process group) cannot be written and the worker waits to write
// it again while the command waits at its gate; the stop comes. Nothing of
// the command runs and its gate is gone; the run is closed as interrupted.
func TestStopWhileCommandMarkRetried(t *testing.T) {
	var logs lockedLog
	ran := filepath.Join(t.TempDir(), "ran")
	rt, stop, sig := stopRuntime(t, &logs, "touch "+ran+"; sleep 30")
	rt.recordRetry = 2 * time.Second
	job := nextCommandJob(rt)
	execOnDB(t, rt.DB, `CREATE TRIGGER fail_mark BEFORE UPDATE OF running_position ON command_executions WHEN NEW.running_position >= 0
		BEGIN SELECT RAISE(ABORT, 'injected failure'); END`)
	done := runInBackground(rt, job)
	waitFor(t, "the first failed attempt", func() bool { return strings.Contains(logs.String(), "storing it failed, tried again in 2s") })
	execOnDB(t, rt.DB, `DROP TRIGGER fail_mark`)
	close(stop)
	if !returned(t, done) {
		t.Fatal("the run counts as not started")
	}
	if len(sig.list()) > 0 {
		t.Fatalf("the command was started after the stop: signals %s", sig)
	}
	if _, err := os.Stat(ran); err == nil {
		t.Fatal("the command line ran although its start was never recorded")
	}
	if n := gatesOf(t); n != 0 {
		t.Fatalf("gates left: %d", n)
	}
	last, executed := lastRun(t, runReports(queuedReports(t, rt)))
	if executed != 0 || last.Reason != ReasonInterrupted {
		t.Fatalf("closing report: %+v (%d results)", last, executed)
	}
	if e := executionOf(t, rt.DB); e.FinishedAt == 0 || e.RunningPosition != -1 {
		t.Fatalf("execution: %+v", e)
	}
}

// writerFunc is an io.Writer made of a function.
type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// gatesOf returns the number of the gates of commands that are children of
// the test process.
func gatesOf(t *testing.T) int {
	t.Helper()
	n := 0
	for _, pid := range strings.Fields(childrenOf(os.Getpid())) {
		if b, err := os.ReadFile("/proc/" + pid + "/cmdline"); err == nil && strings.Contains(string(b), "read -r -u 3 _") {
			n++
		}
	}
	return n
}

// childrenOf returns the process IDs of the children of pid, separated by
// spaces, from the lists of children of its threads.
func childrenOf(pid int) string {
	tasks, _ := filepath.Glob(fmt.Sprintf("/proc/%d/task/*/children", pid))
	var all []string
	for _, f := range tasks {
		if b, err := os.ReadFile(f); err == nil {
			all = append(all, strings.TrimSpace(string(b)))
		}
	}
	return strings.Join(all, " ")
}
