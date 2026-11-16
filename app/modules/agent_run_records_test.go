package modules

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"mimamori/app/models"
)

// execOnDB runs one statement on the agent's local database in a write
// transaction of its own.
func execOnDB(t *testing.T, db *DB, stmt string) {
	t.Helper()
	if err := db.Write(func(tx models.Writer) error { _, err := tx.Exec(stmt); return err }); err != nil {
		t.Fatal(err)
	}
}

// dueScheduledJob stores a set scheduled daily at 00:00, armed two days ago
// (today's occurrence passed and was not run), and returns its job.
func dueScheduledJob(t *testing.T, rt *AgentRuntime, lines ...string) *commandJob {
	t.Helper()
	set := testSet(1, PurposeRunOnly, lines...)
	set.RunMode = RunModeSchedule
	set.Schedule = &CommandSchedule{Kind: ScheduleDaily, Time: "00:00"}
	rt.storeCommandDefinitions(defsOf(set))
	execOnDB(t, rt.DB, `UPDATE command_schedules SET armed_at = armed_at - 2*86400`)
	rt.sched.acceptDefinitions(rt.sched.defs, rt.sched.valid)
	job := nextCommandJob(rt)
	if job == nil {
		t.Fatal("the scheduled set is not due")
	}
	return job
}

// lastDue returns the schedule mark of set 1.
func lastDue(t *testing.T, rt *AgentRuntime) int64 {
	t.Helper()
	st, err := models.GetCommandSchedule(rt.DB.Read(), 1)
	if err != nil || st == nil {
		t.Fatalf("schedule state: %+v %v", st, err)
	}
	return st.LastDue
}

// TestScheduleMarkAndRunRecordedTogether (RT-1): when the run of a scheduled
// occurrence cannot be recorded, its schedule mark is not recorded either,
// so the occurrence is not lost: its entry is queued again after the failed
// run and runs once the run can be recorded.
func TestScheduleMarkAndRunRecordedTogether(t *testing.T) {
	requireBash(t)
	rt := newCommandRuntime(t, &cmdTransport{})
	job := dueScheduledJob(t, rt, "echo hi")
	execOnDB(t, rt.DB, `CREATE TRIGGER fail_run BEFORE INSERT ON command_executions BEGIN SELECT RAISE(ABORT, 'injected failure'); END`)
	stopped := make(chan struct{})
	close(stopped)
	rt.stop, rt.sched.stop = stopped, stopped
	if rt.runCommandJob(job) {
		t.Fatal("a run that could not be recorded was started")
	}
	rt.sched.doneRun(job)
	if due := lastDue(t, rt); due != 0 {
		t.Fatalf("schedule mark %d recorded without the run", due)
	}
	if all, _ := countRuns(rt.DB); all != 0 {
		t.Fatalf("runs recorded: %d", all)
	}

	// The failure is gone: the occurrence is offered again and runs.
	execOnDB(t, rt.DB, `DROP TRIGGER fail_run`)
	rt.stop, rt.sched.stop = nil, nil
	again := nextCommandJob(rt)
	if again == nil || again.scheduledFor != job.scheduledFor {
		t.Fatalf("offered again: %+v, want the occurrence %d", again, job.scheduledFor)
	}
	if !rt.runCommandJob(again) {
		t.Fatal("not started")
	}
	if due := lastDue(t, rt); due != job.scheduledFor {
		t.Fatalf("schedule mark %d, want %d", due, job.scheduledFor)
	}
	if all, finished := countRuns(rt.DB); all != 1 || finished != 1 {
		t.Fatalf("runs recorded %d, finished %d", all, finished)
	}
}

// runInBackground runs job on its own flow and returns a channel that gets
// whether it was started once runCommandJob returned.
func runInBackground(rt *AgentRuntime, job *commandJob) <-chan bool {
	done := make(chan bool, 1)
	go func() { done <- rt.runCommandJob(job) }()
	return done
}

// stillRunning fails when runCommandJob returned within d.
func stillRunning(t *testing.T, done <-chan bool, d time.Duration, what string) {
	t.Helper()
	select {
	case started := <-done:
		t.Fatalf("%s: the run returned (started %v) before its record was stored", what, started)
	case <-time.After(d):
	}
}

// returned waits for runCommandJob to return and gives its result.
func returned(t *testing.T, done <-chan bool) bool {
	t.Helper()
	select {
	case started := <-done:
		return started
	case <-time.After(30 * time.Second):
		t.Fatal("the run did not return")
		return false
	}
}

// executionOf returns the only execution row.
func executionOf(t *testing.T, db *DB) *models.CommandExecution {
	t.Helper()
	var e models.CommandExecution
	if err := db.Read().QueryRow(`SELECT run_uid, finished_at, running_position FROM command_executions`).
		Scan(&e.RunUID, &e.FinishedAt, &e.RunningPosition); err != nil {
		t.Fatal(err)
	}
	return &e
}

// TestRunStartWrittenAgain: while the start of a run cannot be written, the
// worker writes it again (1 s doubling in production, shortened here) and
// starts nothing; nothing of the start is recorded meanwhile. Once it can
// be written, the run starts with its schedule mark, execution row and
// start report.
func TestRunStartWrittenAgain(t *testing.T) {
	requireBash(t)
	var logs lockedLog
	rt := newCommandRuntime(t, &cmdTransport{})
	rt.Logger = logs.logger()
	rt.recordRetry = 20 * time.Millisecond
	job := dueScheduledJob(t, rt, "echo hi")
	before := len(queuedReports(t, rt))
	execOnDB(t, rt.DB, `CREATE TRIGGER fail_run BEFORE INSERT ON command_executions BEGIN SELECT RAISE(ABORT, 'injected failure'); END`)
	done := runInBackground(rt, job)
	stillRunning(t, done, 300*time.Millisecond, "start")
	if due := lastDue(t, rt); due != 0 {
		t.Fatalf("schedule mark %d recorded without the run", due)
	}
	if n := len(queuedReports(t, rt)); n != before {
		t.Fatalf("start report queued without the run: %d", n-before)
	}
	execOnDB(t, rt.DB, `DROP TRIGGER fail_run`)
	if !returned(t, done) {
		t.Fatal("not started")
	}
	if due := lastDue(t, rt); due != job.scheduledFor {
		t.Fatalf("schedule mark %d, want %d", due, job.scheduledFor)
	}
	if e := executionOf(t, rt.DB); e.FinishedAt == 0 {
		t.Fatalf("run not finished: %+v", e)
	}
	runs := runReports(queuedReports(t, rt)[before:])
	if len(runs) != 2 || runs[0].Finished || !runs[1].Finished || runs[1].State != ResultSuccess {
		t.Fatalf("reports: %+v", runs)
	}
	if !strings.Contains(logs.String(), "the start of run") || !strings.Contains(logs.String(), "storing it failed, tried again in 20ms") {
		t.Fatalf("log: %q", logs.String())
	}
}

// TestRunReportsWrittenAgain: a command's report and the end of the run are
// written again until they are stored: the next command does not start and
// the worker does not leave the run before; each is stored once, in order.
func TestRunReportsWrittenAgain(t *testing.T) {
	requireBash(t)
	rt := newCommandRuntime(t, &cmdTransport{})
	rt.recordRetry = 20 * time.Millisecond
	marker := filepath.Join(t.TempDir(), "second")
	rt.storeCommandDefinitions(defsOf(testSet(1, PurposeRunOnly, "echo first", "touch "+marker)))
	receiveRequests(rt, manualRequests(7, 1))
	job := nextCommandJob(rt)
	before := len(queuedReports(t, rt))
	// The report of the first command cannot be written.
	execOnDB(t, rt.DB, `CREATE TRIGGER fail_first BEFORE INSERT ON outbox WHEN NEW.payload LIKE '%"finished":false%"position":0%'
		BEGIN SELECT RAISE(ABORT, 'injected failure'); END`)
	done := runInBackground(rt, job)
	stillRunning(t, done, 400*time.Millisecond, "the first command's report")
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the second command started before the first one's report was stored")
	}
	if e := executionOf(t, rt.DB); e.RunningPosition != 0 {
		t.Fatalf("the first command must stay recorded as running: %+v", e)
	}
	// The end of the run cannot be written either.
	execOnDB(t, rt.DB, `CREATE TRIGGER fail_end BEFORE INSERT ON outbox WHEN NEW.payload LIKE '%"finished":true%'
		BEGIN SELECT RAISE(ABORT, 'injected failure'); END`)
	execOnDB(t, rt.DB, `DROP TRIGGER fail_first`)
	waitFile(t, marker)
	stillRunning(t, done, 300*time.Millisecond, "the end of the run")
	if e := executionOf(t, rt.DB); e.FinishedAt != 0 || e.RunningPosition != 1 {
		t.Fatalf("the run must stay unfinished with its second command running: %+v", e)
	}
	execOnDB(t, rt.DB, `DROP TRIGGER fail_end`)
	if !returned(t, done) {
		t.Fatal("not started")
	}
	if e := executionOf(t, rt.DB); e.FinishedAt == 0 || e.RunningPosition != -1 {
		t.Fatalf("run after its end was stored: %+v", e)
	}
	runs := runReports(queuedReports(t, rt)[before:])
	if len(runs) != 3 || len(runs[1].Results) != 1 || runs[1].Results[0].Position != 0 || !runs[2].Finished ||
		len(runs[2].Results) != 1 || runs[2].Results[0].Position != 1 || runs[2].State != ResultSuccess {
		t.Fatalf("reports: %+v", runs)
	}
}

// TestRunEndNotStoredBeforeStop: when the end of a run cannot be written
// before the stop comes, the worker leaves the run unfinished, and the next
// start reports it as interrupted.
func TestRunEndNotStoredBeforeStop(t *testing.T) {
	requireBash(t)
	rt := newCommandRuntime(t, &cmdTransport{})
	rt.recordRetry = 20 * time.Millisecond
	stop := make(chan struct{})
	rt.stop, rt.sched.stop = stop, stop
	rt.storeCommandDefinitions(defsOf(testSet(1, PurposeRunOnly, "echo only")))
	receiveRequests(rt, manualRequests(7, 1))
	job := nextCommandJob(rt)
	execOnDB(t, rt.DB, `CREATE TRIGGER fail_end BEFORE INSERT ON outbox WHEN NEW.payload LIKE '%"finished":true%'
		BEGIN SELECT RAISE(ABORT, 'injected failure'); END`)
	done := runInBackground(rt, job)
	stillRunning(t, done, 300*time.Millisecond, "the end of the run")
	close(stop)
	if !returned(t, done) {
		t.Fatal("not started")
	}
	if e := executionOf(t, rt.DB); e.FinishedAt != 0 {
		t.Fatalf("a run whose end was not stored was recorded finished: %+v", e)
	}
	execOnDB(t, rt.DB, `DROP TRIGGER fail_end`)
	before := len(queuedReports(t, rt))
	rt2 := NewAgentRuntime(rt.DB, &cmdTransport{}, nil)
	recoverAtStart(t, rt2)
	runs := runReports(queuedReports(t, rt2)[before:])
	if len(runs) != 1 || !runs[0].Finished || runs[0].Reason != ReasonInterrupted || runs[0].RequestID != 7 {
		t.Fatalf("reported at the next start: %+v", runs)
	}
}

// recoverAtStart reads and reports the runs left unfinished, as the start of
// Run does; the read must succeed.
func recoverAtStart(t *testing.T, rt *AgentRuntime) {
	t.Helper()
	if _, err := rt.recoverInterruptedRuns(); err != nil {
		t.Fatalf("reading the unfinished runs: %v", err)
	}
}

// waitFile waits until the file exists.
func waitFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s was not created", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// lockedLog collects the log lines of a runtime.
type lockedLog struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func (l *lockedLog) logger() *log.Logger { return log.New(l, "", 0) }

// TestInterruptedRecordWrittenAgain: a run found unfinished at the start
// whose interrupted record cannot be stored is written again (with the wait
// of the run records) until it is stored, while the rounds and the
// monitoring steps go on; its set does not start before (a request for it
// waits), 'was interrupted' is logged only once it was stored, and then the
// agent no longer lists its request as running.
func TestInterruptedRecordWrittenAgain(t *testing.T) {
	requireBash(t)
	tr := &parTransport{}
	rt := newParallelRuntime(t, tr, 2)
	rt.recordRetry = 50 * time.Millisecond
	var logs lockedLog
	rt.Logger = logs.logger()
	set := testSet(1, PurposeRunOnly, "true")
	rt.storeCommandDefinitions(defsOf(set))
	tr.resp.CommandSets = defsOf(set)
	tr.resp.CommandRequests = manualRequests(6, 1)
	insertExecution(t, rt.DB, "killed-run", 5, 0)
	execOnDB(t, rt.DB, `CREATE TRIGGER fail_finish BEFORE UPDATE OF finished_at ON command_executions WHEN OLD.run_uid = 'killed-run'
		BEGIN SELECT RAISE(ABORT, 'injected failure'); END`)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		rt.Run(stop)
	}()
	defer func() {
		close(stop)
		<-done
	}()
	// The rounds and the monitoring steps go on while the record fails.
	waitFor(t, "three rounds", func() bool { return len(tr.seen()) >= 3 })
	waitFor(t, "a second monitoring step", func() bool {
		return strings.Count(logs.String(), "storing it failed") >= 3 && monitorSteps(t, rt, tr) >= 2
	})
	if all, _ := countRuns(rt.DB); all != 1 {
		t.Fatalf("the set started before its interrupted run was recorded: %d runs", all)
	}
	if strings.Contains(logs.String(), "run killed-run was interrupted (") {
		t.Fatalf("'was interrupted' logged although nothing was stored: %q", logs.String())
	}
	if ids, _ := rt.runningRequests(); !slices.Contains(ids, int64(5)) {
		t.Fatalf("running requests while the record fails: %v", ids)
	}
	execOnDB(t, rt.DB, `DROP TRIGGER fail_finish`)
	waitFor(t, "the request of the set", func() bool { _, finished := countRuns(rt.DB); return finished == 2 })
	if !strings.Contains(logs.String(), "command set run killed-run was interrupted (the agent ended while it ran)") {
		t.Fatalf("log: %q", logs.String())
	}
	if ids, _ := rt.runningRequests(); slices.Contains(ids, int64(5)) {
		t.Fatalf("the interrupted run is still listed as running: %v", ids)
	}
	reported := 0
	for _, r := range runReports(allReports(t, rt, tr)) {
		if r.RunUID == "killed-run" && r.Finished && r.Reason == ReasonInterrupted {
			reported++
		}
	}
	if reported != 1 {
		t.Fatalf("the interrupted run was reported %d time(s)", reported)
	}
}

// monitorSteps returns the number of periodic reports the monitoring steps
// queued (sent or still queued).
func monitorSteps(t *testing.T, rt *AgentRuntime, tr *parTransport) int {
	t.Helper()
	n := 0
	for _, r := range allReports(t, rt, tr) {
		if len(r.CommandRuns) == 0 && len(r.CommandRequestAcks) == 0 && r.CommandApply == nil {
			n++
		}
	}
	return n
}

// failingRead is a read of the runs left unfinished (readUnfinished) that
// fails while failing is set; attempts counts the reads.
type failingRead struct {
	mu       sync.Mutex
	failing  bool
	attempts int
}

func (f *failingRead) read(db models.Reader) ([]*models.CommandExecution, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts++
	if f.failing {
		return nil, errors.New("injected failure")
	}
	return models.ListUnfinishedCommandExecutions(db)
}

func (f *failingRead) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts
}

func (f *failingRead) succeed() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failing = false
}

// newRunsOf returns the number of finished runs of a set other than the
// runs found unfinished at the start (their UIDs end in "-run").
func newRunsOf(rt *AgentRuntime, set int64) int {
	var n int
	_ = rt.DB.Read().QueryRow(`SELECT count(*) FROM command_executions WHERE set_id = ? AND finished_at > 0 AND run_uid NOT LIKE '%-run'`, set).Scan(&n)
	return n
}

// TestUnfinishedRunsReadAgain: when the runs left unfinished cannot be read
// at the start, the read is repeated (with the wait of the run records)
// until it succeeds, and meanwhile no command set starts, neither a
// scheduled one that is due nor a manual request, while the rounds and the
// monitoring steps go on; free_worker counts none of the waiting entries.
// The failure is logged once with the sets waiting, each further attempt as
// a record's is, and the success after it once. Then the runs are handled as
// after a read at the start: the set whose recorded process group still
// exists is held, the others start, and each run is reported interrupted
// once.
func TestUnfinishedRunsReadAgain(t *testing.T) {
	requireBash(t)
	boot, err := currentBoot()
	if err != nil {
		t.Fatal(err)
	}
	survivor := startSurvivor(t)
	tr := &parTransport{}
	rt := newParallelRuntime(t, tr, 2)
	rt.recordRetry = 50 * time.Millisecond
	var logs lockedLog
	rt.Logger = logs.logger()
	scheduled := testSet(2, PurposeRunOnly, "true")
	scheduled.RunMode = RunModeSchedule
	scheduled.Schedule = &CommandSchedule{Kind: ScheduleDaily, Time: "00:00"}
	sets := defsOf(testSet(1, PurposeRunOnly, "true"), scheduled, testSet(3, PurposeRunOnly, "true"))
	rt.storeCommandDefinitions(sets)
	// Today's occurrence of the scheduled set passed and was not run.
	execOnDB(t, rt.DB, `UPDATE command_schedules SET armed_at = armed_at - 2*86400`)
	tr.resp.CommandSets = sets
	tr.resp.CommandRequests = manualRequests(6, 1, 3)
	// The previous agent was killed while a run of set 1 and one of set 3
	// ran; the command of set 3 still runs as its process group.
	insertExecution(t, rt.DB, "killed-run", 5, 0)
	insertExecution(t, rt.DB, "surviving-run", 0, 0)
	execOnDB(t, rt.DB, fmt.Sprintf(`UPDATE command_executions SET set_id = 3, running_position = 0, running_pgid = %d, running_boot = '%s'
		WHERE run_uid = 'surviving-run'`, survivor.Pid, boot.String()))
	reads := &failingRead{failing: true}
	rt.readUnfinished = reads.read
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		rt.Run(stop)
	}()
	defer func() {
		close(stop)
		<-done
	}()

	waitFor(t, "three rounds and a second monitoring step", func() bool { return len(tr.seen()) >= 3 && monitorSteps(t, rt, tr) >= 2 })
	if all, _ := countRuns(rt.DB); all != 2 {
		t.Fatalf("a command set started while the unfinished runs were not read: %d runs", all)
	}
	waitFor(t, "four failed reads", func() bool { return reads.count() >= 4 })
	for i, s := range tr.seen() {
		if !s.freeWorker {
			t.Fatalf("sync %d: free_worker false while every command set waits for the read", i+1)
		}
	}
	logged := logs.String()
	if n := strings.Count(logged, "reading unfinished command set runs failed (no command set starts until they are read), tried again in 50ms: injected failure"); n != 1 {
		t.Fatalf("the failure with the waiting sets logged %d times: %q", n, logged)
	}
	if n := strings.Count(logged, "reading unfinished command set runs failed, tried again in "); n < 2 || n > reads.count()-1 {
		t.Fatalf("%d further failures logged for %d reads: %q", n, reads.count(), logged)
	}
	if strings.Contains(logged, "was interrupted") || strings.Contains(logged, "process group") {
		t.Fatalf("runs handled before they were read: %q", logged)
	}

	reads.succeed()
	waitFor(t, "the manual and the scheduled set", func() bool { return newRunsOf(rt, 1) == 1 && newRunsOf(rt, 2) == 1 })
	time.Sleep(1500 * time.Millisecond)
	if n := newRunsOf(rt, 3); n != 0 {
		t.Fatalf("the set of the surviving group started %d time(s)", n)
	}
	logged = logs.String()
	for line, want := range map[string]int{
		"reading unfinished command set runs succeeded after failing; command sets may start":                  1,
		"command set run killed-run was interrupted (the agent ended while it ran)":                            1,
		"command set run surviving-run was interrupted (the agent ended while it ran)":                         1,
		fmt.Sprintf("command set run surviving-run: its command still runs as process group %d", survivor.Pid): 1,
	} {
		if n := strings.Count(logged, line); n != want {
			t.Fatalf("%q logged %d times, want %d: %q", line, n, want, logged)
		}
	}
	reported := map[string]int{}
	for _, r := range runReports(allReports(t, rt, tr)) {
		if r.Finished && r.Reason == ReasonInterrupted {
			reported[r.RunUID]++
		}
	}
	if reported["killed-run"] != 1 || reported["surviving-run"] != 1 || len(reported) != 2 {
		t.Fatalf("reported interrupted: %v", reported)
	}
}

// TestStopWhileUnfinishedRunsUnread: a stop while the runs left unfinished
// still cannot be read ends the agent as usual (the last report says that it
// stops, the worker count in use is cleared); no command set started, the
// runs stay unfinished, and the next start reads them and reports them
// interrupted.
func TestStopWhileUnfinishedRunsUnread(t *testing.T) {
	requireBash(t)
	tr := &parTransport{}
	rt := newParallelRuntime(t, tr, 1)
	rt.recordRetry = 50 * time.Millisecond
	set := testSet(1, PurposeRunOnly, "true")
	rt.storeCommandDefinitions(defsOf(set))
	tr.resp.CommandSets = defsOf(set)
	tr.resp.CommandRequests = manualRequests(6, 1)
	insertExecution(t, rt.DB, "killed-run", 5, 0)
	reads := &failingRead{failing: true}
	rt.readUnfinished = reads.read
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		rt.Run(stop)
	}()
	waitFor(t, "two rounds", func() bool { return len(tr.seen()) >= 2 })
	if all, _ := countRuns(rt.DB); all != 1 {
		close(stop)
		<-done
		t.Fatalf("a command set started while the unfinished runs were not read: %d runs", all)
	}
	waitFor(t, "three failed reads", func() bool { return reads.count() >= 3 })
	close(stop)
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the agent did not end after the stop")
	}
	if e := executionOf(t, rt.DB); e.RunUID != "killed-run" || e.FinishedAt != 0 {
		t.Fatalf("the unfinished run after the stop: %+v", e)
	}
	tr.mu.Lock()
	stopping := len(tr.batches) > 0 && tr.batches[len(tr.batches)-1].Stopping
	tr.mu.Unlock()
	if !stopping {
		t.Fatal("the last report does not say that the agent stops")
	}
	if _, found, err := models.FindLocalState(rt.DB.Read(), stateRunningWorkers); found || err != nil {
		t.Fatalf("the worker count in use is still recorded (%v)", err)
	}
	before := allReports(t, rt, tr)
	for _, r := range runReports(before) {
		if r.RunUID == "killed-run" {
			t.Fatalf("reported without being read: %+v", r)
		}
	}
	queued := len(queuedReports(t, rt))
	rt2 := NewAgentRuntime(rt.DB, &cmdTransport{}, nil)
	recoverAtStart(t, rt2)
	runs := runReports(queuedReports(t, rt2)[queued:])
	if len(runs) != 1 || runs[0].RunUID != "killed-run" || !runs[0].Finished || runs[0].Reason != ReasonInterrupted {
		t.Fatalf("reported at the next start: %+v", runs)
	}
}
