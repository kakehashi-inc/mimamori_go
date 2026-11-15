package modules

import (
	"sync"
	"testing"
	"time"
)

// The communication flow of the agent: the rounds, the report of each round
// and the early round after a manual run.

// roundEvent is one request the fake central of these tests received.
type roundEvent struct {
	kind     string // "sync" or "report"
	start    time.Time
	items    int
	stopping bool
	batch    *ReportBatch
}

// roundTransport is a fake central whose syncs take syncTook and answer
// resp; it records every request.
type roundTransport struct {
	mu       sync.Mutex
	events   []roundEvent
	syncTook time.Duration
	resp     SyncResponse
}

func (f *roundTransport) Sync(req *SyncRequest) (*SyncResponse, error) {
	start := time.Now()
	time.Sleep(f.syncTook)
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.resp
	if r.CommandSets != nil && req.CommandSig == r.CommandSets.Signature {
		r.CommandSets = nil
	}
	f.events = append(f.events, roundEvent{kind: "sync", start: start})
	return &r, nil
}

func (f *roundTransport) Report(b *ReportBatch) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, roundEvent{kind: "report", start: time.Now(), items: len(b.Reports), stopping: b.Stopping, batch: b})
	return nil
}

func (f *roundTransport) all() []roundEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]roundEvent(nil), f.events...)
}

func (f *roundTransport) of(kind string) []roundEvent {
	var out []roundEvent
	for _, e := range f.all() {
		if e.kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// TestRoundTiming: the next round comes one check interval after the start
// of the previous one, at least one second after its end; every round sends
// one report; at the stop one last report says that the agent stops, and no
// sync is sent.
func TestRoundTiming(t *testing.T) {
	for _, c := range []struct {
		name     string
		took     time.Duration
		interval int
		wantGap  time.Duration // between the starts of two syncs
	}{
		{"from the start", 1200 * time.Millisecond, 3, 3 * time.Second},
		{"one second after the end", 2500 * time.Millisecond, 3, 3500 * time.Millisecond},
	} {
		t.Run(c.name, func(t *testing.T) {
			tr := &roundTransport{syncTook: c.took}
			tr.resp.DefaultIntervalSec = c.interval
			rt := newCommandRuntime(t, tr)
			rt.link.syncInterval = c.interval
			stop := make(chan struct{})
			done := make(chan struct{})
			go func() { defer close(done); rt.Run(stop) }()
			waitFor(t, "three reports", func() bool { return len(tr.of("report")) >= 3 })
			close(stop)
			<-done
			syncs := tr.of("sync")
			for i := 1; i < len(syncs); i++ {
				if gap := syncs[i].start.Sub(syncs[i-1].start); gap < c.wantGap-150*time.Millisecond || gap > c.wantGap+400*time.Millisecond {
					t.Errorf("sync %d started %s after sync %d, want about %s", i+1, gap, i, c.wantGap)
				}
			}
			reports := tr.of("report")
			if len(reports) != len(syncs)+1 {
				t.Errorf("%d syncs, %d reports; want one report per round and the last one", len(syncs), len(reports))
			}
			if all := tr.all(); all[len(all)-1].kind != "report" || !all[len(all)-1].stopping {
				t.Errorf("last request: %+v", all[len(all)-1])
			}
			for _, r := range reports[:len(reports)-1] {
				if r.stopping {
					t.Errorf("the report of a round says that the agent stops")
				}
			}
		})
	}
}

// TestEmptyReportSent: a round sends its report also when nothing is
// queued: an empty list with an identity.
func TestEmptyReportSent(t *testing.T) {
	tr := &roundTransport{}
	rt := newCommandRuntime(t, tr)
	rt.round()
	reports := tr.of("report")
	if len(reports) != 1 || reports[0].items != 0 || reports[0].batch.Reports == nil || !reportIDForm.MatchString(reports[0].batch.ReportID) {
		t.Fatalf("reports %+v", reports)
	}
}

// TestNoEarlyRoundForAnswers: only the end of a manually requested run that
// started brings the next round forward; answers (expired, unknown_set, an
// expiry found when a worker takes the request) do not.
func TestNoEarlyRoundForAnswers(t *testing.T) {
	requireBash(t)
	tr := &roundTransport{}
	tr.resp.DefaultIntervalSec = 60
	tr.resp.CommandSets = defsOf(testSet(1, PurposeRunOnly, "true"))
	rt := newCommandRuntime(t, tr)
	rt.link.syncInterval = 60
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { defer close(done); rt.Run(stop) }()
	waitFor(t, "the first round", func() bool { return len(tr.of("report")) >= 1 })
	now := time.Now().Unix()
	receiveRequests(rt, []CommandRunRequest{{ID: 1, SetID: 1, RequestedAt: now - 100, ExpiresAt: now - 1},
		{ID: 2, SetID: 9, RequestedAt: now, ExpiresAt: now + 3600}})
	// A request whose expiry passes while it waits: answered when a worker
	// takes it.
	rt.sched.mu.Lock()
	rt.sched.addLocked(&queueEntry{setID: 1, request: &CommandRunRequest{ID: 3, SetID: 1, RequestedAt: now, ExpiresAt: now - 1}}, time.Now())
	rt.sched.mu.Unlock()
	time.Sleep(3 * time.Second)
	close(stop)
	<-done
	if n := len(tr.of("sync")); n != 1 {
		t.Fatalf("%d syncs: an answer brought a round forward", n)
	}
}

// TestEarlyRoundCarriesOtherResults: the early round after a manual run
// carries the results of the other sets that ended meanwhile.
func TestEarlyRoundCarriesOtherResults(t *testing.T) {
	requireBash(t)
	tr := &roundTransport{}
	tr.resp.DefaultIntervalSec = 60
	sched := testSet(2, PurposeRunOnly, "sleep 1; echo scheduled")
	sched.RunMode = RunModeSchedule
	sched.Schedule = &CommandSchedule{Kind: ScheduleOnce, At: FormatScheduleAt(time.Now().Add(-time.Minute))}
	tr.resp.CommandSets = defsOf(testSet(1, PurposeRunOnly, "sleep 2; echo manual"), sched)
	rt := newCommandRuntime(t, tr)
	rt.link.syncInterval = 60
	rt.SetWorkers(3)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { defer close(done); rt.Run(stop) }()
	waitFor(t, "the first round", func() bool { return len(tr.of("report")) >= 1 })
	receiveRequests(rt, manualRequests(1, 1))
	waitFor(t, "the early round", func() bool { return len(tr.of("report")) >= 2 })
	close(stop)
	<-done
	var manual, scheduled bool
	for _, it := range tr.of("report")[1].batch.Reports {
		for _, run := range it.CommandRuns {
			manual = manual || (run.Finished && run.SetID == 1)
			scheduled = scheduled || (run.Finished && run.SetID == 2)
		}
	}
	if !manual || !scheduled {
		t.Fatalf("the early report: manual run %v, scheduled run %v", manual, scheduled)
	}
}

// monitorEntries returns the queued monitoring entries (copies).
func monitorEntries(s *agentScheduler) []queueEntry {
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

// TestSyncChangedAssignmentRemovesMonitorTime: a sync that brings a rule
// assignment that differs from the applied one removes the time of the
// queued monitoring entry (and while the step runs, it is queued again
// without a time); an assignment equal to the applied one keeps it.
func TestSyncChangedAssignmentRemovesMonitorTime(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	rt, s := newTestQueue(t, &now)
	tr := rt.comm.transport.(*cmdTransport)
	s.start()
	if u := pick(s); u == nil || !u.monitor {
		t.Fatal("the monitoring step first")
	}
	s.doneMonitor(10 * time.Minute)
	rt.sync()
	if m := monitorEntries(s); len(m) != 1 || m[0].startAt.IsZero() {
		t.Fatalf("no rule assigned and none applied: %+v", m)
	}
	ra := &RuleAssignment{ID: 5, Name: "r", Version: 1}
	tr.resp.Rule = ra
	rt.sync()
	if m := monitorEntries(s); len(m) != 1 || !m[0].startAt.IsZero() {
		t.Fatalf("a changed assignment: %+v", m)
	}
	// The step applies it (here: as if) and is queued again.
	if u := pick(s); u == nil || !u.monitor {
		t.Fatal("the monitoring step")
	}
	applied := *ra
	rt.mon.rule = &applied
	rt.link.publish(&rt.mon)
	s.doneMonitor(10 * time.Minute)
	rt.sync()
	if m := monitorEntries(s); len(m) != 1 || m[0].startAt.IsZero() {
		t.Fatalf("the assignment that is applied: %+v", m)
	}
	// A new version comes while the step runs.
	now = now.Add(11 * time.Minute)
	if u := pick(s); u == nil || !u.monitor {
		t.Fatal("the monitoring step")
	}
	tr.resp.Rule = &RuleAssignment{ID: 5, Name: "r", Version: 2}
	rt.sync()
	s.doneMonitor(10 * time.Minute)
	if m := monitorEntries(s); len(m) != 1 || !m[0].startAt.IsZero() {
		t.Fatalf("a changed assignment while the step ran: %+v", m)
	}
}
