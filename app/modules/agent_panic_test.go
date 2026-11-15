package modules

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"mimamori/app/models"
)

// panicLogHas fails unless the log holds the line of a recovered panic with
// its stack (the stack names fn).
func panicLogHas(t *testing.T, logged, line, fn string) {
	t.Helper()
	i := strings.Index(logged, line)
	if i < 0 {
		t.Fatalf("log lacks %q:\n%s", line, logged)
	}
	if !strings.Contains(logged[i:], fn) {
		t.Fatalf("the stack after %q does not name %s:\n%s", line, fn, logged)
	}
}

// waitFor waits until cond holds (at most 30 s).
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestPanicInMonitoringStep: a panic of the monitoring step is logged with
// its stack and ends only that step: the agent keeps running, the step is
// queued again and the next one does its work, the rounds go on and the
// command another worker runs is not touched.
func TestPanicInMonitoringStep(t *testing.T) {
	requireBash(t)
	tr := &parTransport{}
	rt := newParallelRuntime(t, tr, 2)
	var logged lockedLog
	rt.Logger = logged.logger()
	// A stored rule that the central no longer assigns: the monitoring step
	// removes it, and the first removal panics. (The stored rule is not
	// applied again at the start, so that no audit tool runs.)
	b, _ := json.Marshal(&RuleAssignment{ID: 3, Version: 2, Name: "web", Content: RuleContent{CheckIntervalSec: 1}})
	setLocalState(t, rt.DB, stateRule, string(b))
	rt = NewAgentRuntime(rt.DB, tr, logged.logger())
	rt.SetWorkers(2)
	rt.cmdKillGrace = 300 * time.Millisecond
	rt.link.defaultInterval = 1
	rt.mon.started = true
	var removals atomic.Int32
	rt.removeAudit = func(*AuditGlobals) error {
		if removals.Add(1) == 1 {
			panic("test panic in the monitoring step")
		}
		return nil
	}
	tr.resp.CommandSets = defsOf(testSet(1, PurposeRunOnly, "sleep 2.5; echo done"))
	tr.resp.CommandRequests = manualRequests(1, 1)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		rt.Run(stop)
	}()
	waitFor(t, "the rule removed by a later step", func() bool { return models.GetLocalState(rt.DB.Read(), stateRule) == "" })
	waitFor(t, "the run", func() bool { _, finished := countRuns(rt.DB); return finished == 1 })
	syncs := len(tr.seen())
	waitFor(t, "a round after the panic and the run", func() bool { return len(tr.seen()) > syncs })
	close(stop)
	<-done
	panicLogHas(t, logged.String(), "worker panic: test panic in the monitoring step", "monitorStep")
	if n := removals.Load(); n < 2 {
		t.Fatalf("removals: %d, want the panicking one and a later one", n)
	}
	last, _ := lastRun(t, runReports(allReports(t, rt, tr)))
	if last.State != ResultSuccess || last.Results[0].Reason != "" || string(last.Results[0].Output) != "done\n" {
		t.Fatalf("the command of the other worker: %+v", last)
	}
}

// TestPanicInRound: a panic of a round is logged with its stack and ends
// only that round: the next round comes at its time, and the command a
// worker runs meanwhile runs to its end.
func TestPanicInRound(t *testing.T) {
	requireBash(t)
	tr := &parTransport{}
	rt := newParallelRuntime(t, tr, 1)
	var logged lockedLog
	rt.Logger = logged.logger()
	tr.resp.CommandSets = defsOf(testSet(1, PurposeRunOnly, "sleep 2.5; echo done"))
	tr.resp.CommandRequests = manualRequests(1, 1)
	var panicAt atomic.Int64
	tr.onSync = func(n int) {
		if n == 2 {
			panicAt.Store(time.Now().UnixNano())
			panic("test panic in a round")
		}
	}
	rt.Run(stopWhenRunsFinished(t, rt.DB, 1))
	panicLogHas(t, logged.String(), "communication panic: test panic in a round", "(*AgentRuntime).round")
	seen := tr.seen()
	if len(seen) < 3 {
		t.Fatalf("syncs: %d, want rounds after the one that panicked", len(seen))
	}
	last, _ := lastRun(t, runReports(allReports(t, rt, tr)))
	if last.State != ResultSuccess || string(last.Results[0].Output) != "done\n" {
		t.Fatalf("the run: %+v", last)
	}
}

// TestPanicInRunWhileCommandRuns: a panic while a run's command runs is
// logged; the command is not signalled: the worker waits for it in the
// plain way, it runs to its own end and its output is kept; the command and
// the run get the state error with the reason internal_error. The agent
// keeps running and the next run is processed as usual.
func TestPanicInRunWhileCommandRuns(t *testing.T) {
	requireBash(t)
	tr := &parTransport{}
	rt := newParallelRuntime(t, tr, 1)
	var logged lockedLog
	rt.Logger = logged.logger()
	start := time.Now()
	var injected atomic.Bool
	wait4 = func(pid int, ws *syscall.WaitStatus, options int, ru *syscall.Rusage) (int, error) {
		if time.Since(start) > 300*time.Millisecond && injected.CompareAndSwap(false, true) {
			panic("test panic while a command runs")
		}
		return syscall.Wait4(pid, ws, options, ru)
	}
	t.Cleanup(func() { wait4 = syscall.Wait4 })
	var signals atomic.Int32
	killProcessGroup = func(pid int, sig syscall.Signal) error {
		if sig != 0 {
			signals.Add(1)
		}
		return syscall.Kill(pid, sig)
	}
	t.Cleanup(func() { killProcessGroup = syscall.Kill })
	tr.resp.CommandSets = defsOf(testSet(1, PurposeRunOnly, "echo begin; sleep 2; echo end", "echo second"),
		testSet(2, PurposeRunOnly, "echo next"))
	tr.resp.CommandRequests = manualRequests(1, 1, 2)
	rt.Run(stopWhenRunsFinished(t, rt.DB, 2))
	if !injected.Load() {
		t.Fatal("the panic was not injected")
	}
	panicLogHas(t, logged.String(), "worker panic: test panic while a command runs", "waitMain")
	if n := signals.Load(); n != 0 {
		t.Fatalf("%d signals were sent to commands", n)
	}
	runs := map[int64]CommandRunReport{}
	for _, r := range runReports(allReports(t, rt, tr)) {
		if r.Finished {
			runs[r.SetID] = r
		}
	}
	r := runs[1]
	if r.State != ResultError || r.Reason != ReasonInternalError || len(r.Results) != 1 {
		t.Fatalf("the run the panic hit: %+v", r)
	}
	res := r.Results[0]
	if res.Kind != ResultError || res.Reason != ReasonInternalError || res.ExitCode == nil || *res.ExitCode != 0 ||
		string(res.Output) != "begin\nend\n" || res.FinishedAt-res.StartedAt < 2 {
		t.Fatalf("the command the panic hit ran to its end: %+v %q", res, res.Output)
	}
	if r := runs[2]; r.State != ResultSuccess || string(r.Results[0].Output) != "next\n" {
		t.Fatalf("the next run: %+v", r)
	}
	if open, _ := models.ListUnfinishedCommandExecutions(rt.DB.Read()); len(open) != 0 {
		t.Fatalf("unfinished runs: %d", len(open))
	}
}

// TestPanicWhenRecordingTheGroup: a panic right after the command started,
// while the run records its process group, is handled as any panic while a
// command runs: the worker waits for the command in the plain way and closes
// the run with its result (internal_error), the command is not signalled, and
// a second request for the same set starts only after the first run ended
// (the same set never runs twice at the same time).
func TestPanicWhenRecordingTheGroup(t *testing.T) {
	requireBash(t)
	tr := &parTransport{}
	rt := newParallelRuntime(t, tr, 2)
	var logged lockedLog
	rt.Logger = logged.logger()
	// The first call is the one of the start of Run (recoverInterruptedRuns),
	// the second the one of the first run.
	var calls atomic.Int32
	var injected atomic.Bool
	bootTime = func() (int64, error) {
		if calls.Add(1) == 2 {
			injected.Store(true)
			panic("test panic while the process group is recorded")
		}
		return sysBootTime()
	}
	t.Cleanup(func() { bootTime = sysBootTime })
	var signals atomic.Int32
	killProcessGroup = func(pid int, sig syscall.Signal) error {
		if sig != 0 {
			signals.Add(1)
		}
		return syscall.Kill(pid, sig)
	}
	t.Cleanup(func() { killProcessGroup = syscall.Kill })
	logPath := filepath.Join(t.TempDir(), "log")
	tr.resp.CommandSets = defsOf(testSet(1, PurposeRunOnly,
		fmt.Sprintf("echo start >> %[1]s; echo begin; sleep 3; echo end >> %[1]s; echo done", logPath)))
	tr.resp.CommandRequests = manualRequests(1, 1)
	// The second request for the same set comes from the second sync on,
	// while the first run runs.
	tr.onSync = func(n int) {
		if n == 1 {
			tr.mu.Lock()
			tr.resp.CommandRequests = manualRequests(1, 1, 1)
			tr.mu.Unlock()
		}
	}
	rt.Run(stopWhenRunsFinished(t, rt.DB, 2))
	if !injected.Load() {
		t.Fatal("the panic was not injected")
	}
	panicLogHas(t, logged.String(), "worker panic: test panic while the process group is recorded", "recordGroup")
	if n := signals.Load(); n != 0 {
		t.Fatalf("%d signals were sent to commands", n)
	}
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); got != "start\nend\nstart\nend\n" {
		t.Fatalf("log of the command: %q, want the two runs one after the other", got)
	}
	runs := map[int64]CommandRunReport{}
	for _, r := range runReports(allReports(t, rt, tr)) {
		if r.Finished {
			runs[r.RequestID] = r
		}
	}
	r := runs[1]
	if r.State != ResultError || r.Reason != ReasonInternalError || len(r.Results) != 1 {
		t.Fatalf("the run the panic hit: %+v", r)
	}
	res := r.Results[0]
	if res.Kind != ResultError || res.Reason != ReasonInternalError || res.ExitCode == nil || *res.ExitCode != 0 ||
		string(res.Output) != "begin\ndone\n" || res.FinishedAt-res.StartedAt < 3 {
		t.Fatalf("the command the panic hit ran to its end: %+v %q", res, res.Output)
	}
	if r2 := runs[2]; r2.State != ResultSuccess || r2.StartedAt < r.FinishedAt {
		t.Fatalf("the second run of the set: %+v (first ended at %d)", r2, r.FinishedAt)
	}
	if open, _ := models.ListUnfinishedCommandExecutions(rt.DB.Read()); len(open) != 0 {
		t.Fatalf("unfinished runs: %d", len(open))
	}
}
