package modules

import (
	"log"
	"os"
	"path/filepath"
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
	rt2.recoverInterruptedRuns()
	runs := runReports(queuedReports(t, rt2)[before:])
	if len(runs) != 1 || !runs[0].Finished || runs[0].Reason != ReasonInterrupted || runs[0].RequestID != 7 {
		t.Fatalf("reported at the next start: %+v", runs)
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
