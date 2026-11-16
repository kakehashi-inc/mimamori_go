package modules

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"mimamori/app/models"
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

// TestStopWhileStartRecordWaits: the start record of a run, or the record of
// the command that starts, waits for the write connection (another write
// holds it, as a write that waits for a lock of another process does) when
// the stop comes. When it gets the connection after the stop, it is not
// written: for the run nothing is recorded and nothing starts; the command
// is not released and the run is closed as interrupted.
func TestStopWhileStartRecordWaits(t *testing.T) {
	for _, command := range []bool{false, true} {
		t.Run(map[bool]string{false: "run", true: "command"}[command], func(t *testing.T) {
			var logs lockedLog
			ran := filepath.Join(t.TempDir(), "ran")
			rt, stop, sig := stopRuntime(t, &logs, "touch "+ran+"; sleep 30")
			execOnDB(t, rt.DB, `CREATE TABLE marks (pos INTEGER);
				CREATE TRIGGER seen_mark AFTER UPDATE OF running_position ON command_executions WHEN NEW.running_position >= 0
				BEGIN INSERT INTO marks VALUES (NEW.running_position); END`)
			hold, held, free := make(chan struct{}), make(chan struct{}), make(chan struct{})
			holdWrite := func() {
				go func() {
					defer close(free)
					_ = rt.DB.Write(func(models.Writer) error {
						close(held)
						<-hold
						return nil
					})
				}()
				<-held
			}
			if command {
				// Right after the start of the run was stored, before its
				// command is started and recorded.
				var once sync.Once
				rt.Logger = log.New(writerFunc(func(p []byte) (int, error) {
					if strings.Contains(string(p), "started (manual") {
						once.Do(holdWrite)
					}
					return logs.Write(p)
				}), "", 0)
			} else {
				holdWrite()
			}
			job := nextCommandJob(rt)
			done := runInBackground(rt, job)
			<-held
			// The worker's record waits for the write connection: two flows
			// are in a Write, the holder and the worker.
			waitFor(t, "the worker's record to wait for the write connection", func() bool { return writeFlowCount.Load() >= 2 })
			close(stop)
			close(hold)
			<-free
			if started := returned(t, done); started != command {
				t.Fatalf("the run counts as started: %v", started)
			}
			if _, err := os.Stat(ran); err == nil || len(sig.list()) > 0 {
				t.Fatalf("the command ran after the stop (signals %s)", sig)
			}
			var marks int
			if err := rt.DB.Read().QueryRow(`SELECT count(*) FROM marks`).Scan(&marks); err != nil || marks != 0 {
				t.Fatalf("the command was recorded as running after the stop (%d, %v)", marks, err)
			}
			all, _ := countRuns(rt.DB)
			if !command {
				if all != 0 || len(runReports(queuedReports(t, rt))) != 0 {
					t.Fatalf("after the stop the run was recorded (%d)", all)
				}
				if !strings.Contains(logs.String(), `command set "setb" not started: the agent stops`) {
					t.Fatalf("log: %q", logs.String())
				}
				return
			}
			last, executed := lastRun(t, runReports(queuedReports(t, rt)))
			if all != 1 || executed != 0 || last.Reason != ReasonInterrupted {
				t.Fatalf("runs %d; closing report %+v (%d results)", all, last, executed)
			}
			if !strings.Contains(logs.String(), `command 1 of run `+last.RunUID+` not started: the agent stops`) {
				t.Fatalf("log: %q", logs.String())
			}
		})
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

// runInFlow runs rt.Run on a flow of its own; wait waits at most d for it to
// return and gives how long it waited.
func runInFlow(t *testing.T, rt *AgentRuntime, stop <-chan struct{}) (wait func(d time.Duration) time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		rt.Run(stop)
	}()
	return func(d time.Duration) time.Duration {
		t.Helper()
		start := time.Now()
		select {
		case <-done:
		case <-time.After(d):
			buf := make([]byte, 4<<20)
			t.Fatalf("Run did not return within %s\n%s", d, buf[:runtime.Stack(buf, true)])
		}
		return time.Since(start)
	}
}

// agentGoroutines returns the goroutines that run code of the agent, once
// none is left or after a second.
func agentGoroutines() []string {
	var out []string
	for range 100 {
		buf := make([]byte, 8<<20)
		n := runtime.Stack(buf, true)
		out = nil
		for _, g := range strings.Split(string(buf[:n]), "\n\n") {
			for _, m := range []string{"modules.(*AgentRuntime)", "modules.(*leftoverPipes)", "modules.(*agentScheduler)",
				"modules.(*commandProcess)", "modules.(*outbox)", "modules.RunTool"} {
				if strings.Contains(g, m) {
					out = append(out, g)
					break
				}
			}
		}
		if len(out) == 0 {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return out
}

// TestStopWhileEndRecordRetried: the result of a command cannot be stored
// (written again every 2 s); the stop comes. Run returns soon, nothing of
// the agent is left, the run stays unfinished, and the next start reports
// it as interrupted.
func TestStopWhileEndRecordRetried(t *testing.T) {
	requireBash(t)
	var logs lockedLog
	tr := &parTransport{}
	rt := newParallelRuntime(t, tr, 2)
	rt.Logger = logs.logger()
	rt.recordRetry = 2 * time.Second
	tr.resp.CommandSets = defsOf(testSet(1, PurposeRunOnly, "sleep 0.6; echo hi"))
	tr.resp.CommandRequests = manualRequests(1, 1)
	stop := make(chan struct{})
	wait := runInFlow(t, rt, stop)
	waitFor(t, "the run to start", func() bool { all, _ := countRuns(rt.DB); return all == 1 })
	execOnDB(t, rt.DB, `CREATE TRIGGER fail_out BEFORE INSERT ON outbox BEGIN SELECT RAISE(ABORT, 'injected failure'); END`)
	waitFor(t, "the failed attempt to store the result", func() bool {
		return strings.Contains(logs.String(), "the result of command 1") && strings.Contains(logs.String(), "storing it failed")
	})
	close(stop)
	if took := wait(20 * time.Second); took > 3*time.Second {
		t.Errorf("Run returned %s after the stop", took)
	}
	if left := agentGoroutines(); len(left) != 0 {
		t.Errorf("goroutines of the agent left:\n%s", strings.Join(left, "\n\n"))
	}
	open, err := models.ListUnfinishedCommandExecutions(rt.DB.Read())
	if err != nil || len(open) != 1 {
		t.Fatalf("the run whose end could not be stored must stay unfinished: %d %v", len(open), err)
	}
	execOnDB(t, rt.DB, `DROP TRIGGER fail_out`)
	rt2 := NewAgentRuntime(rt.DB, tr, nil)
	recoverAtStart(t, rt2)
	if last, _ := lastRun(t, runReports(queuedReports(t, rt2))); last.RunUID != open[0].RunUID || last.Reason != ReasonInterrupted {
		t.Errorf("the next start must report run %s as interrupted: %+v", open[0].RunUID, last)
	}
}

// TestStopWhileWaitingForHeldSet: the workers wait for a set kept by a
// surviving process group; the stop comes. Run returns at once, nothing
// starts and the surviving group is not signalled.
func TestStopWhileWaitingForHeldSet(t *testing.T) {
	requireBash(t)
	survivor := startSurvivor(t)
	sig := recordSignals(t)
	tr := &parTransport{}
	rt := newParallelRuntime(t, tr, 2)
	rt.sched.hold(1, survivor.Pid)
	tr.resp.CommandSets = defsOf(testSet(1, PurposeRunOnly, "echo again"))
	tr.resp.CommandRequests = manualRequests(1, 1)
	stop := make(chan struct{})
	wait := runInFlow(t, rt, stop)
	waitFor(t, "the manual entry of the held set", func() bool { return len(manualEntries(rt)) == 1 })
	time.Sleep(1500 * time.Millisecond)
	close(stop)
	if took := wait(20 * time.Second); took > 2*time.Second {
		t.Errorf("Run returned %s after the stop while its workers only waited", took)
	}
	if all, _ := countRuns(rt.DB); all != 0 || len(sig.list()) != 0 || !processAlive(survivor.Pid) {
		t.Errorf("runs %d, signals %s, survivor alive %v", all, sig, processAlive(survivor.Pid))
	}
	if left := agentGoroutines(); len(left) != 0 {
		t.Errorf("goroutines of the agent left: %d", len(left))
	}
}

// TestStopEveryCommandAtOnce: four workers run four commands; the stop sends
// SIGTERM to every group at once, the grace (here 3 s) belongs to each group
// as a whole, and only what is left then gets SIGKILL:
//
//	set 1, 2: end at SIGTERM
//	set 3:    ignores SIGTERM: SIGKILL when the grace is over
//	set 4:    its main process ends at SIGTERM, a member of the group ends
//	          one second after SIGTERM: no SIGKILL
func TestStopEveryCommandAtOnce(t *testing.T) {
	requireBash(t)
	dir := t.TempDir()
	f := func(name string) string { return filepath.Join(dir, name) }
	onTerm := func(n int) string {
		return fmt.Sprintf(`trap 'exit 0' TERM; echo $$ > %s; sleep 30 & wait`, f(fmt.Sprintf("pid%d", n)))
	}
	ignore := fmt.Sprintf(`trap '' TERM; echo $$ > %s; sleep 30`, f("pid3"))
	member := fmt.Sprintf(`( trap 'sleep 1; echo $EPOCHREALTIME > %s; exit 0' TERM; echo > %s; sleep 30 & wait ) & trap 'exit 0' TERM; echo $$ > %s; sleep 30 & wait`,
		f("member4"), f("ready4"), f("pid4"))
	sig := recordSignals(t)
	tr := &parTransport{}
	rt := newParallelRuntime(t, tr, 4)
	rt.cmdKillGrace = 3 * time.Second
	tr.resp.CommandSets = defsOf(testSet(1, PurposeRunOnly, onTerm(1)), testSet(2, PurposeRunOnly, onTerm(2)),
		testSet(3, PurposeRunOnly, ignore), testSet(4, PurposeRunOnly, member))
	tr.resp.CommandRequests = manualRequests(1, 1, 2, 3, 4)
	stop := make(chan struct{})
	wait := runInFlow(t, rt, stop)
	pids := map[int]int{}
	for n := 1; n <= 4; n++ {
		pids[n] = pidFrom(t, f(fmt.Sprintf("pid%d", n)))
	}
	// Every script has set its traps: each writes its file after them.
	waitFor(t, "the member of set 4", func() bool { _, err := os.Stat(f("ready4")); return err == nil })
	stopAt := time.Now()
	close(stop)
	took := wait(30 * time.Second)
	terms, kills := map[int]time.Time{}, map[int]time.Time{}
	for _, s := range sig.list() {
		switch s.sig {
		case syscall.SIGTERM:
			terms[s.pgid] = s.at
		case syscall.SIGKILL:
			kills[s.pgid] = s.at
		}
	}
	for n := 1; n <= 4; n++ {
		if at, ok := terms[pids[n]]; !ok || at.Sub(stopAt) > 500*time.Millisecond {
			t.Errorf("set %d: SIGTERM %v, %s after the stop (want every running command at the stop)", n, ok, at.Sub(stopAt))
		}
	}
	if at, ok := kills[pids[3]]; !ok || at.Sub(stopAt) < 3*time.Second || at.Sub(stopAt) > 4*time.Second {
		t.Errorf("set 3 (ignores SIGTERM): SIGKILL %v, %s after the stop, want at the end of the grace of 3 s", ok, at.Sub(stopAt))
	}
	for _, n := range []int{1, 2, 4} {
		if _, ok := kills[pids[n]]; ok {
			t.Errorf("set %d: SIGKILL although its group was gone before the grace was over", n)
		}
	}
	if _, err := os.Stat(f("member4")); err != nil {
		t.Errorf("set 4: the member of the group did not get the rest of the grace: %v", err)
	}
	if took < 3*time.Second || took > 5*time.Second {
		t.Errorf("Run returned %s after the stop, want just after the grace of 3 s", took)
	}
	closed := map[int64]string{}
	for _, r := range runReports(allReports(t, rt, tr)) {
		if r.Finished {
			closed[r.SetID] = r.Reason
		}
	}
	for id := int64(1); id <= 4; id++ {
		if closed[id] != ReasonInterrupted {
			t.Errorf("set %d: run reason %q, want interrupted", id, closed[id])
		}
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if len(tr.batches) == 0 || !tr.batches[len(tr.batches)-1].Stopping {
		t.Errorf("the last report must say that the agent stops")
	}
}

// TestStopWhileMonitoringToolRuns: the stop comes while the monitoring step
// waits for an audit tool (a script that takes 30 s). The tool is ended, no
// other tool starts, Run returns and the last report says that the agent
// stops.
func TestStopWhileMonitoringToolRuns(t *testing.T) {
	requireBash(t)
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	script := fmt.Sprintf("echo \"$(date +%%s.%%N) $0\" >> %s\nsleep 30\n", calls)
	for _, name := range []string{"auditctl", "ausearch"} {
		fakeTool(t, name, script)
	}
	tr := &parTransport{}
	rt := newParallelRuntime(t, tr, 1)
	stop := make(chan struct{})
	wait := runInFlow(t, rt, stop)
	started := func() int { b, _ := os.ReadFile(calls); return strings.Count(string(b), "\n") }
	waitFor(t, "the monitoring step's tool", func() bool { return started() == 1 })
	time.Sleep(300 * time.Millisecond)
	stopAt := time.Now()
	close(stop)
	if took := wait(30 * time.Second); took > 3*time.Second {
		t.Errorf("Run returned %s after the stop while its monitoring step waited for a tool", took)
	}
	b, _ := os.ReadFile(calls)
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		ts, err := strconv.ParseFloat(strings.Fields(l)[0], 64)
		if err != nil || ts > float64(stopAt.UnixNano())/1e9 {
			t.Errorf("a tool was started after the stop: %q", l)
		}
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if len(tr.batches) == 0 || !tr.batches[len(tr.batches)-1].Stopping {
		t.Errorf("the last report must say that the agent stops (reports %d)", len(tr.batches))
	}
}

// TestStopEndsTheMonitoringStepsToolLikeACommand: the stop comes while the
// monitoring step waits for an audit tool that ignores SIGTERM. The tool
// gets SIGTERM and, after the grace, SIGKILL, so the stop takes the grace;
// no tool starts after the stop, and the step's report goes in the last
// report without host information. This is what the command references say
// of the monitoring step at agent stop.
func TestStopEndsTheMonitoringStepsToolLikeACommand(t *testing.T) {
	requireBash(t)
	prev := toolKillGrace
	toolKillGrace = time.Second
	t.Cleanup(func() { toolKillGrace = prev })
	dir := t.TempDir()
	calls, ready, pidFile := filepath.Join(dir, "calls"), filepath.Join(dir, "ready"), filepath.Join(dir, "pid")
	script := fmt.Sprintf("echo \"$(date +%%s.%%N) $0\" >> %s\ntrap '' TERM\necho $$ > %s\n: > %s\nsleep 30\n", calls, pidFile, ready)
	for _, name := range []string{"auditctl", "ausearch"} {
		fakeTool(t, name, script)
	}
	tr := &parTransport{}
	rt := newParallelRuntime(t, tr, 1)
	stop := make(chan struct{})
	wait := runInFlow(t, rt, stop)
	waitFor(t, "the monitoring step's tool to ignore SIGTERM", func() bool { _, err := os.Stat(ready); return err == nil })
	pid, err := strconv.Atoi(strings.TrimSpace(readFile(t, pidFile)))
	if err != nil {
		t.Fatal(err)
	}
	stopAt := time.Now()
	close(stop)
	took := wait(30 * time.Second)
	if took < toolKillGrace || took > toolKillGrace+5*time.Second {
		t.Errorf("Run returned %s after the stop; the tool's SIGKILL comes %s after its SIGTERM", took, toolKillGrace)
	}
	if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
		t.Errorf("the tool (pid %d) is still there after Run returned: %v", pid, err)
	}
	b, _ := os.ReadFile(calls)
	if lines := strings.Split(strings.TrimSpace(string(b)), "\n"); len(lines) != 1 {
		t.Errorf("tools started: %q, want only the one the stop ended", lines)
	}
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		ts, err := strconv.ParseFloat(strings.Fields(l)[0], 64)
		if err != nil || ts > float64(stopAt.UnixNano())/1e9 {
			t.Errorf("a tool was started after the stop: %q", l)
		}
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if len(tr.batches) == 0 || !tr.batches[len(tr.batches)-1].Stopping {
		t.Fatalf("the last report must say that the agent stops (reports %d)", len(tr.batches))
	}
	if len(tr.reports) == 0 {
		t.Fatalf("the report of the monitoring step is missing")
	}
	for i, r := range tr.reports {
		if r.HostInfo != nil {
			t.Errorf("report %d carries host information: %+v", i, r.HostInfo)
		}
	}
}
