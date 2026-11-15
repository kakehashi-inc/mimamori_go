package modules

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"mimamori/app/models"
)

// cmdTransport is a fake central for the command set flow: every sync
// answers with resp (definitions only when the signature differs) and
// reports are recorded.
type cmdTransport struct {
	resp  SyncResponse
	syncs int
	// syncReqs records every sync request as it was sent.
	syncReqs []SyncRequest
	// batches are the report requests received, reports their items in
	// order.
	batches []*ReportBatch
	reports []*ReportRequest
	onSync  func(n int)
	// failReports makes every report fail; attempts records when reports
	// were attempted.
	failReports bool
	attempts    []time.Time
}

func (f *cmdTransport) Sync(req *SyncRequest) (*SyncResponse, error) {
	f.syncs++
	f.syncReqs = append(f.syncReqs, *req)
	if f.onSync != nil {
		f.onSync(f.syncs)
	}
	r := f.resp
	if r.CommandSets != nil && req.CommandSig == r.CommandSets.Signature {
		r.CommandSets = nil
	}
	return &r, nil
}

func (f *cmdTransport) Report(b *ReportBatch) error {
	f.attempts = append(f.attempts, time.Now())
	if f.failReports {
		return os.ErrDeadlineExceeded
	}
	f.batches = append(f.batches, b)
	for i := range b.Reports {
		f.reports = append(f.reports, &b.Reports[i])
	}
	return nil
}

// stopWhenRunsFinished returns a stop channel for Run that is closed once n
// runs are recorded finished in the local database.
func stopWhenRunsFinished(t *testing.T, db *DB, n int) <-chan struct{} {
	t.Helper()
	stop, quit, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			var finished int
			err := db.Read().QueryRow(`SELECT count(*) FROM command_executions WHERE finished_at > 0`).Scan(&finished)
			if err == nil && finished >= n {
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
	return stop
}

// stopAfterFirstSync returns a stop channel for Run that is closed after
// delay from the first sync on, and the time the stop is due (set at that
// sync). first, when not nil, also runs at that sync.
func stopAfterFirstSync(tr *cmdTransport, delay time.Duration, first func()) (<-chan struct{}, *time.Time) {
	stop := make(chan struct{})
	stopAt := new(time.Time)
	tr.onSync = func(n int) {
		if n != 1 {
			return
		}
		if first != nil {
			first()
		}
		*stopAt = time.Now().Add(delay)
		time.AfterFunc(delay, func() { close(stop) })
	}
	return stop, stopAt
}

func newCommandRuntime(t *testing.T, tr AgentTransport) *AgentRuntime {
	t.Helper()
	MigrationsFS = os.DirFS("../..")
	db, err := OpenSQLite(filepath.Join(t.TempDir(), "agent.db"), SchemaAgentLocal)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	rt := NewAgentRuntime(db, tr, nil)
	rt.cmdKillGrace = 300 * time.Millisecond
	return rt
}

// testSet returns a valid set run as the agent's own user.
func testSet(id int64, purpose string, lines ...string) CommandSetDef {
	s := CommandSetSpec{Name: "set" + string(rune('a'+id)), Purpose: purpose, OSUser: agentUserName(), WorkDir: "/"}
	for _, l := range lines {
		s.Commands = append(s.Commands, CommandSpec{CommandLine: l})
	}
	s.Normalize()
	return CommandSetDef{ID: id, Revision: 1, CommandSetSpec: s}
}

func defsOf(sets ...CommandSetDef) *CommandDefinitions {
	return &CommandDefinitions{Signature: "sig" + time.Now().String(), Sets: sets}
}

// nextCommandJob takes the run that may start now from the queue as a
// worker does, without waiting, and queues the answers found on the way.
// The caller runs the job (or not) and ends it with rt.sched.doneRun.
func nextCommandJob(rt *AgentRuntime) *commandJob {
	rt.sched.mu.Lock()
	var job *commandJob
	var acks []CommandRequestAck
	for {
		u, a := rt.sched.pickLocked(rt.now())
		acks = append(acks, a...)
		if u == nil || !u.monitor {
			if u != nil {
				job = u.job
			}
			break
		}
		// The monitoring entry is not the test's: put it back.
		rt.sched.monitoring = false
		rt.sched.addLocked(&queueEntry{monitor: true, startAt: rt.now().Add(time.Hour)}, rt.now())
	}
	rt.sched.mu.Unlock()
	rt.queueAcks(acks)
	return job
}

// runNext takes the run that may start now, runs it to its end and ends it
// in the queue as a worker does; it reports whether there was one.
func runNext(rt *AgentRuntime) bool {
	job := nextCommandJob(rt)
	if job == nil {
		return false
	}
	rt.runCommandJob(job)
	rt.sched.doneRun(job)
	return true
}

// queuedEntries returns the entries of the queue (copies), oldest first.
func queuedEntries(rt *AgentRuntime) []queueEntry {
	rt.sched.mu.Lock()
	defer rt.sched.mu.Unlock()
	var out []queueEntry
	for _, e := range rt.sched.entries {
		out = append(out, *e)
	}
	return out
}

// receiveRequests hands manual requests to the queue as a sync does.
func receiveRequests(rt *AgentRuntime, reqs []CommandRunRequest) {
	rt.queueAcks(rt.sched.receive(reqs))
}

// queuedRuns returns the run reports queued in the outbox, in order.
func queuedReports(t *testing.T, rt *AgentRuntime) []ReportRequest {
	t.Helper()
	items, err := models.ListOutbox(rt.DB.Read())
	if err != nil {
		t.Fatal(err)
	}
	var out []ReportRequest
	for _, it := range items {
		var r ReportRequest
		if err := json.Unmarshal([]byte(it.Payload), &r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func runReports(reports []ReportRequest) []CommandRunReport {
	var out []CommandRunReport
	for _, r := range reports {
		out = append(out, r.CommandRuns...)
	}
	return out
}

func acksOf(reports []ReportRequest) []CommandRequestAck {
	var out []CommandRequestAck
	for _, r := range reports {
		out = append(out, r.CommandRequestAcks...)
	}
	return out
}

// lastRun returns the closing report of the last run and the number of
// executed commands of that run.
func lastRun(t *testing.T, runs []CommandRunReport) (CommandRunReport, int) {
	t.Helper()
	if len(runs) == 0 {
		t.Fatal("no run reported")
	}
	last := runs[len(runs)-1]
	if !last.Finished {
		t.Fatalf("the last report must close the run: %+v", last)
	}
	n := 0
	for _, r := range runs {
		if r.RunUID == last.RunUID {
			n += len(r.Results)
		}
	}
	return last, n
}

func TestCommandSetOnProblems(t *testing.T) {
	requireBash(t)
	cases := []struct {
		name     string
		mode     string
		first    string
		state    string
		executed int
	}{
		{"stop after failure", OnProblemsStop, "exit 1", ResultFailure, 1},
		{"stop after error", OnProblemsStop, "kill -KILL $$", ResultError, 1},
		{"continue on failure: failure", OnProblemsContinueOnFailure, "exit 1", ResultFailure, 2},
		{"continue on failure: error", OnProblemsContinueOnFailure, "kill -KILL $$", ResultError, 1},
		{"continue always: error", OnProblemsContinueAlways, "kill -KILL $$", ResultError, 2},
		{"success", OnProblemsStop, "true", ResultSuccess, 2},
	}
	for _, c := range cases {
		rt := newCommandRuntime(t, &cmdTransport{})
		def := testSet(1, PurposeRunOnly, c.first, "echo second")
		def.Commands[0].OnProblems = c.mode
		if !rt.runCommandJob(&commandJob{def: &def, trigger: TriggerManual}) {
			t.Fatalf("%s: not started", c.name)
		}
		runs := runReports(queuedReports(t, rt))
		last, n := lastRun(t, runs)
		if last.State != c.state || n != c.executed || last.CommandsTotal != 2 || last.Display != nil {
			t.Errorf("%s: state %s executed %d (%+v)", c.name, last.State, n, last)
		}
		// One report for the start and one per executed command.
		if len(runs) != 1+c.executed {
			t.Errorf("%s: %d reports", c.name, len(runs))
		}
		if open, _ := models.ListUnfinishedCommandExecutions(rt.DB.Read()); len(open) != 0 {
			t.Errorf("%s: the run must be recorded as finished", c.name)
		}
	}
}

func TestCommandSetDisplayValue(t *testing.T) {
	requireBash(t)
	rt := newCommandRuntime(t, &cmdTransport{})
	def := testSet(1, PurposeCollectOutput, "echo 'user=alice'; echo 'user=bob'", "echo 'user=carol host=h1'; exit 1", "echo plain")
	def.Commands[0].Patterns = []string{`user=(?P<user>\w+)`}
	def.Commands[1].Patterns = []string{`user=(?P<user>\w+)`, `host=(?<host>\w+)`}
	def.Commands[1].OnProblems = OnProblemsContinueOnFailure
	def.Joiner = JoinerComma
	rt.runCommandJob(&commandJob{def: &def, trigger: TriggerManual})
	last, n := lastRun(t, runReports(queuedReports(t, rt)))
	if n != 3 || last.State != ResultFailure || last.Display == nil {
		t.Fatalf("run: %+v", last)
	}
	got := []string{}
	for _, r := range last.Display.Rows {
		got = append(got, r.Name+"="+r.Value)
	}
	if strings.Join(got, "|") != "user=alice,bob,carol|host=h1|=plain" {
		t.Fatalf("display value: %v", got)
	}
	if last.Results[0].Values == nil || last.Results[0].Position != 2 {
		t.Fatalf("the closing report carries the last command: %+v", last.Results)
	}
}

// manualEntries returns the request IDs of the manual entries of the queue.
func manualEntries(rt *AgentRuntime) []int64 {
	var ids []int64
	for _, e := range queuedEntries(rt) {
		if e.request != nil {
			ids = append(ids, e.request.ID)
		}
	}
	return ids
}

// TestCommandRequestsRunOnce: delivered requests are answered from what is
// stored: one that arrived after its expiry is answered expired, one of a set
// the agent does not have unknown_set, the others are queued and run; a
// request delivered again (also to a restarted agent) is answered with its
// run and not run again.
func TestCommandRequestsRunOnce(t *testing.T) {
	requireBash(t)
	tr := &cmdTransport{}
	rt := newCommandRuntime(t, tr)
	now := time.Now().Unix()
	tr.resp.CommandSets = defsOf(testSet(1, PurposeRunOnly, "echo hi"))
	tr.resp.CommandRequests = []CommandRunRequest{
		{ID: 7, SetID: 1, RequestedAt: now - 10, ExpiresAt: now + 3600},
		{ID: 8, SetID: 1, RequestedAt: now - 20, ExpiresAt: now - 1}, // arrives after its expiry
		{ID: 9, SetID: 42, RequestedAt: now - 5, ExpiresAt: now + 3600},
	}
	rt.sync()
	if got := manualEntries(rt); len(got) != 1 || got[0] != 7 {
		t.Fatalf("queued requests: %v", got)
	}
	for runNext(rt) {
	}
	reports := queuedReports(t, rt)
	acks := map[int64]CommandRequestAck{}
	for _, a := range acksOf(reports) {
		acks[a.RequestID] = a
	}
	runs := runReports(reports)
	if acks[7].Status != RequestAckStarted || acks[7].RunUID == "" || acks[7].RunUID != runs[0].RunUID || runs[0].RequestID != 7 {
		t.Fatalf("started ack: %+v", acks[7])
	}
	if acks[8].Status != RequestAckExpired || acks[9].Status != RequestAckUnknownSet {
		t.Fatalf("acks: %+v", acks)
	}
	if reports[0].CommandApply == nil || reports[0].CommandApply.Signature != tr.resp.CommandSets.Signature {
		t.Fatalf("the stored definitions must be reported first: %+v", reports[0])
	}

	// Delivered again (the central did not process the answer yet), to the
	// same agent and to a restarted one: the request is answered with its
	// run each time and not run a second time.
	before := len(acksOf(reports))
	rt.sync()
	rt2 := NewAgentRuntime(rt.DB, tr, nil)
	if rt2.sched.signature() != tr.resp.CommandSets.Signature {
		t.Fatal("the stored definitions must be restored")
	}
	rt2.sync()
	if job := nextCommandJob(rt2); job != nil {
		t.Fatalf("a request must not run twice: %+v", job)
	}
	var again []CommandRequestAck
	for _, a := range acksOf(queuedReports(t, rt2))[before:] {
		if a.RequestID == 7 {
			again = append(again, a)
		}
	}
	if len(again) != 2 {
		t.Fatalf("the repeated request must be answered at every delivery: %+v", again)
	}
	for _, a := range again {
		if a.Status != RequestAckStarted || a.RunUID != acks[7].RunUID {
			t.Fatalf("the repeated request must be answered with its run: %+v", again)
		}
	}
	if n := len(runReports(queuedReports(t, rt2))); n != len(runs) {
		t.Fatalf("no new run: %d reports", n)
	}
	// Even a direct attempt is refused by the local record.
	err := rt.DB.Write(func(tx models.Writer) error {
		return models.InsertCommandExecution(tx, &models.CommandExecution{RunUID: "x", SetID: 1, RequestID: 7, Trigger: TriggerManual, StartedAt: now})
	})
	if err != models.ErrAlreadyExecuted {
		t.Fatalf("local record: %v", err)
	}
}

func TestCommandSetInvalidOnAgent(t *testing.T) {
	rt := newCommandRuntime(t, &cmdTransport{})
	bad := testSet(2, PurposeCollectOutput, "echo x")
	bad.Commands[0].Patterns = []string{`(no named group)`}
	rt.storeCommandDefinitions(defsOf(testSet(1, PurposeRunOnly, "true"), bad))
	if rt.sched.valid[1] == nil || rt.sched.valid[2] != nil {
		t.Fatalf("valid sets: %v", rt.sched.valid)
	}
	reports := queuedReports(t, rt)
	ap := reports[len(reports)-1].CommandApply
	if ap == nil || len(ap.Problems) != 1 || ap.Problems[0].SetID != 2 {
		t.Fatalf("apply result: %+v", ap)
	}
}

func TestCommandRunInterrupted(t *testing.T) {
	rt := newCommandRuntime(t, &cmdTransport{})
	rt.storeCommandDefinitions(defsOf(testSet(1, PurposeRunOnly, "sleep 100", "true")))
	if err := rt.DB.Write(func(tx models.Writer) error {
		return models.InsertCommandExecution(tx, &models.CommandExecution{RunUID: "u1", SetID: 1, SetRevision: 1, RequestID: 3,
			Trigger: TriggerManual, StartedAt: 100})
	}); err != nil {
		t.Fatal(err)
	}
	// The agent starts again and finds the run unfinished.
	rt2 := NewAgentRuntime(rt.DB, &cmdTransport{}, nil)
	rt2.recoverInterruptedRuns()
	runs := runReports(queuedReports(t, rt2))
	r := runs[len(runs)-1]
	if r.RunUID != "u1" || !r.Finished || r.State != ResultError || r.Reason != ReasonInterrupted || r.SetName != "setb" ||
		r.CommandsTotal != 2 || r.RequestID != 3 || r.StartedAt != 100 {
		t.Fatalf("interrupted run: %+v", r)
	}
	if open, _ := models.ListUnfinishedCommandExecutions(rt.DB.Read()); len(open) != 0 {
		t.Fatal("the interrupted run must be closed")
	}
}

// TestCommandRunInterruptedCommand: the command that ran when the agent
// ended gets an interrupted result at the next start, with its command line
// only when the stored definition is the one the run executed.
func TestCommandRunInterruptedCommand(t *testing.T) {
	rt := newCommandRuntime(t, &cmdTransport{})
	def := testSet(1, PurposeCollectOutput, "echo begin", "echo begin; sleep 120", "true")
	def.Commands[1].Note = "the long one"
	rt.storeCommandDefinitions(defsOf(def))
	for _, e := range []*models.CommandExecution{
		{RunUID: "same", SetID: 1, SetRevision: 1, Trigger: TriggerManual, StartedAt: 100},
		{RunUID: "older", SetID: 1, SetRevision: 0, Trigger: TriggerSchedule, ScheduledFor: 60, StartedAt: 90},
		{RunUID: "between", SetID: 1, SetRevision: 1, Trigger: TriggerManual, StartedAt: 110},
	} {
		if err := rt.DB.Write(func(tx models.Writer) error { return models.InsertCommandExecution(tx, e) }); err != nil {
			t.Fatal(err)
		}
	}
	if err := rt.DB.Write(func(tx models.Writer) error {
		if err := models.SetCommandExecutionRunning(tx, "same", 1, 105); err != nil {
			return err
		}
		if err := models.SetCommandExecutionRunning(tx, "older", 1, 95); err != nil {
			return err
		}
		// "between" ended between two commands: its last command was reported.
		if err := models.SetCommandExecutionRunning(tx, "between", 0, 111); err != nil {
			return err
		}
		return models.ClearCommandExecutionRunning(tx, "between")
	}); err != nil {
		t.Fatal(err)
	}
	before := len(queuedReports(t, rt))
	rt2 := NewAgentRuntime(rt.DB, &cmdTransport{}, nil)
	rt2.recoverInterruptedRuns()
	byUID := map[string]CommandRunReport{}
	for _, r := range runReports(queuedReports(t, rt2)[before:]) {
		byUID[r.RunUID] = r
	}
	now := time.Now().Unix()
	r := byUID["same"]
	if !r.Finished || r.State != ResultError || r.Reason != ReasonInterrupted || len(r.Results) != 1 {
		t.Fatalf("interrupted run: %+v", r)
	}
	res := r.Results[0]
	if res.Position != 1 || res.Kind != ResultError || res.Reason != ReasonInterrupted || res.ExitCode != nil ||
		res.StartedAt != 105 || res.FinishedAt < now-5 || len(res.Output) != 0 || res.Values != nil ||
		res.CommandLine != "echo begin; sleep 120" || res.Note != "the long one" {
		t.Fatalf("result of the running command: %+v", res)
	}
	r = byUID["older"]
	if len(r.Results) != 1 || r.Results[0].Position != 1 || r.Results[0].StartedAt != 95 ||
		r.Results[0].CommandLine != "" || r.Results[0].Note != "" {
		t.Fatalf("another revision gives no command line: %+v", r.Results)
	}
	if r = byUID["between"]; !r.Finished || len(r.Results) != 0 {
		t.Fatalf("no running command, no result: %+v", r)
	}
}

// TestCommandRunRecordsRunningCommand: while a command runs, the agent's
// database names it; after its report is queued, nothing is running.
func TestCommandRunRecordsRunningCommand(t *testing.T) {
	requireBash(t)
	rt := newCommandRuntime(t, &cmdTransport{})
	rt.storeCommandDefinitions(defsOf(testSet(1, PurposeRunOnly, "true", "sleep 1")))
	start := time.Now().Unix()
	seen := make(chan *models.CommandExecution, 1)
	quit := make(chan struct{})
	go func() {
		for {
			runs, _ := models.ListUnfinishedCommandExecutions(rt.DB.Read())
			if len(runs) == 1 && runs[0].RunningPosition == 1 {
				seen <- runs[0]
				return
			}
			select {
			case <-quit:
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}()
	rt.runCommandJob(&commandJob{def: rt.sched.valid[1], trigger: TriggerManual, requestID: 5})
	close(quit)
	select {
	case e := <-seen:
		if e.RunningStartedAt < start {
			t.Fatalf("running command seen during the run: %+v", e)
		}
	default:
		t.Fatal("the second command was not recorded as running while it ran")
	}
	e, err := models.CommandExecutionByRequest(rt.DB.Read(), 5)
	if err != nil || e == nil || e.FinishedAt == 0 || e.RunningPosition != -1 || e.RunningStartedAt != 0 {
		t.Fatalf("after the run: %+v %v", e, err)
	}
}

// entryOf returns the queued entry of a set (nil when none).
func entryOf(rt *AgentRuntime, setID int64) *queueEntry {
	for _, e := range queuedEntries(rt) {
		if !e.monitor && e.setID == setID {
			return &e
		}
	}
	return nil
}

// TestCommandSchedules: the queue holds one entry per scheduled set, at its
// next occurrence; an entry whose time came while no worker took it starts
// late with its own occurrence; after the run the entry of the next
// occurrence is queued. Unchanged definitions keep the schedule state, a
// changed schedule is armed again.
func TestCommandSchedules(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Skip("no time zone data")
	}
	rt := newCommandRuntime(t, &cmdTransport{})
	rt.loc = loc
	at := func(hh, mm int) time.Time { return time.Date(2026, 11, 12, hh, mm, 0, 0, loc) }
	now := at(9, 0).Add(30 * time.Second)
	rt.clock = func() time.Time { return now }

	every := testSet(1, PurposeRunOnly, "true")
	every.Schedule = &CommandSchedule{Kind: ScheduleEveryMinutes, Every: 5}
	every.RunMode = RunModeSchedule
	daily := testSet(2, PurposeRunOnly, "true")
	daily.Schedule = &CommandSchedule{Kind: ScheduleDaily, Time: "09:02"}
	daily.RunMode = RunModeSchedule
	manual := testSet(3, PurposeRunOnly, "true")
	rt.storeCommandDefinitions(defsOf(every, daily, manual))

	// Right after the definitions arrived: one entry per scheduled set, at
	// its next occurrence; none for the manual set.
	if e := entryOf(rt, 1); e == nil || !e.startAt.Equal(at(9, 5)) || e.scheduledFor != at(9, 5).Unix() {
		t.Fatalf("entry of the every-5-minutes set: %+v", e)
	}
	if e := entryOf(rt, 2); e == nil || !e.startAt.Equal(at(9, 2)) {
		t.Fatalf("entry of the daily set: %+v", e)
	}
	if e := entryOf(rt, 3); e != nil {
		t.Fatalf("entry of the manual set: %+v", e)
	}
	if job := nextCommandJob(rt); job != nil {
		t.Fatalf("nothing may start yet: %+v", job)
	}
	// 09:21: no worker took the entries meanwhile. The one that became
	// startable first (09:02) comes first; the every-5-minutes entry starts
	// late with its own occurrence (09:05), the one entry of the set.
	now = at(9, 21)
	requireBash(t)
	job := nextCommandJob(rt)
	if job == nil || job.def.ID != 2 || job.scheduledFor != at(9, 2).Unix() {
		t.Fatalf("first: %+v", job)
	}
	rt.runCommandJob(job)
	rt.sched.doneRun(job)
	job = nextCommandJob(rt)
	if job == nil || job.def.ID != 1 || job.scheduledFor != at(9, 5).Unix() {
		t.Fatalf("the late entry runs its own occurrence: %+v", job)
	}
	rt.runCommandJob(job)
	rt.sched.doneRun(job)
	if job := nextCommandJob(rt); job != nil {
		t.Fatalf("nothing more may start: %+v", job)
	}
	// After their runs: the next occurrences (the daily one tomorrow).
	if e := entryOf(rt, 1); e == nil || !e.startAt.Equal(at(9, 25)) {
		t.Fatalf("next entry of the every-5-minutes set: %+v", e)
	}
	if e := entryOf(rt, 2); e == nil || !e.startAt.Equal(at(9, 2).AddDate(0, 0, 1)) {
		t.Fatalf("next entry of the daily set: %+v", e)
	}
	runs := runReports(queuedReports(t, rt))
	if runs[0].Trigger != TriggerSchedule || runs[0].ScheduledFor == 0 || runs[0].StartedAt == 0 {
		t.Fatalf("run of a schedule: %+v", runs[0])
	}
	// Unchanged definitions delivered again keep the schedule state and the
	// entries; a changed schedule is armed again and its past occurrences do
	// not run.
	rt.storeCommandDefinitions(defsOf(every, daily, manual))
	if st, _ := models.GetCommandSchedule(rt.DB.Read(), 1); st.LastDue != at(9, 5).Unix() {
		t.Fatalf("schedule state: %+v", st)
	}
	if e := entryOf(rt, 1); e == nil || !e.startAt.Equal(at(9, 25)) {
		t.Fatalf("entry after the same definitions: %+v", e)
	}
	every.Schedule.Every = 10
	rt.storeCommandDefinitions(defsOf(every, daily))
	if st, _ := models.GetCommandSchedule(rt.DB.Read(), 1); st.LastDue != 0 || st.ArmedAt != now.Unix() {
		t.Fatalf("re-armed: %+v", st)
	}
	if e := entryOf(rt, 1); e == nil || !e.startAt.Equal(at(9, 30)) {
		t.Fatalf("entry of the changed schedule: %+v", e)
	}
	if e := entryOf(rt, 3); e != nil {
		t.Fatalf("a set no longer delivered keeps no entry: %+v", e)
	}
}

// TestManualEntryReplacesScheduled: a manual request of a scheduled set
// replaces its waiting entry; after the manual run the scheduled entry is
// computed again. A manual request of a set that runs waits in the queue
// until the run ended.
func TestManualEntryReplacesScheduled(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Skip("no time zone data")
	}
	requireBash(t)
	rt := newCommandRuntime(t, &cmdTransport{})
	rt.loc = loc
	now := time.Date(2026, 11, 12, 8, 0, 0, 0, loc)
	rt.clock = func() time.Time { return now }
	daily := testSet(1, PurposeRunOnly, "true")
	daily.Schedule = &CommandSchedule{Kind: ScheduleDaily, Time: "09:00"}
	daily.RunMode = RunModeSchedule
	rt.storeCommandDefinitions(defsOf(daily))
	execOnDB(t, rt.DB, `UPDATE command_schedules SET armed_at = armed_at - 86400`)
	expires := now.Unix() + 3600
	receiveRequests(rt, []CommandRunRequest{{ID: 1, SetID: 1, RequestedAt: now.Unix(), ExpiresAt: expires}})
	if es := queuedEntries(rt); len(es) != 1 || es[0].request == nil || es[0].request.ID != 1 {
		t.Fatalf("the manual entry must replace the scheduled one: %+v", es)
	}
	job := nextCommandJob(rt)
	if job == nil || job.trigger != TriggerManual {
		t.Fatalf("manual job: %+v", job)
	}
	// While it runs (09:10) a second request of the set waits in the queue.
	now = now.Add(70 * time.Minute)
	receiveRequests(rt, []CommandRunRequest{{ID: 2, SetID: 1, RequestedAt: now.Unix(), ExpiresAt: expires + 7200}})
	if got := manualEntries(rt); len(got) != 1 || got[0] != 2 {
		t.Fatalf("the request of the running set must wait: %v", got)
	}
	if j := nextCommandJob(rt); j != nil {
		t.Fatalf("the set runs: %+v", j)
	}
	rt.runCommandJob(job)
	rt.sched.doneRun(job)
	// After the run: the waiting manual entry, and no scheduled entry next
	// to it; after that run, 09:00 of today was not executed: at once.
	job = nextCommandJob(rt)
	if job == nil || job.requestID != 2 || entryOf(rt, 1) != nil {
		t.Fatalf("after the first run: %+v, entries %+v", job, queuedEntries(rt))
	}
	rt.runCommandJob(job)
	rt.sched.doneRun(job)
	if e := entryOf(rt, 1); e == nil || !e.startAt.IsZero() || e.scheduledFor != time.Date(2026, 11, 12, 9, 0, 0, 0, loc).Unix() {
		t.Fatalf("the scheduled entry after the manual runs: %+v", e)
	}
}

// TestCommandRunKeepsMonitoring runs the whole flow: a request delivered by
// sync runs while the monitoring step keeps going, and the results are sent
// with the following reports.
func TestCommandRunKeepsMonitoring(t *testing.T) {
	requireBash(t)
	tr := &cmdTransport{}
	rt := newCommandRuntime(t, tr)
	now := time.Now().Unix()
	tr.resp.DefaultIntervalSec = 1
	tr.resp.CommandSets = defsOf(testSet(1, PurposeRunOnly, "sleep 2.5; echo done", "echo second"))
	tr.resp.CommandRequests = []CommandRunRequest{{ID: 1, SetID: 1, RequestedAt: now, ExpiresAt: now + 3600}}
	rt.link.syncInterval = 1
	rt.Run(stopWhenRunsFinished(t, rt.DB, 1))
	if tr.syncs < 3 {
		t.Fatalf("the monitoring must go on while the command runs: %d syncs", tr.syncs)
	}
	var sent []ReportRequest
	for _, r := range tr.reports {
		sent = append(sent, *r)
	}
	sent = append(sent, queuedReports(t, rt)...)
	last, n := lastRun(t, runReports(sent))
	if last.State != ResultSuccess || n != 2 {
		t.Fatalf("run: %+v (%d commands)", last, n)
	}
	// Results of the first command were already sent while the second one
	// had not yet run, or right after: at least the start went out early.
	if len(runReports(func() []ReportRequest {
		var out []ReportRequest
		for _, r := range tr.reports {
			out = append(out, *r)
		}
		return out
	}())) == 0 {
		t.Fatal("the run's progress must be reported during the run")
	}
}

func TestCommandRunStoppedByAgentStop(t *testing.T) {
	requireBash(t)
	tr := &cmdTransport{}
	rt := newCommandRuntime(t, tr)
	now := time.Now().Unix()
	tr.resp.CommandSets = defsOf(testSet(1, PurposeRunOnly, "echo started; sleep 30", "echo never"))
	tr.resp.CommandRequests = []CommandRunRequest{{ID: 1, SetID: 1, RequestedAt: now, ExpiresAt: now + 3600}}
	stop, _ := stopAfterFirstSync(tr, 300*time.Millisecond, nil)
	start := time.Now()
	rt.Run(stop)
	if time.Since(start) > 10*time.Second {
		t.Fatalf("the stop took %s", time.Since(start))
	}
	// The reports of the stopped run are sent before Run returns.
	if q := queuedReports(t, rt); len(q) != 0 {
		t.Fatalf("the reports of the stopped run must be sent: %d queued", len(q))
	}
	var sent []ReportRequest
	for _, r := range tr.reports {
		sent = append(sent, *r)
	}
	last, n := lastRun(t, runReports(sent))
	if last.State != ResultError || n != 1 || last.Results[0].Reason != ReasonInterrupted || string(last.Results[0].Output) != "started\n" {
		t.Fatalf("stopped run: %+v", last)
	}
	// The stop cut the run: the run's own reason is interrupted too.
	if !last.Finished || last.Reason != ReasonInterrupted {
		t.Fatalf("reason of the stopped run %q, want %q", last.Reason, ReasonInterrupted)
	}
	if open, _ := models.ListUnfinishedCommandExecutions(rt.DB.Read()); len(open) != 0 {
		t.Fatal("a stopped run is reported, not left unfinished")
	}
}

// TestCommandRunStoppedSendsOnce: after a run closed by the stop, the agent
// makes one attempt to send; what was not sent stays queued.
func TestCommandRunStoppedSendsOnce(t *testing.T) {
	requireBash(t)
	tr := &cmdTransport{}
	rt := newCommandRuntime(t, tr)
	now := time.Now().Unix()
	tr.resp.CommandSets = defsOf(testSet(1, PurposeRunOnly, "sleep 30"))
	tr.resp.CommandRequests = []CommandRunRequest{{ID: 1, SetID: 1, RequestedAt: now, ExpiresAt: now + 3600}}
	stop, stopAt := stopAfterFirstSync(tr, 300*time.Millisecond, func() { tr.failReports = true })
	rt.Run(stop)
	after := 0
	for _, a := range tr.attempts {
		if a.After(*stopAt) {
			after++
		}
	}
	if after != 1 {
		t.Fatalf("one attempt after the stop: %d of %d", after, len(tr.attempts))
	}
	last, _ := lastRun(t, runReports(queuedReports(t, rt)))
	if last.Results[0].Reason != ReasonInterrupted {
		t.Fatalf("the closing report stays queued: %+v", last)
	}
}

func TestCommandOutbox(t *testing.T) {
	rt := newCommandRuntime(t, &fakeTransport{fail: true})
	big := strings.Repeat("x", 4000)
	mk := func(purpose string, finished bool) *ReportRequest {
		return &ReportRequest{CommandRuns: []CommandRunReport{{RunUID: purpose, Purpose: purpose, Finished: finished,
			Results: []CommandResultReport{{Output: []byte(big), OutputBytes: 4000}}}}}
	}
	rt.out.limit = 1 << 30
	rt.queueCommandReport(mk(PurposeCollectOutput, false))
	rt.queueCommandReport(mk(PurposeRunOnly, false))
	rt.queueCommandReport(&ReportRequest{CommandRequestAcks: []CommandRequestAck{{RequestID: 1, Status: RequestAckStarted}}})
	// A regular report does not replace queued command set reports.
	if err := queueReport(rt, &ReportRequest{}); err != nil {
		t.Fatal(err)
	}
	if n := models.CountOutbox(rt.DB.Read()); n != 4 {
		t.Fatalf("command set reports must be kept: %d", n)
	}
	// Over the limit: the output of run_only sets goes first.
	total := models.OutboxCommandBytes(rt.DB.Read())
	rt.out.limit = total - 1000
	if err := rt.DB.Write(func(tx models.Writer) error { _, _, err := rt.out.applyCommandLimit(tx); return err }); err != nil {
		t.Fatal(err)
	}
	runs := runReports(queuedReports(t, rt))
	if len(runs[0].Results[0].Output) == 0 || len(runs[1].Results[0].Output) != 0 || !runs[1].Results[0].Truncated {
		t.Fatalf("run_only output must be dropped first: %+v", runs)
	}
	// Far over the limit: outputs, then whole reports, oldest first.
	rt.out.limit = 10
	rt.queueCommandReport(mk(PurposeCollectOutput, true))
	if b := models.OutboxCommandBytes(rt.DB.Read()); b > 10 {
		t.Fatalf("the limit must hold: %d", b)
	}
	if acks := acksOf(queuedReports(t, rt)); len(acks) != 1 {
		t.Fatalf("answers to requests are kept: %+v", acks)
	}
}

// TestConfigSavedReachesTransport: a configuration changed by the central is
// handed to OnConfigSaved after it was saved (the agent's transport takes
// the token and the CAs from it); a sync that changes nothing does not call
// it.
func TestConfigSavedReachesTransport(t *testing.T) {
	tr := &cmdTransport{}
	rt := newCommandRuntime(t, tr)
	var saved []string
	rt.useConfig(&AgentConfig{AgentID: "id", CentralHost: "192.0.2.1", CentralPort: 8443, Token: "old", TokenName: "a"}, func(c *AgentConfig) {
		if stored := models.GetLocalState(rt.DB.Read(), agentCfgToken); stored != c.Token {
			t.Errorf("called before the configuration was saved: stored token %q", stored)
		}
		saved = append(saved, c.Token)
	})
	tr.resp.TokenRenewal = &TokenRenewal{Token: "new", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	rt.round()
	if len(saved) != 1 || saved[0] != "new" || rt.comm.config.Token != "new" {
		t.Fatalf("renewed token: %v, config %q", saved, rt.comm.config.Token)
	}
	rt.round()
	if len(saved) != 1 {
		t.Fatalf("a sync without changes must not call OnConfigSaved: %v", saved)
	}
}

// TestAgentStopEndsLeftoverReading (TO-1): a process left behind by a command
// that keeps writing holds the reading of the output after the command
// ended; agent stop ends it (SIGTERM to the process group; the writer ends
// at once, so nothing waits for the grace), the run is recorded and the
// agent ends.
func TestAgentStopEndsLeftoverReading(t *testing.T) {
	requireBash(t)
	tr := &parTransport{}
	rt := newParallelRuntime(t, tr, 1)
	rt.cmdKillGrace = 20 * time.Second
	pidFile := filepath.Join(t.TempDir(), "pid")
	tr.resp.CommandSets = defsOf(testSet(1, PurposeRunOnly, "( while :; do echo x; sleep 0.3; done ) & echo $! > "+pidFile+"; echo main"))
	tr.resp.CommandRequests = manualRequests(1, 1)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		rt.Run(stop)
	}()
	writer := pidFrom(t, pidFile)
	t.Cleanup(func() { _ = syscall.Kill(writer, syscall.SIGKILL) })
	time.Sleep(2500 * time.Millisecond)
	if _, finished := countRuns(rt.DB); finished != 0 {
		t.Fatal("the run ended while the process it left behind still wrote")
	}
	stopAt := time.Now()
	close(stop)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("agent stop did not end while a process left behind keeps writing")
	}
	if d := time.Since(stopAt); d > 4*time.Second {
		t.Fatalf("the stop took %s (the writer ends at the SIGTERM; grace 20 s)", d)
	}
	if all, finished := countRuns(rt.DB); all != 1 || finished != 1 {
		t.Fatalf("runs recorded %d, finished %d", all, finished)
	}
	last, _ := lastRun(t, runReports(allReports(t, rt, tr)))
	if out := string(last.Results[0].Output); last.State != ResultSuccess || !strings.Contains(out, "main\n") || strings.Count(out, "x\n") < 5 {
		t.Fatalf("the run keeps the result of the command's main process: %+v %q", last, last.Results[0].Output)
	}
	if processAlive(writer) {
		t.Fatal("the writer left behind did not get SIGTERM at the stop")
	}
}

// TestLeftoverWriterNotBlockedWhileWorkerBusy (TO-2): a process left behind
// by a command writes more than the pipe holds while the only worker runs a
// long command of another set: what it writes is read and dropped as it
// arrives, so it goes on while that command still runs. A pipe still held
// when the agent ends is closed and its reader ends with Run.
func TestLeftoverWriterNotBlockedWhileWorkerBusy(t *testing.T) {
	requireBash(t)
	tr := &parTransport{}
	rt := newParallelRuntime(t, tr, 1)
	dir := t.TempDir()
	after, pidFile := filepath.Join(dir, "after"), filepath.Join(dir, "pid")
	tr.resp.CommandSets = defsOf(
		testSet(1, PurposeRunOnly, fmt.Sprintf("( sleep 4; head -c 4194304 /dev/zero; touch %s; exec sleep 60 ) & echo $! > %s; echo main", after, pidFile)),
		testSet(2, PurposeRunOnly, "sleep 15"))
	tr.resp.CommandRequests = manualRequests(1, 1, 2)
	readersBefore := pipeReaders()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		rt.Run(stop)
	}()
	// Every wait for Run is bounded, so that a reader that does not end
	// fails the test instead of hanging it.
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			close(stop)
			select {
			case <-done:
			case <-time.After(30 * time.Second):
				t.Error("Run did not return after the stop")
			}
		}
	})
	holder := pidFrom(t, pidFile)
	t.Cleanup(func() { _ = syscall.Kill(holder, syscall.SIGKILL) })
	waitFor(t, "the long command of set 2", func() bool { all, _ := countRuns(rt.DB); return all == 2 })
	if n, r := rt.leftovers.held(), pipeReaders(); n != 1 || r < 1 {
		t.Fatalf("pipes held when set 2 started: %d, reader goroutines %d", n, r)
	}
	waitFile(t, after)
	if _, finished := countRuns(rt.DB); finished != 1 {
		t.Fatalf("the process left behind went on only after the long command ended (%d runs finished)", finished)
	}
	if !processAlive(holder) {
		t.Fatal("the process left behind ended")
	}
	stopped = true
	close(stop)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("Run did not return after the stop (a pipe reader did not end; %d pipes held)", rt.leftovers.held())
	}
	if n := rt.leftovers.held(); n != 0 {
		t.Fatalf("pipes held after Run returned: %d", n)
	}
	waitFor(t, "the reader goroutines to be gone", func() bool { return pipeReaders() <= readersBefore })
}

// pipeReaders returns the number of goroutines that read a pipe kept for
// processes left behind (leftoverPipes).
func pipeReaders() int {
	buf := make([]byte, 8<<20)
	n := runtime.Stack(buf, true)
	return strings.Count(string(buf[:n]), "(*leftoverPipes).read(")
}

// TestScheduledRunsCarrySkipped: the run of a scheduled set carries how many
// occurrences were skipped before it and their range: for hourly_at the
// occurrences that passed while the set's own run took long or while its
// entry waited for a worker, for daily the earlier days; never for the
// interval kinds.
func TestScheduledRunsCarrySkipped(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Skip("no time zone data")
	}
	requireBash(t)
	rt := newCommandRuntime(t, &cmdTransport{})
	rt.loc = loc
	at := func(day, hh, mm int) time.Time { return time.Date(2026, 11, day, hh, mm, 0, 0, loc) }
	now := at(12, 9, 0)
	rt.clock = func() time.Time { return now }
	hourly := testSet(1, PurposeRunOnly, "true")
	hourly.RunMode, hourly.Schedule = RunModeSchedule, &CommandSchedule{Kind: ScheduleHourlyAt, Minute: intPtr(30)}
	every := testSet(2, PurposeRunOnly, "true")
	every.RunMode, every.Schedule = RunModeSchedule, &CommandSchedule{Kind: ScheduleEveryMinutes, Every: 15}
	daily := testSet(3, PurposeRunOnly, "true")
	daily.RunMode, daily.Schedule = RunModeSchedule, &CommandSchedule{Kind: ScheduleDaily, Time: "08:00"}
	rt.storeCommandDefinitions(defsOf(hourly, every, daily))
	// The daily set last ran three days ago.
	execOnDB(t, rt.DB, fmt.Sprintf(`UPDATE command_schedules SET armed_at = %d, last_due = %d WHERE set_id = 3`,
		at(1, 0, 0).Unix(), at(9, 8, 0).Unix()))
	rt.sched.acceptDefinitions(rt.sched.defs, rt.sched.valid)
	skipped := func(setID int64) (int, int64, int64) {
		t.Helper()
		var last *CommandRunReport
		for _, r := range runReports(queuedReports(t, rt)) {
			if r.SetID == setID && r.Finished {
				r := r
				last = &r
			}
		}
		if last == nil {
			t.Fatalf("no run of set %d", setID)
		}
		return last.SkippedCount, last.SkippedFrom, last.SkippedTo
	}
	// The daily set: today's 08:00 at once, carrying the two earlier days.
	if !runNext(rt) {
		t.Fatal("the daily set did not run")
	}
	if n, from, to := skipped(3); n != 2 || from != at(10, 8, 0).Unix() || to != at(11, 8, 0).Unix() {
		t.Fatalf("daily: %d %d %d", n, from, to)
	}
	// 09:30: the hourly set and the 09:15 entry of the every-15-minutes set
	// (taken late, with its own occurrence): nothing skipped. Their runs take
	// long: they end at 11:45.
	now = at(12, 9, 30)
	var jobs []*commandJob
	for job := nextCommandJob(rt); job != nil; job = nextCommandJob(rt) {
		rt.runCommandJob(job)
		jobs = append(jobs, job)
	}
	if len(jobs) != 2 {
		t.Fatalf("jobs at 09:30: %d", len(jobs))
	}
	now = at(12, 11, 45)
	for _, job := range jobs {
		rt.sched.doneRun(job)
	}
	if n, _, _ := skipped(1); n != 0 {
		t.Fatalf("first hourly run: %d skipped", n)
	}
	if e := entryOf(rt, 1); e == nil || e.scheduledFor != at(12, 12, 30).Unix() {
		t.Fatalf("hourly entry after the long run: %+v", e)
	}
	if e := entryOf(rt, 2); e == nil || e.scheduledFor != at(12, 12, 0).Unix() {
		t.Fatalf("every-15-minutes entry after the long run: %+v", e)
	}
	// 12:30: the hourly run carries 10:30 and 11:30; the every-15-minutes
	// run (12:00, skipping 09:45 to 11:45) carries nothing.
	now = at(12, 12, 30)
	for runNext(rt) {
	}
	if n, from, to := skipped(1); n != 2 || from != at(12, 10, 30).Unix() || to != at(12, 11, 30).Unix() {
		t.Fatalf("hourly after the long run: %d %d %d", n, from, to)
	}
	if n, from, to := skipped(2); n != 0 || from != 0 || to != 0 {
		t.Fatalf("every 15 minutes: %d %d %d", n, from, to)
	}
}

// startSurvivor starts "sleep 60" in a session of its own (a process group
// that outlived an agent) and returns it; the test ends it and reaps it.
func startSurvivor(t *testing.T) *os.Process {
	t.Helper()
	sleep, err := LookupCommand("sleep")
	if err != nil {
		t.Skip("sleep is not available")
	}
	p, err := os.StartProcess(sleep, []string{"sleep", "60"}, &os.ProcAttr{Sys: &syscall.SysProcAttr{Setsid: true}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = p.Kill()
		_, _ = p.Wait()
	})
	return p
}

// TestSurvivingGroupKeepsSetFromStarting (STOP-2): a run found unfinished at
// start whose recorded process group still exists in the same boot is
// reported interrupted, and its set does not start (also not for a manual
// request) until the group has no process left that has not ended: a zombie
// nobody reaped yet does not keep it; the group is never signalled. A record
// of another boot, or of a group that is gone, keeps nothing.
func TestSurvivingGroupKeepsSetFromStarting(t *testing.T) {
	requireBash(t)
	boot, err := bootTime()
	if err != nil {
		t.Fatal(err)
	}
	survivor := startSurvivor(t)
	// A process group that is gone: a process that ended and was reaped.
	gone, err := os.StartProcess("/bin/sh", []string{"sh", "-c", "exit 0"}, &os.ProcAttr{Sys: &syscall.SysProcAttr{Setsid: true}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gone.Wait(); err != nil {
		t.Fatal(err)
	}
	tr := &parTransport{}
	rt := newParallelRuntime(t, tr, 2)
	rt.storeCommandDefinitions(defsOf(testSet(1, PurposeRunOnly, "echo again"), testSet(2, PurposeRunOnly, "echo other"),
		testSet(3, PurposeRunOnly, "echo third")))
	for _, e := range []struct {
		uid  string
		set  int64
		pgid int
		boot int64
	}{{"alive", 1, survivor.Pid, boot}, {"other-boot", 2, survivor.Pid, boot - 3600}, {"gone", 3, gone.Pid, boot}} {
		insertExecution(t, rt.DB, e.uid, 0, 0)
		execOnDB(t, rt.DB, fmt.Sprintf(`UPDATE command_executions SET set_id = %d, running_position = 0, running_pgid = %d, running_boot = %d
			WHERE run_uid = '%s'`, e.set, e.pgid, e.boot, e.uid))
	}
	var signalled atomic.Int32
	killProcessGroup = func(pid int, sig syscall.Signal) error {
		if sig != 0 && (pid == -survivor.Pid || pid == survivor.Pid) {
			signalled.Add(1)
		}
		return syscall.Kill(pid, sig)
	}
	t.Cleanup(func() { killProcessGroup = syscall.Kill })
	rt = NewAgentRuntime(rt.DB, tr, nil)
	rt.SetWorkers(2)
	rt.link.syncInterval = 1
	tr.resp.CommandSets = defsOf(testSet(1, PurposeRunOnly, "echo again"), testSet(2, PurposeRunOnly, "echo other"),
		testSet(3, PurposeRunOnly, "echo third"))
	tr.resp.CommandRequests = manualRequests(1, 1, 2, 3)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		rt.Run(stop)
	}()
	newRuns := func(set int64) int {
		var n int
		_ = rt.DB.Read().QueryRow(`SELECT count(*) FROM command_executions WHERE set_id = ? AND request_id > 0 AND finished_at > 0`, set).Scan(&n)
		return n
	}
	waitFor(t, "the sets of the void records", func() bool { return newRuns(2) == 1 && newRuns(3) == 1 })
	time.Sleep(2 * time.Second)
	if n := newRuns(1); n != 0 {
		t.Fatalf("the set of the surviving group started %d time(s) while the group exists", n)
	}
	if !processAlive(survivor.Pid) || signalled.Load() != 0 {
		t.Fatalf("the surviving group was signalled (alive %v, %d signals)", processAlive(survivor.Pid), signalled.Load())
	}
	// The process of the group ends and stays a zombie (the test reaps it
	// at its end; signal 0 to the group still answers): the set starts.
	if err := syscall.Kill(survivor.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the survivor to become a zombie", func() bool {
		st, ok := readProcStat(procDir, survivor.Pid)
		return ok && st.ended()
	})
	waitFor(t, "the set after the group's only process ended", func() bool { return newRuns(1) == 1 })
	if st, ok := readProcStat(procDir, survivor.Pid); !ok || !st.ended() || syscall.Kill(-survivor.Pid, 0) != nil {
		t.Fatalf("the set did not start with a zombie left in the group: %+v %v", st, ok)
	}
	close(stop)
	<-done
	interrupted := map[string]bool{}
	for _, r := range runReports(allReports(t, rt, tr)) {
		if r.Finished && r.Reason == ReasonInterrupted {
			interrupted[r.RunUID] = true
		}
	}
	if !interrupted["alive"] || !interrupted["other-boot"] || !interrupted["gone"] {
		t.Fatalf("runs found unfinished must be reported interrupted: %v", interrupted)
	}
}

// TestRunRecordsItsProcessGroup: while a command runs, its process group and
// the boot time are recorded with the run; after its report nothing is.
func TestRunRecordsItsProcessGroup(t *testing.T) {
	requireBash(t)
	rt := newCommandRuntime(t, &cmdTransport{})
	pidFile := filepath.Join(t.TempDir(), "pid")
	rt.storeCommandDefinitions(defsOf(testSet(1, PurposeRunOnly, "echo $$ > "+pidFile+"; sleep 1")))
	done := runInBackground(rt, &commandJob{def: rt.sched.valid[1], trigger: TriggerManual, requestID: 3})
	pid := pidFrom(t, pidFile)
	boot, _ := bootTime()
	var e *models.CommandExecution
	waitFor(t, "the recorded group", func() bool {
		e, _ = models.CommandExecutionByRequest(rt.DB.Read(), 3)
		return e != nil && e.RunningPgid != 0
	})
	if e.RunningPgid != pid || !sameBoot(e.RunningBoot, boot) {
		t.Fatalf("recorded group %d boot %d, want %d and about %d", e.RunningPgid, e.RunningBoot, pid, boot)
	}
	returned(t, done)
	if e, _ = models.CommandExecutionByRequest(rt.DB.Read(), 3); e.RunningPgid != 0 || e.RunningBoot != 0 {
		t.Fatalf("after the run: %+v", e)
	}
}
