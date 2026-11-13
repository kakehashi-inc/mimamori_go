package modules

import (
	"fmt"
	"os"
	"path/filepath"
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
	syncs   []syncSeen
	reports []*ReportRequest
	// reportFlows holds the flow (goroutine number) of each report attempt.
	reportFlows []uint64
	attempts    []time.Time
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
	seen := syncSeen{inWaitLoop: inWaitLoop(), goroutine: goroutineNumber()}
	if f.db != nil {
		runs, _ := models.ListUnfinishedCommandExecutions(f.db.Read())
		seen.running = len(runs)
	}
	// A little time inside the sync, so that two steps at once would overlap.
	time.Sleep(20 * time.Millisecond)
	f.mu.Lock()
	f.syncs = append(f.syncs, seen)
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

func (f *parTransport) Report(r *ReportRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts = append(f.attempts, time.Now())
	f.reportFlows = append(f.reportFlows, goroutineNumber())
	if f.failReports {
		return os.ErrDeadlineExceeded
	}
	f.reports = append(f.reports, r)
	return nil
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
	return strings.Contains(string(buf[:runtime.Stack(buf, false)]), "modules.waitProcess")
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

// newParallelRuntime is newCommandRuntime with n workers, the first
// monitoring step due at once and a check interval of one second.
func newParallelRuntime(t *testing.T, tr *parTransport, n int) *AgentRuntime {
	t.Helper()
	rt := newCommandRuntime(t, tr)
	rt.workers = n
	rt.sched.lastMonitor = time.Time{}
	rt.mon.defaultInterval = 1
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

// TestSchedulerIdleWorkerMonitors: with three workers and one long set, the
// monitoring steps run at the interval on an idle worker, never inside the
// wait for the set's command.
func TestSchedulerIdleWorkerMonitors(t *testing.T) {
	requireBash(t)
	tr := &parTransport{}
	rt := newParallelRuntime(t, tr, 3)
	tr.resp.CommandSets = defsOf(testSet(1, PurposeRunOnly, "sleep 3.2"))
	tr.resp.CommandRequests = manualRequests(1, 1)
	rt.Run(stopWhenRunsFinished(t, rt.DB, 1))
	during := 0
	for i, s := range tr.seen() {
		if s.inWaitLoop {
			t.Fatalf("step %d ran inside the command's wait loop although workers were idle", i+1)
		}
		if s.running > 0 {
			during++
		}
	}
	if during < 2 {
		t.Fatalf("the monitoring must go on while the set runs: %d steps during the run (%+v)", during, tr.seen())
	}
	if m := tr.maxInSync.Load(); m != 1 {
		t.Fatalf("two monitoring steps at once: %d", m)
	}
}

// TestSchedulerAllBusyMonitors: with three workers all running long sets,
// the steps go on at the interval inside the commands' wait loops, one at a
// time.
func TestSchedulerAllBusyMonitors(t *testing.T) {
	requireBash(t)
	tr := &parTransport{}
	rt := newParallelRuntime(t, tr, 3)
	tr.resp.CommandSets = defsOf(testSet(1, PurposeRunOnly, "sleep 3.5"), testSet(2, PurposeRunOnly, "sleep 3.5"),
		testSet(3, PurposeRunOnly, "sleep 3.5"))
	tr.resp.CommandRequests = manualRequests(1, 1, 2, 3)
	rt.Run(stopWhenRunsFinished(t, rt.DB, 3))
	inside := 0
	for _, s := range tr.seen() {
		if s.inWaitLoop {
			inside++
		}
	}
	if inside < 2 {
		t.Fatalf("the monitoring must go on while every worker runs a set: %d steps (%+v)", inside, tr.seen())
	}
	if m := tr.maxInSync.Load(); m != 1 {
		t.Fatalf("two monitoring steps at once: %d", m)
	}
}

// TestSchedulerSameSetNotTwice: with four workers, requests for sets 1, 2, 1
// and a due schedule of set 1 never run set 1 twice at the same time, while
// set 2 runs next to it.
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
	one.Schedule = &CommandSchedule{Kind: ScheduleEveryHours, Every: 1}
	two := testSet(2, PurposeRunOnly, line(2))
	defs := defsOf(one, two)
	rt.storeCommandDefinitions(defs)
	// Armed two hours ago: the latest occurrence of set 1 is due.
	if err := rt.DB.Write(func(tx models.Writer) error {
		_, err := tx.Exec(`UPDATE command_schedules SET armed_at = armed_at - 7200`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
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
	rt.mon.defaultInterval = 60
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
			if r.State != ResultError || len(r.Results) != 1 || r.Results[0].Reason != ReasonInterrupted {
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
		if err := models.SetCommandExecutionRunning(tx, "a", 1, 105); err != nil {
			return err
		}
		return models.SetCommandExecutionRunning(tx, "b", 0, 101)
	}); err != nil {
		t.Fatal(err)
	}
	rt2 := NewAgentRuntime(rt.DB, &cmdTransport{}, nil)
	rt2.workers = 3
	stop := make(chan struct{})
	close(stop)
	rt2.Run(stop)
	got := map[string]CommandRunReport{}
	for _, r := range runReports(queuedReports(t, rt2)) {
		got[r.RunUID] = r
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
// to the monitoring steps, the central receives every report once, every
// run's reports in the order start, commands, close, and the host
// information in exactly one report per step.
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
	// What the last step did not send yet.
	rt.flushOutbox()
	if q := queuedReports(t, rt); len(q) != 0 {
		t.Fatalf("%d reports left queued", len(q))
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	seenIDs := map[string]bool{}
	periodic, hostInfos := 0, 0
	next := map[string]int{} // run UID: the next result position expected (-1: the start)
	closedRuns := 0
	for _, r := range tr.reports {
		if r.ReportID == "" || seenIDs[r.ReportID] {
			t.Fatalf("report identity %q missing or received twice", r.ReportID)
		}
		seenIDs[r.ReportID] = true
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
	// One report per step, each with host information (the step that the
	// stop came into may report without it).
	if periodic != len(tr.syncs) || hostInfos < periodic-1 {
		t.Fatalf("%d step reports, %d with host information, for %d steps", periodic, hostInfos, len(tr.syncs))
	}
}

// shrinkingTransport queues a large command set report and lets the limit
// shrink the queued rows when the first report of a flush arrives (as
// another worker would), and records what it receives.
type shrinkingTransport struct {
	rt       *AgentRuntime
	received []ReportRequest
}

func (f *shrinkingTransport) Sync(*SyncRequest) (*SyncResponse, error) { return &SyncResponse{}, nil }

func (f *shrinkingTransport) Report(r *ReportRequest) error {
	if len(f.received) == 0 {
		// Over this the output of the queued run_only reports is dropped,
		// oldest first, and nothing more.
		f.rt.out.limit = 2000
		f.rt.queueCommandReport(&ReportRequest{CommandRuns: []CommandRunReport{{RunUID: "late", Purpose: PurposeRunOnly,
			Results: []CommandResultReport{{Output: []byte(strings.Repeat("y", 3000))}}}}})
	}
	f.received = append(f.received, *r)
	return nil
}

// TestOutboxRewriteShrinksOnly: a limit applied while a flush runs makes the
// later rows smaller; they are sent as they are now, never with content
// they no longer hold, and the row the flush did not list is not sent.
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
	if len(tr.received) != 3 || len(tr.received[0].CommandRequestAcks) != 1 {
		t.Fatalf("received: %d reports", len(tr.received))
	}
	for _, r := range tr.received[1:] {
		for _, run := range r.CommandRuns {
			if run.RunUID == "late" {
				t.Fatal("a row queued during the flush is sent by the next flush")
			}
			for _, res := range run.Results {
				if len(res.Output) != 0 || !res.Truncated {
					t.Fatalf("run %s sent with the output the limit dropped: %d bytes", run.RunUID, len(res.Output))
				}
			}
		}
	}
}
