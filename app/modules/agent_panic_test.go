package modules

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime/debug"
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
	rt.link.syncInterval = 1
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

// TestPanicWhenRecordingTheGroup: a panic after the command was started at
// its gate, while the run records its process group, gives the command up
// before its release: nothing of it runs, nothing is signalled, and the run
// is closed with internal_error without a result of that command. A second
// request for the same set runs afterwards.
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
	panicLogHas(t, logged.String(), "worker panic: test panic while the process group is recorded", "recordCommandStart")
	if n := signals.Load(); n != 0 {
		t.Fatalf("%d signals were sent to commands", n)
	}
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); got != "start\nend\n" {
		t.Fatalf("log of the command: %q, want only the second run", got)
	}
	runs := map[int64]CommandRunReport{}
	for _, r := range runReports(allReports(t, rt, tr)) {
		if r.Finished {
			runs[r.RequestID] = r
		}
	}
	r := runs[1]
	if r.State != ResultError || r.Reason != ReasonInternalError || len(r.Results) != 0 {
		t.Fatalf("the run the panic hit: %+v", r)
	}
	if r2 := runs[2]; r2.State != ResultSuccess || r2.StartedAt < r.FinishedAt {
		t.Fatalf("the second run of the set: %+v (first ended at %d)", r2, r.FinishedAt)
	}
	if open, _ := models.ListUnfinishedCommandExecutions(rt.DB.Read()); len(open) != 0 {
		t.Fatalf("unfinished runs: %d", len(open))
	}
}

// TestPanicTwiceInTheWaitForACommand: when the wait for a command panics and
// the plain wait after it panics again, the run is left unfinished (the next
// start reports it as interrupted) and its command is let go without a
// signal: its output is still read and dropped (it writes more than the pipe
// holds and is not blocked), and the set does not start again until the
// command's process group has no process that has not ended (the second
// request for the set runs after it).
func TestPanicTwiceInTheWaitForACommand(t *testing.T) {
	requireBash(t)
	tr := &parTransport{}
	rt := newParallelRuntime(t, tr, 2)
	var logged lockedLog
	rt.Logger = logged.logger()
	start := time.Now()
	// Every wait for the first command that runs at 300 ms panics.
	var target atomic.Int64
	var panics atomic.Int32
	wait4 = func(pid int, ws *syscall.WaitStatus, options int, ru *syscall.Rusage) (int, error) {
		if time.Since(start) > 300*time.Millisecond {
			target.CompareAndSwap(0, int64(pid))
		}
		if int64(pid) == target.Load() {
			panics.Add(1)
			panic("test panic in the wait for a command")
		}
		return syscall.Wait4(pid, ws, options, ru)
	}
	t.Cleanup(func() { wait4 = syscall.Wait4 })
	t.Cleanup(func() {
		if pid := target.Load(); pid > 0 {
			_ = syscall.Kill(-int(pid), syscall.SIGKILL)
		}
	})
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
		fmt.Sprintf("echo start >> %[1]s; sleep 1; head -c 4194304 /dev/zero; sleep 1; echo end >> %[1]s", logPath)))
	tr.resp.CommandRequests = manualRequests(1, 1)
	tr.onSync = func(n int) {
		if n == 1 {
			tr.mu.Lock()
			tr.resp.CommandRequests = manualRequests(1, 1, 1)
			tr.mu.Unlock()
		}
	}
	rt.Run(stopWhenRunsFinished(t, rt.DB, 1))
	if n := panics.Load(); n < 2 {
		t.Fatalf("panics: %d, want the wait and the wait after it", n)
	}
	logs := logged.String()
	for _, want := range []string{"worker panic: test panic in the wait for a command", "is left unfinished",
		"the set does not start until that group is gone"} {
		if !strings.Contains(logs, want) {
			t.Fatalf("log lacks %q:\n%s", want, logs)
		}
	}
	if n := signals.Load(); n != 0 {
		t.Fatalf("%d signals were sent to commands", n)
	}
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); got != "start\nend\nstart\nend\n" {
		t.Fatalf("log of the command: %q, want the first command to its end and then the second run", got)
	}
	open, _ := models.ListUnfinishedCommandExecutions(rt.DB.Read())
	if len(open) != 1 || open[0].RequestID != 1 {
		t.Fatalf("unfinished runs: %+v, want the first one", open)
	}
	if n := rt.leftovers.held(); n != 0 {
		t.Fatalf("held pipes after Run: %d", n)
	}
}

// panicOnceFrom returns a function that panics with msg the first time it is
// called from a function whose name contains caller (looked up on the
// stack), and otherwise calls through.
func panicOnceFrom[T any](caller, msg string, through func() T) (fn func() T, injected *atomic.Bool) {
	injected = new(atomic.Bool)
	return func() T {
		if !injected.Load() && strings.Contains(string(debug.Stack()), caller) && injected.CompareAndSwap(false, true) {
			panic(msg)
		}
		return through()
	}, injected
}

// TestPanicWhenPickingAnEntry: a panic while a worker takes an entry from
// the queue (the queue's lock is held) is logged with its stack; the lock is
// released, the worker waits a second and goes on: the run of a request is
// processed, the rounds go on and the stop ends the agent.
func TestPanicWhenPickingAnEntry(t *testing.T) {
	requireBash(t)
	tr := &parTransport{}
	rt := newParallelRuntime(t, tr, 1)
	var logged lockedLog
	rt.Logger = logged.logger()
	now, injected := panicOnceFrom("(*agentScheduler).next", "test panic while an entry is picked", time.Now)
	rt.sched.now = now
	tr.resp.CommandSets = defsOf(testSet(1, PurposeRunOnly, "echo done"))
	tr.resp.CommandRequests = manualRequests(1, 1)
	rt.Run(stopWhenRunsFinished(t, rt.DB, 1))
	if !injected.Load() {
		t.Fatal("the panic was not injected")
	}
	panicLogHas(t, logged.String(), "worker panic: test panic while an entry is picked", "(*agentScheduler).next")
	last, _ := lastRun(t, runReports(allReports(t, rt, tr)))
	if last.State != ResultSuccess {
		t.Fatalf("the run after the panic: %+v", last)
	}
}

// TestPanicInTheBookkeepingAfterARun: a panic in the bookkeeping after a run
// (here when the next entry of the scheduled set is computed) is logged;
// the set is no longer marked as running (a manual request of it runs), and
// after a second the worker does the bookkeeping again: the set gets the
// entry of its next occurrence.
func TestPanicInTheBookkeepingAfterARun(t *testing.T) {
	requireBash(t)
	tr := &parTransport{}
	rt := newParallelRuntime(t, tr, 1)
	var logged lockedLog
	rt.Logger = logged.logger()
	set := testSet(1, PurposeRunOnly, "echo run")
	set.RunMode = RunModeSchedule
	set.Schedule = &CommandSchedule{Kind: ScheduleDaily, Time: "00:00"}
	rt.storeCommandDefinitions(defsOf(set))
	execOnDB(t, rt.DB, `UPDATE command_schedules SET armed_at = armed_at - 2*86400`)
	rt = NewAgentRuntime(rt.DB, tr, logged.logger())
	rt.link.syncInterval = 1
	rt.cmdKillGrace = 300 * time.Millisecond
	now, injected := panicOnceFrom("(*agentScheduler).doneRun", "test panic in the bookkeeping after a run", time.Now)
	rt.sched.now = now
	tr.resp.CommandSets = defsOf(set)
	tr.onSync = func(n int) {
		if injected.Load() {
			tr.mu.Lock()
			tr.resp.CommandRequests = manualRequests(1, 1)
			tr.mu.Unlock()
		}
	}
	rt.Run(stopWhenRunsFinished(t, rt.DB, 2))
	if !injected.Load() {
		t.Fatal("the panic was not injected")
	}
	panicLogHas(t, logged.String(), "worker panic: test panic in the bookkeeping after a run", "(*agentScheduler).doneRun")
	tomorrow := time.Now().Add(24 * time.Hour)
	midnight := time.Date(tomorrow.Year(), tomorrow.Month(), tomorrow.Day(), 0, 0, 0, 0, time.Local)
	if e := entryOf(rt, 1); e == nil || e.scheduledFor != midnight.Unix() {
		t.Fatalf("the entry of the next occurrence: %+v", e)
	}
	runs := map[string]bool{}
	for _, r := range runReports(allReports(t, rt, tr)) {
		if r.Finished && r.State == ResultSuccess {
			runs[r.Trigger] = true
		}
	}
	if !runs[TriggerSchedule] || !runs[TriggerManual] {
		t.Fatalf("runs: %v, want the scheduled run and the manual one after it", runs)
	}
}

// TestPanicInAPipeReader: a panic in the reader of a held pipe is logged
// with its stack; after a second the reader reads on, so that the process
// left behind writes more than the pipe holds and is not blocked, and the
// reader ends with the pipe.
func TestPanicInAPipeReader(t *testing.T) {
	requireBash(t)
	dir := t.TempDir()
	after := filepath.Join(dir, "after")
	var logged lockedLog
	held := leftoverPipes{logf: logged.logger().Printf}
	var injected atomic.Bool
	leftoverRead = func(f *os.File, b []byte) (int, error) {
		if injected.CompareAndSwap(false, true) {
			panic("test panic in a pipe reader")
		}
		return f.Read(b)
	}
	t.Cleanup(func() { leftoverRead = (*os.File).Read })
	line := fmt.Sprintf(`( sleep 3; head -c 4194304 /dev/zero; touch %s ) & echo early`, after)
	r := ExecuteCommand(CommandExecInput{OSUser: agentUserName(), WorkDir: dir, Command: testCommand(line), Leftover: held.add})
	if r.Kind != ResultSuccess || held.held() != 1 {
		t.Fatalf("result %+v, %d pipes held", r, held.held())
	}
	waitFile(t, after)
	waitFor(t, "the reader to end with the pipe", func() bool { return held.held() == 0 })
	if !injected.Load() {
		t.Fatal("the panic was not injected")
	}
	panicLogHas(t, logged.String(), "worker panic: test panic in a pipe reader", "(*leftoverPipes).readToEnd")
}

// TestPanicInTheCommunicationLoop: a panic in the communication flow's own
// loop (outside a round) is logged with its stack; the flow waits a second
// and goes on: the rounds go on, and at the stop the last report says that
// the agent stops.
func TestPanicInTheCommunicationLoop(t *testing.T) {
	tr := &parTransport{}
	rt := newParallelRuntime(t, tr, 1)
	var logged lockedLog
	rt.Logger = logged.logger()
	var turns atomic.Int32
	roundsHook = func() {
		if turns.Add(1) == 2 {
			panic("test panic in the communication loop")
		}
	}
	t.Cleanup(func() { roundsHook = nil })
	stop, _ := stopWhen(t, func() bool { return len(tr.seen()) >= 3 })
	rt.Run(stop)
	panicLogHas(t, logged.String(), "communication panic: test panic in the communication loop", "(*AgentRuntime).communicateOnce")
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if len(tr.batches) == 0 || !tr.batches[len(tr.batches)-1].Stopping {
		t.Fatal("the last report must say that the agent stops")
	}
}

// panicWriter is a log writer that panics once when a line holds on.
type panicWriter struct {
	on   string
	done atomic.Bool
}

func (w *panicWriter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), w.on) && w.done.CompareAndSwap(false, true) {
		panic("test panic: " + w.on)
	}
	return len(p), nil
}

// TestPanicInScheduledRunBetweenCommands: a panic in a scheduled run while no
// command runs (right after its start) closes the run with internal_error,
// leaves nothing unfinished, and the set gets the entry of its next
// occurrence.
func TestPanicInScheduledRunBetweenCommands(t *testing.T) {
	requireBash(t)
	base := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	rt := newCommandRuntime(t, &cmdTransport{})
	rt.loc = time.UTC
	now := base.Add(-time.Minute)
	rt.clock = func() time.Time { return now }
	rt.Logger = log.New(&panicWriter{on: "started (schedule"}, "", 0)
	d := testSet(1, PurposeRunOnly, "echo one", "echo two")
	d.Schedule, d.RunMode = &CommandSchedule{Kind: ScheduleEveryMinutes, Every: 5}, RunModeSchedule
	rt.storeCommandDefinitions(defsOf(d))
	now = base.Add(10 * time.Second)
	if !runNext(rt) {
		t.Fatal("no run")
	}
	last, executed := lastRun(t, runReports(queuedReports(t, rt)))
	if last.Reason != ReasonInternalError || last.State != ResultError || executed != 0 {
		t.Fatalf("the closing report: %+v (%d results)", last, executed)
	}
	if open, _ := models.ListUnfinishedCommandExecutions(rt.DB.Read()); len(open) != 0 {
		t.Fatalf("unfinished runs: %d", len(open))
	}
	if e := entryOf(rt, 1); e == nil || e.scheduledFor != base.Add(5*time.Minute).Unix() {
		t.Fatalf("the next entry: %+v", e)
	}
}
