package modules

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
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

// blockingCentral answers the first sync at once; a later sync waits until
// the agent's stop cuts it (cutRequests), or release is closed.
type blockingCentral struct {
	mu      sync.Mutex
	first   SyncResponse
	syncs   int
	inSync  chan struct{}
	release chan struct{}
	cut     <-chan struct{}
	calls   []string
}

func (c *blockingCentral) cutRequests(stop <-chan struct{}) { c.cut = stop }

func (c *blockingCentral) note(s string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, s)
}

func (c *blockingCentral) Sync(req *SyncRequest) (*SyncResponse, error) {
	c.mu.Lock()
	c.syncs++
	n := c.syncs
	c.mu.Unlock()
	if isClosed(c.cut) {
		return nil, errRequestCut
	}
	c.note(fmt.Sprintf("sync %d", n))
	if n == 1 {
		r := c.first
		return &r, nil
	}
	if n == 2 {
		close(c.inSync)
	}
	select {
	case <-c.cut:
		c.note(fmt.Sprintf("sync %d cut", n))
		return nil, errRequestCut
	case <-c.release:
		return &SyncResponse{DefaultIntervalSec: 1}, nil
	}
}

func (c *blockingCentral) Report(b *ReportBatch) error {
	if isClosed(c.cut) && !b.Stopping {
		return errRequestCut
	}
	c.note(fmt.Sprintf("report stopping=%v", b.Stopping))
	return nil
}

func (c *blockingCentral) seen() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.calls...)
}

// TestStopCutsTheRoundInRequest: the stop comes while the communication
// flow waits for the answer to a sync and a worker runs a command. The
// command is ended at once, the sync is cut and the rest of the round is not
// sent; after the workers ended, the last request is the report with
// stopping true, and Run returns without waiting for the central.
func TestStopCutsTheRoundInRequest(t *testing.T) {
	requireBash(t)
	c := &blockingCentral{inSync: make(chan struct{}), release: make(chan struct{})}
	c.first = SyncResponse{DefaultIntervalSec: 1, CommandSets: defsOf(testSet(1, PurposeRunOnly, "sleep 30")),
		CommandRequests: manualRequests(1, 1)}
	rt := newCommandRuntime(t, c)
	rt.SetWorkers(2)
	rt.link.syncInterval = 1
	var logs lockedLog
	rt.Logger = logs.logger()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		rt.Run(stop)
	}()
	defer close(c.release)
	waitFor(t, "the run to start", func() bool { all, _ := countRuns(rt.DB); return all == 1 })
	select {
	case <-c.inSync:
	case <-time.After(20 * time.Second):
		t.Fatal("no second sync")
	}
	before := len(c.seen())
	stopAt := time.Now()
	close(stop)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run waits for the sync the stop should have cut")
	}
	took := time.Since(stopAt)
	calls := c.seen()[before:]
	if want := []string{"sync 2 cut", "report stopping=true"}; !slices.Equal(calls, want) {
		t.Fatalf("requests from the stop on: %v, want %v", calls, want)
	}
	if took > 3*time.Second {
		t.Fatalf("Run returned %s after the stop", took)
	}
	if _, finished := countRuns(rt.DB); finished != 1 {
		t.Fatalf("the run cut by the stop was not closed")
	}
	if !strings.Contains(logs.String(), "sync: cut by the agent's stop") {
		t.Fatalf("log: %q", logs.String())
	}
}
