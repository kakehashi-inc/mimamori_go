package modules

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mimamori/app/models"
)

// parTransport is a fake central for runs with several workers. Every field
// is guarded by mu; inSync counts the syncs in progress at the same time.
type parTransport struct {
	mu   sync.Mutex
	resp SyncResponse
	// syncs records each sync: whether it ran inside the wait loop of a
	// running command, the flow it ran on and the sets that ran.
	syncs []syncSeen
	// batches are the report requests received, reports their items in
	// order.
	batches []*ReportBatch
	reports []*ReportRequest
	// reportFlows holds the flow (goroutine number) of each report attempt.
	reportFlows []uint64
	attempts    []time.Time
	// order is the sequence of the requests received: "sync" at the end of
	// a sync, "report" at every report attempt.
	order       []string
	failReports bool
	onSync      func(n int)
	// db, when set, is read at each sync for the running sets.
	db *DB

	inSync, maxInSync atomic.Int32
}

type syncSeen struct {
	inWaitLoop bool
	goroutine  uint64
	running    int
	freeWorker bool
}

func (f *parTransport) Sync(req *SyncRequest) (*SyncResponse, error) {
	n := f.inSync.Add(1)
	defer f.inSync.Add(-1)
	for {
		m := f.maxInSync.Load()
		if n <= m || f.maxInSync.CompareAndSwap(m, n) {
			break
		}
	}
	seen := syncSeen{inWaitLoop: inWaitLoop(), goroutine: goroutineNumber(), freeWorker: req.FreeWorker}
	if f.db != nil {
		runs, _ := models.ListUnfinishedCommandExecutions(f.db.Read())
		seen.running = len(runs)
	}
	// A little time inside the sync, so that two steps at once would overlap.
	time.Sleep(20 * time.Millisecond)
	f.mu.Lock()
	f.syncs = append(f.syncs, seen)
	f.order = append(f.order, "sync")
	count := len(f.syncs)
	r := f.resp
	onSync := f.onSync
	f.mu.Unlock()
	if onSync != nil {
		onSync(count)
	}
	if r.CommandSets != nil && req.CommandSig == r.CommandSets.Signature {
		r.CommandSets = nil
	}
	return &r, nil
}

func (f *parTransport) Report(b *ReportBatch) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts = append(f.attempts, time.Now())
	f.order = append(f.order, "report")
	f.reportFlows = append(f.reportFlows, goroutineNumber())
	if f.failReports {
		return os.ErrDeadlineExceeded
	}
	f.batches = append(f.batches, b)
	for i := range b.Reports {
		f.reports = append(f.reports, &b.Reports[i])
	}
	return nil
}

// roundsOrder matches the requests of a run of the agent: one sync and then
// one report per round, except the round whose sync the stop came after (the
// rest of a round is not sent after the stop), and the last report.
var roundsOrder = regexp.MustCompile(`^(sync report )*(sync )?report$`)

// checkRounds checks, with f.mu held, the order of the requests (roundsOrder)
// and that only the last batch says that the agent stops.
func (f *parTransport) checkRounds(t *testing.T) {
	t.Helper()
	if seq := strings.Join(f.order, " "); !roundsOrder.MatchString(seq) {
		t.Fatalf("requests %q: want one sync and one report per round and the last report", seq)
	}
	for i, b := range f.batches {
		if b.Stopping != (i == len(f.batches)-1) {
			t.Fatalf("batch %d of %d: stopping %v", i+1, len(f.batches), b.Stopping)
		}
	}
}

func (f *parTransport) seen() []syncSeen {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.syncs)
}

// inWaitLoop reports whether the calling flow is inside the wait loop of a
// running command.
func inWaitLoop() bool {
	buf := make([]byte, 64<<10)
	return strings.Contains(string(buf[:runtime.Stack(buf, false)]), "(*commandProcess).waitMain")
}

// stopWhen returns a stop channel for Run that is closed once cond holds
// (looked at every 5 ms), and the time it was closed (valid once Run has
// returned).
func stopWhen(t *testing.T, cond func() bool) (<-chan struct{}, *time.Time) {
	t.Helper()
	stop, quit, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	at := new(time.Time)
	go func() {
		defer close(done)
		for {
			if cond() {
				*at = time.Now()
				close(stop)
				return
			}
			select {
			case <-quit:
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}()
	t.Cleanup(func() {
		close(quit)
		<-done
	})
	return stop, at
}

// countRuns returns the number of recorded runs and of finished ones.
func countRuns(db *DB) (all, finished int) {
	_ = db.Read().QueryRow(`SELECT count(*), COALESCE(SUM(finished_at > 0), 0) FROM command_executions`).Scan(&all, &finished)
	return
}

// newParallelRuntime is newCommandRuntime with n workers and a check
// interval of one second.
func newParallelRuntime(t *testing.T, tr *parTransport, n int) *AgentRuntime {
	t.Helper()
	rt := newCommandRuntime(t, tr)
	rt.SetWorkers(n)
	rt.link.syncInterval = 1
	tr.db = rt.DB
	tr.resp.DefaultIntervalSec = 1
	return rt
}

// manualRequests returns one request per set ID, oldest first, with IDs from
// first on.
func manualRequests(first int64, sets ...int64) []CommandRunRequest {
	now := time.Now().Unix()
	var out []CommandRunRequest
	for i, s := range sets {
		out = append(out, CommandRunRequest{ID: first + int64(i), SetID: s, RequestedAt: now - 100 + int64(i), ExpiresAt: now + 3600})
	}
	return out
}

// allReports returns what the fake central received followed by what is
// still queued.
func allReports(t *testing.T, rt *AgentRuntime, tr *parTransport) []ReportRequest {
	t.Helper()
	var out []ReportRequest
	tr.mu.Lock()
	for _, r := range tr.reports {
		out = append(out, *r)
	}
	tr.mu.Unlock()
	return append(out, queuedReports(t, rt)...)
}

// TestRoundsWhileWorkersBusy (C3, TO-3, MON-1, UNITS): with three workers all
// running long sets, the rounds go on every interval on the communication
// flow (never inside the wait for a command), one at a time; the monitoring
// step waits in the queue until a worker is free, so that never more entries
// are processed than there are workers.
func TestRoundsWhileWorkersBusy(t *testing.T) {
	requireBash(t)
	tr := &parTransport{}
	rt := newParallelRuntime(t, tr, 3)
	dir := t.TempDir()
	var sets []CommandSetDef
	for id := int64(1); id <= 3; id++ {
		sets = append(sets, testSet(id, PurposeRunOnly, fmt.Sprintf("touch %s/live.$$; sleep 3.5; rm %s/live.$$", dir, dir)))
	}
	tr.resp.CommandSets = defsOf(sets...)
	tr.resp.CommandRequests = manualRequests(1, 1, 2, 3)
	var steps, maxUnits atomic.Int32
	quit := make(chan struct{})
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		for {
			select {
			case <-quit:
				return
			case <-time.After(3 * time.Millisecond):
			}
			es, _ := os.ReadDir(dir)
			rt.sched.mu.Lock()
			units := int32(len(es))
			if rt.sched.monitoring {
				units++
				steps.Add(1)
			}
			rt.sched.mu.Unlock()
			if units > maxUnits.Load() {
				maxUnits.Store(units)
			}
		}
	}()
	rt.Run(stopWhenRunsFinished(t, rt.DB, 3))
	close(quit)
	<-watched
	during := 0
	for i, s := range tr.seen() {
		if s.inWaitLoop {
			t.Fatalf("sync %d ran inside the wait for a command", i+1)
		}
		if s.running == 3 {
			during++
		}
	}
	if during < 2 {
		t.Fatalf("the rounds must go on while every worker runs a set: %d syncs during the runs (%+v)", during, tr.seen())
	}
	if m := tr.maxInSync.Load(); m != 1 {
		t.Fatalf("two rounds at once: %d", m)
	}
	if m := maxUnits.Load(); m > 3 {
		t.Fatalf("%d entries processed at once by 3 workers", m)
	}
}

// TestOneWorkerLongCommand (C3, TO-3): with one worker and a long command the
// monitoring step waits in the queue until the run ended, while the rounds
// go on; the time limit and the grace of the command are not delayed by
// anything.
func TestOneWorkerLongCommand(t *testing.T) {
	requireBash(t)
	tr := &parTransport{}
	rt := newParallelRuntime(t, tr, 1)
	rt.cmdKillGrace = time.Second
	set := testSet(1, PurposeRunOnly, "trap '' TERM; sleep 30")
	set.Commands[0].TimeoutSec = 3
	tr.resp.CommandSets = defsOf(set)
	tr.resp.CommandRequests = manualRequests(1, 1)
	// The rounds take long (as a slow central would); the command does not
	// wait for them.
	tr.onSync = func(n int) {
		if n > 1 {
			time.Sleep(500 * time.Millisecond)
		}
	}
	var stepsDuring atomic.Int32
	quit := make(chan struct{})
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		for {
			select {
			case <-quit:
				return
			case <-time.After(3 * time.Millisecond):
			}
			rt.sched.mu.Lock()
			if rt.sched.monitoring && len(rt.sched.running) > 0 {
				stepsDuring.Add(1)
			}
			rt.sched.mu.Unlock()
		}
	}()
	rt.Run(stopWhenRunsFinished(t, rt.DB, 1))
	close(quit)
	<-watched
	if n := stepsDuring.Load(); n != 0 {
		t.Fatalf("a monitoring step ran while the only worker ran the command (%d samples)", n)
	}
	during := 0
	for _, s := range tr.seen() {
		if s.inWaitLoop {
			t.Fatal("a sync ran inside the wait for the command")
		}
		if s.running > 0 {
			during++
		}
	}
	if during < 2 {
		t.Fatalf("the rounds must go on while the command runs: %d syncs during the run", during)
	}
	last, _ := lastRun(t, runReports(allReports(t, rt, tr)))
	res := last.Results[0]
	// Time limit 3 s and grace 1 s: ended after 4 s, not later.
	if res.Reason != ReasonTimeout || res.FinishedAt-res.StartedAt > 5 {
		t.Fatalf("the time limit and the grace were delayed: %+v", res)
	}
}

// TestSchedulerSameSetNotTwice: with four workers, requests for sets 1, 2, 1
// and a scheduled occurrence of set 1 that passed never run set 1 twice at
// the same time, while set 2 runs next to it.
func TestSchedulerSameSetNotTwice(t *testing.T) {
	requireBash(t)
	tr := &parTransport{}
	rt := newParallelRuntime(t, tr, 4)
	dir := t.TempDir()
	logPath := filepath.Join(dir, "log")
	// mkdir fails when another run of the same set holds the directory.
	line := func(set int) string {
		lock := filepath.Join(dir, "lock"+strconv.Itoa(set))
		return fmt.Sprintf(`mkdir %[1]s || exit 3; echo "start %[2]d $(date +%%s%%N)" >> %[3]s; sleep 0.6; echo "end %[2]d $(date +%%s%%N)" >> %[3]s; rmdir %[1]s`,
			lock, set, logPath)
	}
	one := testSet(1, PurposeRunOnly, line(1))
	one.RunMode = RunModeSchedule
	// Today's 00:00 has passed and was not run: at once, and again after a
	// manual run until it was run.
	one.Schedule = &CommandSchedule{Kind: ScheduleDaily, Time: "00:00"}
	two := testSet(2, PurposeRunOnly, line(2))
	defs := defsOf(one, two)
	rt.storeCommandDefinitions(defs)
	execOnDB(t, rt.DB, `UPDATE command_schedules SET armed_at = armed_at - 2*86400`)
	tr.resp.CommandSets = defs
	tr.resp.CommandRequests = manualRequests(1, 1, 2, 1)
	rt.Run(stopWhenRunsFinished(t, rt.DB, 4))

	for _, r := range runReports(allReports(t, rt, tr)) {
		if r.Finished && r.State != ResultSuccess {
			t.Fatalf("a run of set %d failed (two runs of it at once?): %+v", r.SetID, r)
		}
	}
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	type span struct{ start, end int64 }
	var spans [3][]span
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var what string
		var set int
		var at int64
		if _, err := fmt.Sscanf(l, "%s %d %d", &what, &set, &at); err != nil {
			t.Fatalf("log line %q: %v", l, err)
		}
		if what == "start" {
			spans[set] = append(spans[set], span{start: at})
		} else {
			spans[set][len(spans[set])-1].end = at
		}
	}
	if len(spans[1]) != 3 || len(spans[2]) != 1 {
		t.Fatalf("runs: set 1 %d, set 2 %d", len(spans[1]), len(spans[2]))
	}
	for i := 1; i < len(spans[1]); i++ {
		if spans[1][i].start < spans[1][i-1].end {
			t.Fatalf("two runs of set 1 overlap: %+v", spans[1])
		}
	}
	if s2 := spans[2][0].start; s2 > spans[1][0].end {
		t.Fatalf("set 2 must start while the first run of set 1 runs: %+v %+v", spans[1], spans[2])
	}
}

// TestStopWithSeveralSets: a stop during three running sets ends every
// command at once, closes and records every run and makes exactly one send
// attempt after all workers returned.
func TestStopWithSeveralSets(t *testing.T) {
	requireBash(t)
	tr := &parTransport{}
	rt := newParallelRuntime(t, tr, 3)
	rt.link.syncInterval = 60
	tr.resp.DefaultIntervalSec = 60
	tr.resp.CommandSets = defsOf(testSet(1, PurposeRunOnly, "sleep 30", "true"), testSet(2, PurposeRunOnly, "sleep 30"),
		testSet(3, PurposeRunOnly, "sleep 30"))
	tr.resp.CommandRequests = manualRequests(1, 1, 2, 3)
	tr.onSync = func(int) {
		tr.mu.Lock()
		tr.failReports = true
		tr.mu.Unlock()
	}
	stop, stopAt := stopWhen(t, func() bool {
		runs, _ := models.ListUnfinishedCommandExecutions(rt.DB.Read())
		running := 0
		for _, r := range runs {
			if r.RunningPosition >= 0 {
				running++
			}
		}
		return running == 3
	})
	rt.Run(stop)
	if d := time.Since(*stopAt); d > 3*time.Second {
		t.Fatalf("the stop took %s", d)
	}
	if all, finished := countRuns(rt.DB); all != 3 || finished != 3 {
		t.Fatalf("runs recorded %d, finished %d", all, finished)
	}
	closed := map[int64]bool{}
	for _, r := range runReports(queuedReports(t, rt)) {
		if r.Finished {
			if r.State != ResultError || r.Reason != ReasonInterrupted || len(r.Results) != 1 || r.Results[0].Reason != ReasonInterrupted {
				t.Fatalf("stopped run: %+v", r)
			}
			closed[r.SetID] = true
		}
	}
	if len(closed) != 3 {
		t.Fatalf("closed runs: %v", closed)
	}
	after := 0
	for _, a := range tr.attempts {
		if a.After(*stopAt) {
			after++
		}
	}
	if after != 1 {
		t.Fatalf("one send attempt after the stop: %d of %d", after, len(tr.attempts))
	}
}

// TestRestartAfterCrashSeveralRuns: runs left unfinished by a crash during
// several runs are each reported interrupted, with the command that ran, at
// the next Run.
func TestRestartAfterCrashSeveralRuns(t *testing.T) {
	rt := newCommandRuntime(t, &cmdTransport{})
	rt.storeCommandDefinitions(defsOf(testSet(1, PurposeRunOnly, "true", "sleep 100"), testSet(2, PurposeRunOnly, "sleep 100")))
	for _, e := range []*models.CommandExecution{
		{RunUID: "a", SetID: 1, SetRevision: 1, Trigger: TriggerManual, RequestID: 4, StartedAt: 100},
		{RunUID: "b", SetID: 2, SetRevision: 1, Trigger: TriggerSchedule, ScheduledFor: 90, StartedAt: 100},
	} {
		if err := rt.DB.Write(func(tx models.Writer) error { return models.InsertCommandExecution(tx, e) }); err != nil {
			t.Fatal(err)
		}
	}
	if err := rt.DB.Write(func(tx models.Writer) error {
		if err := models.SetCommandExecutionRunning(tx, "a", 1, 105, 0, ""); err != nil {
			return err
		}
		return models.SetCommandExecutionRunning(tx, "b", 0, 101, 0, "")
	}); err != nil {
		t.Fatal(err)
	}
	tr2 := &cmdTransport{}
	rt2 := NewAgentRuntime(rt.DB, tr2, nil)
	rt2.SetWorkers(3)
	stop := make(chan struct{})
	close(stop)
	rt2.Run(stop)
	// The interrupted runs went with the last report of the stop.
	if len(tr2.batches) != 1 || !tr2.batches[0].Stopping || len(queuedReports(t, rt2)) != 0 {
		t.Fatalf("reports at the stop: %d batches, %d left queued", len(tr2.batches), len(queuedReports(t, rt2)))
	}
	got := map[string]CommandRunReport{}
	for _, r := range tr2.reports {
		for _, run := range r.CommandRuns {
			got[run.RunUID] = run
		}
	}
	for uid, pos := range map[string]int{"a": 1, "b": 0} {
		r := got[uid]
		if !r.Finished || r.Reason != ReasonInterrupted || len(r.Results) != 1 || r.Results[0].Position != pos ||
			r.Results[0].CommandLine != "sleep 100" {
			t.Fatalf("run %s: %+v", uid, r)
		}
	}
	if open, _ := models.ListUnfinishedCommandExecutions(rt.DB.Read()); len(open) != 0 {
		t.Fatalf("unfinished runs left: %d", len(open))
	}
}

// TestOutboxOrderWithWorkers: with four workers running many short sets next
// to the monitoring steps, the central receives every batch once and one
// batch per round (none for the round whose sync the stop came after, and
// the last one at the stop), every run's reports in the order start,
// commands, close, and the host information only in the reports of the
// monitoring steps.
func TestOutboxOrderWithWorkers(t *testing.T) {
	requireBash(t)
	tr := &parTransport{}
	rt := newParallelRuntime(t, tr, 4)
	var sets []CommandSetDef
	var ids []int64
	for id := int64(1); id <= 6; id++ {
		sets = append(sets, testSet(id, PurposeRunOnly, "sleep 0.3; echo a", "echo b"))
		ids = append(ids, id, id)
	}
	tr.resp.CommandSets = defsOf(sets...)
	tr.resp.CommandRequests = manualRequests(1, ids...)
	stop, _ := stopWhen(t, func() bool {
		_, finished := countRuns(rt.DB)
		return finished == len(ids) && len(tr.seen()) >= 3
	})
	rt.Run(stop)
	if q := queuedReports(t, rt); len(q) != 0 {
		t.Fatalf("%d reports left queued", len(q))
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	seenIDs := map[string]bool{}
	for _, b := range tr.batches {
		if !reportIDForm.MatchString(b.ReportID) || seenIDs[b.ReportID] {
			t.Fatalf("batch identity %q not of its form or received twice", b.ReportID)
		}
		seenIDs[b.ReportID] = true
	}
	if len(tr.batches) != len(tr.attempts) {
		t.Fatalf("%d batches for %d report requests", len(tr.batches), len(tr.attempts))
	}
	tr.checkRounds(t)
	periodic, hostInfos := 0, 0
	next := map[string]int{} // run UID: the next result position expected (-1: the start)
	closedRuns := 0
	for _, r := range tr.reports {
		if len(r.CommandRuns) == 0 && len(r.CommandRequestAcks) == 0 && r.CommandApply == nil {
			periodic++
			if r.HostInfo != nil {
				hostInfos++
			}
		} else if r.HostInfo != nil {
			t.Fatalf("host information in a report about command sets: %+v", r)
		}
		for _, run := range r.CommandRuns {
			pos, known := next[run.RunUID]
			switch {
			case !known:
				if len(run.Results) != 0 || run.Finished {
					t.Fatalf("run %s: the start must come first: %+v", run.RunUID, run)
				}
				next[run.RunUID] = 0
			case pos < 0:
				t.Fatalf("run %s: a report after its close", run.RunUID)
			default:
				if len(run.Results) != 1 || run.Results[0].Position != pos {
					t.Fatalf("run %s: result out of order (want position %d): %+v", run.RunUID, pos, run.Results)
				}
				next[run.RunUID] = pos + 1
				if run.Finished {
					if pos != 1 {
						t.Fatalf("run %s closed after %d commands", run.RunUID, pos+1)
					}
					next[run.RunUID] = -1
					closedRuns++
				}
			}
		}
	}
	if closedRuns != len(ids) {
		t.Fatalf("closed runs received: %d of %d", closedRuns, len(ids))
	}
	// The monitoring steps' reports, each with host information (the step
	// the stop came into may report without it).
	if periodic == 0 || hostInfos < periodic-1 {
		t.Fatalf("%d step reports, %d with host information", periodic, hostInfos)
	}
}

// shrinkingTransport queues a large command set report and lets the limit
// shrink the queued rows when the first batch arrives (as another worker
// would), loses the answer of that first batch and records what it receives.
type shrinkingTransport struct {
	rt       *AgentRuntime
	received []ReportBatch
}

func (f *shrinkingTransport) Sync(*SyncRequest) (*SyncResponse, error) { return &SyncResponse{}, nil }

func (f *shrinkingTransport) Report(b *ReportBatch) error {
	f.received = append(f.received, *b)
	if len(f.received) == 1 {
		// Over this the output of the queued run_only reports is dropped,
		// oldest first, and nothing more.
		f.rt.out.limit = 2000
		f.rt.queueCommandReport(&ReportRequest{CommandRuns: []CommandRunReport{{RunUID: "late", Purpose: PurposeRunOnly,
			Results: []CommandResultReport{{Output: []byte(strings.Repeat("y", 3000))}}}}})
		return errors.New("the answer was lost")
	}
	return nil
}

// TestOutboxRewriteShrinksOnly: a limit applied while a batch is recorded
// makes its items smaller; the batch is sent again with the same identity
// and its items as they are now, never with content they no longer hold,
// and the item queued meanwhile does not join it but goes with the next
// batch.
func TestOutboxRewriteShrinksOnly(t *testing.T) {
	tr := &shrinkingTransport{}
	rt := newCommandRuntime(t, tr)
	tr.rt = rt
	rt.out.limit = 1 << 30
	big := []byte(strings.Repeat("x", 4000))
	rt.queueCommandReport(&ReportRequest{CommandRequestAcks: []CommandRequestAck{{RequestID: 1, Status: RequestAckStarted}}})
	for _, uid := range []string{"r1", "r2"} {
		rt.queueCommandReport(&ReportRequest{CommandRuns: []CommandRunReport{{RunUID: uid, Purpose: PurposeRunOnly,
			Results: []CommandResultReport{{Output: big, OutputBytes: 4000}}}}})
	}
	rt.flushOutbox()
	rt.flushOutbox()
	if len(tr.received) != 2 || tr.received[0].ReportID != tr.received[1].ReportID {
		t.Fatalf("received %d batches, want the same batch twice", len(tr.received))
	}
	if first := tr.received[0].Reports; len(first) != 3 || len(first[1].CommandRuns[0].Results[0].Output) != 4000 {
		t.Fatalf("first batch: %+v", first)
	}
	again := tr.received[1].Reports
	if len(again) != 3 || len(again[0].CommandRequestAcks) != 1 {
		t.Fatalf("batch sent again: %d items", len(again))
	}
	for _, r := range again[1:] {
		for _, run := range r.CommandRuns {
			if run.RunUID == "late" {
				t.Fatal("an item queued while the batch was recorded joined it")
			}
			for _, res := range run.Results {
				if len(res.Output) != 0 || !res.Truncated {
					t.Fatalf("run %s sent with the output the limit dropped: %d bytes", run.RunUID, len(res.Output))
				}
			}
		}
	}
	rt.flushOutbox()
	if len(tr.received) != 3 || tr.received[2].ReportID == tr.received[0].ReportID ||
		len(tr.received[2].Reports) != 1 || tr.received[2].Reports[0].CommandRuns[0].RunUID != "late" {
		t.Fatalf("next batch: %+v", tr.received[2:])
	}
}

// TestReportOneRequestPerStep: while the central takes no reports, two sets
// run to their end and many items are queued; every round, before and after
// the central takes reports again, makes exactly one sync and one report
// request (the round whose sync the stop came after sends no report), the
// stop one more report (the last one, stopping true), and the queued items
// reach the central.
func TestReportOneRequestPerStep(t *testing.T) {
	requireBash(t)
	tr := &parTransport{}
	rt := newParallelRuntime(t, tr, 1)
	lines := make([]string, 20)
	for i := range lines {
		lines[i] = "echo c"
	}
	tr.resp.CommandSets = defsOf(testSet(1, PurposeRunOnly, lines...), testSet(2, PurposeRunOnly, "echo d"))
	tr.resp.CommandRequests = manualRequests(1, 1, 2)
	tr.failReports = true
	steps := func() int {
		tr.mu.Lock()
		defer tr.mu.Unlock()
		return len(tr.syncs)
	}
	waitUntil := func(what string, cond func() bool) {
		deadline := time.Now().Add(30 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		rt.Run(stop)
	}()
	waitUntil("both runs", func() bool { all, fin := countRuns(rt.DB); return all == 2 && fin == 2 })
	n := steps()
	waitUntil("a step after the runs", func() bool { return steps() > n })
	tr.mu.Lock()
	tr.failReports = false
	tr.resp.CommandRequests = nil
	tr.mu.Unlock()
	waitUntil("the queue to empty", func() bool { return models.CountOutbox(rt.DB.Read()) == 0 })
	close(stop)
	<-done
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.checkRounds(t)
	runs := 0
	for _, r := range tr.reports {
		for _, run := range r.CommandRuns {
			if run.Finished {
				runs++
			}
		}
	}
	if runs != 2 {
		t.Fatalf("closing reports received: %d", runs)
	}
}
