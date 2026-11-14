package modules

import (
	"bytes"
	"log"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"mimamori/app/models"
)

// TestSchedulerOneWorkerAsToday: with one worker no flow is started; the
// monitoring step goes on at the interval inside the wait loop of a running
// command, as before the scheduler.
func TestSchedulerOneWorkerAsToday(t *testing.T) {
	requireBash(t)
	tr := &parTransport{}
	rt := newParallelRuntime(t, tr, 1)
	tr.resp.CommandSets = defsOf(testSet(1, PurposeRunOnly, "sleep 2.5"))
	tr.resp.CommandRequests = manualRequests(1, 1)
	stop := stopWhenRunsFinished(t, rt.DB, 1)
	// Every sync and every report runs on the flow that called Run.
	here := goroutineNumber()
	if err := rt.Run(stop); err != nil {
		t.Fatal(err)
	}
	tr.mu.Lock()
	reportFlows := slices.Clone(tr.reportFlows)
	tr.mu.Unlock()
	if len(reportFlows) == 0 {
		t.Fatal("no report was sent")
	}
	for i, g := range reportFlows {
		if g != here {
			t.Fatalf("report %d ran on flow %d, Run was called on %d: a flow was started", i+1, g, here)
		}
	}
	during := 0
	for i, s := range tr.seen() {
		if s.goroutine != here {
			t.Fatalf("step %d ran on flow %d, Run was called on %d: a flow was started", i+1, s.goroutine, here)
		}
		if s.running > 0 {
			if !s.inWaitLoop {
				t.Fatalf("step %d ran during the run outside the command's wait loop", i+1)
			}
			during++
		}
	}
	if during < 2 {
		t.Fatalf("the monitoring must go on while the command runs: %d steps during the run", during)
	}
}

// TestSchedulerOrder: one worker runs manual requests oldest first (not in
// the order they arrived), then the due schedules by due time, lower ID first
// on equal times.
func TestSchedulerOrder(t *testing.T) {
	requireBash(t)
	tr := &parTransport{}
	rt := newParallelRuntime(t, tr, 1)
	hourly := func(id int64) CommandSetDef {
		d := testSet(id, PurposeRunOnly, "true")
		d.RunMode = RunModeSchedule
		d.Schedule = &CommandSchedule{Kind: ScheduleEveryHours, Every: 1}
		return d
	}
	defs := defsOf(hourly(2), hourly(1), testSet(3, PurposeRunOnly, "true"), testSet(4, PurposeRunOnly, "true"))
	rt.storeCommandDefinitions(defs)
	if err := rt.DB.Write(func(tx models.Writer) error {
		_, err := tx.Exec(`UPDATE command_schedules SET armed_at = armed_at - 7200`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	tr.resp.CommandSets = defs
	tr.resp.CommandRequests = []CommandRunRequest{
		{ID: 10, SetID: 3, RequestedAt: now - 5, ExpiresAt: now + 3600},
		{ID: 11, SetID: 4, RequestedAt: now - 10, ExpiresAt: now + 3600},
	}
	// The schedules are due before the first step: the step comes first and
	// delivers the requests, which come before the schedules.
	if err := rt.Run(stopWhenRunsFinished(t, rt.DB, 4)); err != nil {
		t.Fatal(err)
	}
	var order []int64
	for _, r := range runReports(allReports(t, rt, tr)) {
		if len(r.Results) == 0 && !r.Finished {
			order = append(order, r.SetID)
		}
	}
	if len(order) != 4 || order[0] != 4 || order[1] != 3 || order[2] != 1 || order[3] != 2 {
		t.Fatalf("order of the runs: %v", order)
	}
}

// TestSchedulerNoBusyRetry: a scheduled set whose occurrence cannot be
// recorded is not started again before the next monitoring step.
func TestSchedulerNoBusyRetry(t *testing.T) {
	tr := &parTransport{}
	rt := newParallelRuntime(t, tr, 2)
	var logBuf bytes.Buffer
	var logMu sync.Mutex
	rt.Logger = log.New(lockedWriter{&logMu, &logBuf}, "", 0)
	d := testSet(1, PurposeRunOnly, "true")
	d.RunMode = RunModeSchedule
	d.Schedule = &CommandSchedule{Kind: ScheduleEveryHours, Every: 1}
	defs := defsOf(d)
	rt.storeCommandDefinitions(defs)
	tr.resp.CommandSets = defs
	if err := rt.DB.Write(func(tx models.Writer) error {
		if _, err := tx.Exec(`UPDATE command_schedules SET armed_at = armed_at - 7200`); err != nil {
			return err
		}
		_, err := tx.Exec(`CREATE TEMP TRIGGER fail_last_due BEFORE UPDATE OF last_due ON command_schedules
			BEGIN SELECT RAISE(ABORT, 'refused by the test'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	stop, _ := stopWhen(t, func() bool { return len(tr.seen()) >= 3 })
	if err := rt.Run(stop); err != nil {
		t.Fatal(err)
	}
	logMu.Lock()
	failures := strings.Count(logBuf.String(), "recording the schedule failed")
	logMu.Unlock()
	steps := len(tr.seen())
	if failures == 0 || failures > steps+1 {
		t.Fatalf("%d failed starts for %d monitoring steps", failures, steps)
	}
	if all, _ := countRuns(rt.DB); all != 0 {
		t.Fatalf("runs recorded: %d", all)
	}
}

// lockedWriter serializes writes to a buffer shared with the test.
type lockedWriter struct {
	mu  *sync.Mutex
	buf *bytes.Buffer
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

// TestSchedulerInlineMonitor: a worker waiting for a command gets the due
// monitoring step only when no other worker is idle, never while a step runs
// and never after the stop; the step is due again only after the interval
// from its start and monitorGap from its end.
func TestSchedulerInlineMonitor(t *testing.T) {
	now := time.Unix(1000, 0)
	s := newAgentScheduler(nil, func() time.Time { return now }, func() *time.Location { return time.UTC }, 10*time.Second)
	s.idle = 1
	if s.inlineMonitor() {
		t.Fatal("an idle worker takes the step")
	}
	s.idle = 0
	if !s.inlineMonitor() {
		t.Fatal("the first step is due")
	}
	if s.inlineMonitor() {
		t.Fatal("a step runs already")
	}
	now = now.Add(15 * time.Second)
	s.doneMonitor(10 * time.Second)
	if s.inlineMonitor() {
		t.Fatal("due only monitorGap after the end of a long step")
	}
	now = now.Add(monitorGap)
	if !s.inlineMonitor() {
		t.Fatal("due after the gap")
	}
	s.doneMonitor(10 * time.Second)
	now = now.Add(time.Hour)
	stop := make(chan struct{})
	close(stop)
	s.stop = stop
	if s.inlineMonitor() {
		t.Fatal("no step after the stop")
	}
	if u, acks := s.next(); u != nil || acks != nil {
		t.Fatalf("no unit after the stop: %+v %+v", u, acks)
	}
}
