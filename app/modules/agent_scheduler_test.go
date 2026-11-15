package modules

import (
	"bytes"
	"log"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"mimamori/app/models"
)

// newTestQueue returns the queue of a runtime on a fresh local database with
// a clock the test sets.
func newTestQueue(t *testing.T, now *time.Time) (*AgentRuntime, *agentScheduler) {
	t.Helper()
	rt := newCommandRuntime(t, &cmdTransport{})
	rt.clock = func() time.Time { return *now }
	rt.sched.now = rt.now
	return rt, rt.sched
}

// pick takes the next unit without waiting.
func pick(s *agentScheduler) *agentUnit {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, _ := s.pickLocked(s.now())
	return u
}

// TestQueuePickRule: a worker takes, among the entries that may start now and
// whose set does not run, the one that became startable first (the later of
// the time it was queued and its startAt; on equal times the one queued
// first). Requests of one sync are queued oldest first. With nothing
// startable a worker waits until the earliest startAt.
func TestQueuePickRule(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	rt, s := newTestQueue(t, &now)
	rt.storeCommandDefinitions(defsOf(testSet(1, PurposeRunOnly, "true"), testSet(2, PurposeRunOnly, "true"),
		testSet(3, PurposeRunOnly, "true"), testSet(4, PurposeRunOnly, "true")))
	s.mu.Lock()
	// Set 1 may start at now+10s (queued now); set 2 at now-30s (queued
	// now-60s); the monitoring entry at now+5s.
	s.addLocked(&queueEntry{setID: 1, startAt: now.Add(10 * time.Second), scheduledFor: now.Add(10 * time.Second).Unix()}, now)
	s.addLocked(&queueEntry{setID: 2, startAt: now.Add(-30 * time.Second), scheduledFor: now.Add(-30 * time.Second).Unix()}, now.Add(-60*time.Second))
	s.addLocked(&queueEntry{monitor: true, startAt: now.Add(5 * time.Second)}, now)
	s.mu.Unlock()
	s.valid[1].RunMode, s.valid[2].RunMode = RunModeSchedule, RunModeSchedule
	// Two requests delivered in one sync: the older one first.
	exp := now.Unix() + 3600
	receiveRequests(rt, []CommandRunRequest{{ID: 11, SetID: 4, RequestedAt: now.Unix() - 5, ExpiresAt: exp},
		{ID: 10, SetID: 3, RequestedAt: now.Unix() - 10, ExpiresAt: exp}})

	order := []string{}
	take := func() {
		u := pick(s)
		switch {
		case u == nil:
			order = append(order, "-")
		case u.monitor:
			order = append(order, "monitor")
			s.mu.Lock()
			s.monitoring = false
			s.mu.Unlock()
		default:
			order = append(order, string(rune('0'+u.job.def.ID)))
			s.doneRun(u.job)
		}
	}
	take() // set 2: startable since now-30s
	take() // set 3: queued now, older request
	take() // set 4
	take() // nothing startable
	s.mu.Lock()
	wait := s.waitLocked(now)
	s.mu.Unlock()
	if wait != 5*time.Second {
		t.Fatalf("wait %s, want until the earliest startAt (5 s)", wait)
	}
	now = now.Add(20 * time.Second)
	take() // monitoring (startable now+5s) before set 1 (now+10s)
	take()
	if got := strings.Join(order, " "); got != "2 3 4 - monitor 1" {
		t.Fatalf("order %q", got)
	}

	// An entry whose set runs is not taken; it may start when the run ended.
	receiveRequests(rt, []CommandRunRequest{{ID: 12, SetID: 3, RequestedAt: now.Unix(), ExpiresAt: exp}})
	s.mu.Lock()
	s.running[3] = true
	s.mu.Unlock()
	if u := pick(s); u != nil {
		t.Fatalf("an entry of a running set was taken: %+v", u)
	}
	s.doneRun(&commandJob{def: s.valid[3]})
	if u := pick(s); u == nil || u.job.requestID != 12 {
		t.Fatalf("after the run: %+v", u)
	}
}

// TestQueueMonitoringEntry: exactly one monitoring entry: at the start
// without a time, none while a worker processes it, queued again one
// interval after its end; a sync that brings a changed rule assignment
// removes its time, also while the step runs. After the stop nothing is
// taken.
func TestQueueMonitoringEntry(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	_, s := newTestQueue(t, &now)
	monitors := func() []queueEntry {
		s.mu.Lock()
		defer s.mu.Unlock()
		var out []queueEntry
		for _, e := range s.entries {
			if e.monitor {
				out = append(out, *e)
			}
		}
		return out
	}
	s.start()
	if m := monitors(); len(m) != 1 || !m[0].startAt.IsZero() {
		t.Fatalf("at the start: %+v", m)
	}
	if u := pick(s); u == nil || !u.monitor {
		t.Fatalf("the step may start at once: %+v", u)
	}
	if m := monitors(); len(m) != 0 {
		t.Fatalf("while it runs: %+v", m)
	}
	now = now.Add(3 * time.Second)
	s.doneMonitor(10 * time.Second)
	if m := monitors(); len(m) != 1 || !m[0].startAt.Equal(now.Add(10*time.Second)) {
		t.Fatalf("after the step: %+v", m)
	}
	if u := pick(s); u != nil {
		t.Fatalf("taken before its time: %+v", u)
	}
	s.monitorNow()
	if m := monitors(); len(m) != 1 || !m[0].startAt.IsZero() {
		t.Fatalf("after a changed assignment: %+v", m)
	}
	if u := pick(s); u == nil || !u.monitor {
		t.Fatalf("the step may start at once: %+v", u)
	}
	s.monitorNow()
	s.doneMonitor(10 * time.Second)
	if m := monitors(); len(m) != 1 || !m[0].startAt.IsZero() {
		t.Fatalf("a changed assignment while the step ran: %+v", m)
	}
	stop := make(chan struct{})
	close(stop)
	s.stop = stop
	if u, acks := s.next(); u != nil || acks != nil {
		t.Fatalf("no unit after the stop: %+v %+v", u, acks)
	}
}

// TestSyncSaysFreeWorker: the sync says free_worker when the agent has more
// workers than command set runs in progress plus command set entries that
// may start now and wait for a worker. The monitoring entry does not count
// (queued, due or being processed), nor does a manual entry that waits
// because its own set runs or is held by a surviving process group.
func TestSyncSaysFreeWorker(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	rt, s := newTestQueue(t, &now)
	rt.storeCommandDefinitions(defsOf(testSet(1, PurposeRunOnly, "true"), testSet(2, PurposeRunOnly, "true"),
		testSet(3, PurposeRunOnly, "true")))
	exp := now.Unix() + 3600
	request := func(id, set int64) {
		receiveRequests(rt, []CommandRunRequest{{ID: id, SetID: set, RequestedAt: now.Unix(), ExpiresAt: exp}})
	}
	setRunning := func(set int64, on bool) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if on {
			s.running[set] = true
		} else {
			delete(s.running, set)
		}
	}

	// One worker: the monitoring step is due (the start) or being processed,
	// and no run is in progress: free. A request handed over then runs when
	// the step ended, before the monitoring entry queued after it.
	s.start()
	if !s.freeWorker() {
		t.Fatal("one worker, the monitoring step due, no run: not free")
	}
	if u := pick(s); u == nil || !u.monitor {
		t.Fatalf("the monitoring step: %+v", u)
	}
	if !s.freeWorker() {
		t.Fatal("one worker, the monitoring step being processed, no run: not free")
	}
	request(1, 1)
	now = now.Add(2 * time.Second)
	s.doneMonitor(0)
	if u := pick(s); u == nil || u.job == nil || u.job.requestID != 1 {
		t.Fatalf("after the step: %+v, want the request handed over during it", u)
	}
	// The monitoring entry queued after the step (due at once) comes next;
	// its next one is an hour away.
	if u := pick(s); u == nil || !u.monitor {
		t.Fatalf("the monitoring step after the run: %+v", u)
	}
	s.doneMonitor(time.Hour)
	// One worker, a command runs: not free.
	if s.freeWorker() {
		t.Fatal("one worker, a run in progress: free")
	}
	s.doneRun(&commandJob{def: s.valid[1], requestID: 1})

	// One worker, a run startable now and waiting for the worker: not free.
	request(2, 2)
	if s.freeWorker() {
		t.Fatal("one worker, a run waits for it: free")
	}
	if u := pick(s); u == nil || u.job == nil || u.job.requestID != 2 {
		t.Fatalf("the waiting run: %+v", u)
	}
	s.doneRun(&commandJob{def: s.valid[2], requestID: 2})

	// Four workers: set 1 runs and a second request of set 1 waits for that
	// run: free, and a request of set 2 is taken and runs.
	s.setWorkers(4)
	setRunning(1, true)
	request(3, 1)
	if !s.freeWorker() {
		t.Fatal("four workers, one run, a request waiting for its own set: not free")
	}
	request(4, 2)
	if u := pick(s); u == nil || u.job == nil || u.job.requestID != 4 {
		t.Fatalf("the request of set 2: %+v", u)
	}
	// A request of a set held by a surviving group does not count either.
	survivor := startSurvivor(t)
	s.hold(3, survivor.Pid)
	request(5, 3)
	s.setWorkers(3)
	if !s.freeWorker() {
		t.Fatal("three workers, two runs, the requests of a running and of a held set waiting: not free")
	}

	// Two workers, two runs: not free.
	s.setWorkers(2)
	if s.freeWorker() {
		t.Fatal("two workers, two runs: free")
	}
	setRunning(1, false)
}

// TestFirstSyncSaysFreeWorker: with one worker, the first sync after the
// start (the monitoring step is due or being processed then) says
// free_worker, so the central hands a request over at once.
func TestFirstSyncSaysFreeWorker(t *testing.T) {
	tr := &parTransport{}
	rt := newParallelRuntime(t, tr, 1)
	stop, _ := stopWhen(t, func() bool { return len(tr.seen()) >= 2 })
	rt.Run(stop)
	if seen := tr.seen(); !seen[0].freeWorker || !seen[1].freeWorker {
		t.Fatalf("free_worker of the first syncs: %+v", seen)
	}
}

// TestRequestAnswersFromStoredFacts (LOST-ACK): the answer to a delivered
// request is made again from what is stored at every delivery, also when an
// earlier answer was lost; a request that waits in the queue gets no new
// answer, and one whose expiry passed while it waited is answered expired
// and not started.
func TestRequestAnswersFromStoredFacts(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	rt, _ := newTestQueue(t, &now)
	rt.storeCommandDefinitions(defsOf(testSet(1, PurposeRunOnly, "true"), testSet(2, PurposeRunOnly, "true")))
	expired := CommandRunRequest{ID: 1, SetID: 1, RequestedAt: now.Unix() - 100, ExpiresAt: now.Unix() - 1}
	unknown := CommandRunRequest{ID: 2, SetID: 9, RequestedAt: now.Unix(), ExpiresAt: now.Unix() + 60}
	waiting := CommandRunRequest{ID: 3, SetID: 2, RequestedAt: now.Unix(), ExpiresAt: now.Unix() + 60}
	// The answers of the first delivery are lost (the outbox refuses them).
	execOnDB(t, rt.DB, `CREATE TRIGGER lose_answers BEFORE INSERT ON outbox WHEN NEW.payload LIKE '%command_request_acks%'
		BEGIN SELECT RAISE(ABORT, 'injected failure'); END`)
	receiveRequests(rt, []CommandRunRequest{expired, unknown, waiting})
	execOnDB(t, rt.DB, `DROP TRIGGER lose_answers`)
	if n := len(acksOf(queuedReports(t, rt))); n != 0 {
		t.Fatalf("answers stored although lost: %d", n)
	}
	// Delivered again: answered again; the waiting request gets nothing new.
	receiveRequests(rt, []CommandRunRequest{expired, unknown, waiting})
	got := map[int64]string{}
	for _, a := range acksOf(queuedReports(t, rt)) {
		got[a.RequestID] = a.Status
	}
	if len(got) != 2 || got[1] != RequestAckExpired || got[2] != RequestAckUnknownSet {
		t.Fatalf("answers of the second delivery: %v", got)
	}
	if ids := manualEntries(rt); len(ids) != 1 || ids[0] != 3 {
		t.Fatalf("queued: %v", ids)
	}
	// The waiting request's expiry passes while it waits: delivered again,
	// it is taken out and answered expired; it never starts.
	now = now.Add(2 * time.Minute)
	receiveRequests(rt, []CommandRunRequest{waiting})
	if ids := manualEntries(rt); len(ids) != 0 {
		t.Fatalf("still queued: %v", ids)
	}
	last := acksOf(queuedReports(t, rt))
	if a := last[len(last)-1]; a.RequestID != 3 || a.Status != RequestAckExpired {
		t.Fatalf("answer: %+v", a)
	}
	// The same at the time a worker would take it.
	receiveRequests(rt, []CommandRunRequest{{ID: 4, SetID: 2, RequestedAt: now.Unix(), ExpiresAt: now.Unix() + 60}})
	now = now.Add(2 * time.Minute)
	if job := nextCommandJob(rt); job != nil {
		t.Fatalf("an expired request was started: %+v", job)
	}
	last = acksOf(queuedReports(t, rt))
	if a := last[len(last)-1]; a.RequestID != 4 || a.Status != RequestAckExpired {
		t.Fatalf("answer at the pick: %+v", a)
	}
}

// TestDefinitionsNotStoredRequestsNotTaken (DEFS-FAIL): when the definitions
// a sync brings cannot be stored, nothing of them is stored and the run
// requests of that sync are not taken (no unknown_set answer); the next
// delivery stores them and takes the request.
func TestDefinitionsNotStoredRequestsNotTaken(t *testing.T) {
	tr := &cmdTransport{}
	rt := newCommandRuntime(t, tr)
	tr.resp.CommandSets = defsOf(testSet(1, PurposeRunOnly, "true"))
	tr.resp.CommandRequests = manualRequests(7, 1)
	execOnDB(t, rt.DB, `CREATE TRIGGER fail_defs BEFORE INSERT ON state WHEN NEW.key = 'command_defs'
		BEGIN SELECT RAISE(ABORT, 'injected failure'); END`)
	rt.sync()
	if sig := rt.sched.signature(); sig != "" {
		t.Fatalf("definitions taken into use although not stored: %q", sig)
	}
	if acks := acksOf(queuedReports(t, rt)); len(acks) != 0 {
		t.Fatalf("the request of that sync was answered: %+v", acks)
	}
	if ids := manualEntries(rt); len(ids) != 0 {
		t.Fatalf("the request of that sync was taken: %v", ids)
	}
	if q := queuedReports(t, rt); len(q) != 0 {
		t.Fatalf("an outcome of definitions that were not stored was queued: %+v", q)
	}
	execOnDB(t, rt.DB, `DROP TRIGGER fail_defs`)
	rt.sync()
	if rt.sched.signature() != tr.resp.CommandSets.Signature {
		t.Fatal("the definitions delivered again were not stored")
	}
	if ids := manualEntries(rt); len(ids) != 1 || ids[0] != 7 {
		t.Fatalf("the request delivered again was not taken: %v", ids)
	}
}

// TestSchedulerNoBusyRetry: a scheduled set whose start cannot be recorded
// is written again by its worker with a wait that doubles each time (no busy
// retry), while the rounds and the monitoring steps go on; nothing of the
// run is recorded, and at the stop the start is given up.
func TestSchedulerNoBusyRetry(t *testing.T) {
	tr := &parTransport{}
	rt := newParallelRuntime(t, tr, 2)
	rt.recordRetry = 100 * time.Millisecond
	var logBuf bytes.Buffer
	var logMu sync.Mutex
	rt.Logger = log.New(lockedWriter{&logMu, &logBuf}, "", 0)
	d := testSet(1, PurposeRunOnly, "true")
	d.RunMode = RunModeSchedule
	d.Schedule = &CommandSchedule{Kind: ScheduleDaily, Time: "00:00"}
	defs := defsOf(d)
	rt.storeCommandDefinitions(defs)
	tr.resp.CommandSets = defs
	execOnDB(t, rt.DB, `UPDATE command_schedules SET armed_at = armed_at - 2*86400`)
	execOnDB(t, rt.DB, `CREATE TEMP TRIGGER fail_last_due BEFORE UPDATE OF last_due ON command_schedules
		BEGIN SELECT RAISE(ABORT, 'refused by the test'); END`)
	start := time.Now()
	stop, _ := stopWhen(t, func() bool { return len(tr.seen()) >= 3 })
	rt.Run(stop)
	took := time.Since(start)
	logMu.Lock()
	logged := logBuf.String()
	logMu.Unlock()
	retries := strings.Count(logged, "storing it failed, tried again in")
	// Waits of 100 ms, 200 ms, 400 ms, ...: about log2 of the time taken.
	if retries == 0 || retries > 2+int(math.Log2(float64(took/(100*time.Millisecond))+1)) {
		t.Fatalf("%d repeated starts in %s", retries, took)
	}
	if !strings.Contains(logged, `command set "setb" not started: the agent stops (recording its start failed: `) {
		t.Fatalf("the start must be given up at the stop: %q", logged)
	}
	if all, _ := countRuns(rt.DB); all != 0 {
		t.Fatalf("runs recorded: %d", all)
	}
	if st, _ := models.GetCommandSchedule(rt.DB.Read(), 1); st == nil || st.LastDue != 0 {
		t.Fatalf("schedule state: %+v", st)
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

// TestEarlyRoundAfterManualRun: when a manually requested run ended, the next
// round comes at once (one second after the previous one) and carries its
// result; the end of a scheduled run does not bring a round forward.
func TestEarlyRoundAfterManualRun(t *testing.T) {
	requireBash(t)
	tr := &parTransport{}
	rt := newParallelRuntime(t, tr, 1)
	rt.link.syncInterval = 60
	tr.resp.DefaultIntervalSec = 60
	sched := testSet(2, PurposeRunOnly, "sleep 1.5")
	sched.RunMode = RunModeSchedule
	sched.Schedule = &CommandSchedule{Kind: ScheduleOnce, At: FormatScheduleAt(time.Now().Add(-time.Minute))}
	tr.resp.CommandSets = defsOf(testSet(1, PurposeRunOnly, "sleep 1.5; echo manual"), sched)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		rt.Run(stop)
	}()
	// The first round brings the definitions; the scheduled once set runs
	// at once. Its end brings no round.
	waitFor(t, "the scheduled run", func() bool { _, finished := countRuns(rt.DB); return finished == 1 })
	time.Sleep(2 * time.Second)
	if n := len(tr.seen()); n != 1 {
		t.Fatalf("rounds after a scheduled run ended: %d, want only the first", n)
	}
	// A manual request, put into the queue as a sync delivers it (the next
	// regular round is a minute away); the central keeps delivering it.
	tr.mu.Lock()
	tr.resp.CommandRequests = manualRequests(1, 1)
	tr.mu.Unlock()
	receiveRequests(rt, manualRequests(1, 1))
	waitFor(t, "the manual run", func() bool { _, finished := countRuns(rt.DB); return finished == 2 })
	ended := time.Now()
	var closing *CommandRunReport
	waitFor(t, "the early round with the result", func() bool {
		tr.mu.Lock()
		defer tr.mu.Unlock()
		for _, r := range tr.reports {
			for i, run := range r.CommandRuns {
				if run.RequestID == 1 && run.Finished {
					closing = &r.CommandRuns[i]
					return true
				}
			}
		}
		return false
	})
	if d := time.Since(ended); d > 3*time.Second || len(tr.seen()) != 2 {
		t.Fatalf("the round after the manual run came after %s (%d rounds)", d, len(tr.seen()))
	}
	if string(closing.Results[0].Output) != "manual\n" {
		t.Fatalf("the early round did not carry the result of the manual run: %+v", closing)
	}
	close(stop)
	<-done
}
