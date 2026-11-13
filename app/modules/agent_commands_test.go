package modules

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
	reports  []*ReportRequest
	onSync   func(n int)
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

func (f *cmdTransport) Report(r *ReportRequest) error {
	f.attempts = append(f.attempts, time.Now())
	if f.failReports {
		return os.ErrDeadlineExceeded
	}
	f.reports = append(f.reports, r)
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
	// No monitoring step during unit runs (the flow tests reset this).
	rt.lastMonitor = time.Now()
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
	if len(rt.cmdRequests) != 2 || rt.cmdRequests[0].ID != 7 {
		t.Fatalf("queued requests: %+v", rt.cmdRequests)
	}
	for job := rt.nextCommandJob(); job != nil; job = rt.nextCommandJob() {
		rt.runCommandJob(job)
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

	// Delivered again (the central did not process the answer yet), even by
	// a restarted agent: the request is not run a second time.
	rt2 := NewAgentRuntime(rt.DB, tr, nil)
	if rt2.commandSignature() != tr.resp.CommandSets.Signature {
		t.Fatal("the stored definitions must be restored")
	}
	rt2.sync()
	if job := rt2.nextCommandJob(); job != nil {
		t.Fatalf("a request must not run twice: %+v", job)
	}
	var again []CommandRequestAck
	for _, a := range acksOf(queuedReports(t, rt2)) {
		if a.RequestID == 7 {
			again = append(again, a)
		}
	}
	if a := again[len(again)-1]; len(again) != 2 || a.Status != RequestAckStarted || a.RunUID != acks[7].RunUID {
		t.Fatalf("the repeated request must be answered with its run: %+v", again)
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
	if rt.cmdValid[1] == nil || rt.cmdValid[2] != nil {
		t.Fatalf("valid sets: %v", rt.cmdValid)
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
	tr := &cmdTransport{}
	rt := newCommandRuntime(t, tr)
	rt.storeCommandDefinitions(defsOf(testSet(1, PurposeRunOnly, "true", "sleep 1")))
	var seen []*models.CommandExecution
	tr.onSync = func(int) {
		runs, err := models.ListUnfinishedCommandExecutions(rt.DB.Read())
		if err != nil {
			t.Error(err)
		}
		seen = append(seen, runs...)
	}
	// The monitoring step becomes due while the second command runs.
	rt.lastMonitor = time.Now().Add(300*time.Millisecond - rt.Interval())
	start := time.Now().Unix()
	rt.runCommandJob(&commandJob{def: rt.cmdValid[1], trigger: TriggerManual, requestID: 5})
	if len(seen) != 1 || seen[0].RunningPosition != 1 || seen[0].RunningStartedAt < start {
		t.Fatalf("running command seen during the run: %+v", seen)
	}
	e, err := models.CommandExecutionByRequest(rt.DB.Read(), 5)
	if err != nil || e == nil || e.FinishedAt == 0 || e.RunningPosition != -1 || e.RunningStartedAt != 0 {
		t.Fatalf("after the run: %+v %v", e, err)
	}
}

func TestCommandSchedules(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Skip("no time zone data")
	}
	rt := newCommandRuntime(t, &cmdTransport{})
	rt.loc = loc
	now := time.Date(2026, 11, 12, 9, 0, 30, 0, loc)
	rt.clock = func() time.Time { return now }

	every := testSet(1, PurposeRunOnly, "true")
	every.Schedule = &CommandSchedule{Kind: ScheduleEveryMinutes, Every: 5}
	every.RunMode = RunModeSchedule
	daily := testSet(2, PurposeRunOnly, "true")
	daily.Schedule = &CommandSchedule{Kind: ScheduleDaily, Time: "09:02"}
	daily.RunMode = RunModeSchedule
	manual := testSet(3, PurposeRunOnly, "true")
	rt.storeCommandDefinitions(defsOf(every, daily, manual))

	// Right after the definitions arrived, past occurrences are not run.
	if job := rt.nextCommandJob(); job != nil {
		t.Fatalf("armed now, nothing is due: %+v", job)
	}
	if d := rt.nextWait(); d > 90*time.Second+time.Second {
		t.Fatalf("the wait must end at the next due time (09:02): %s", d)
	}
	// 09:21: the every-5-minutes set missed 09:05..09:20 and runs once
	// (09:20); the daily set is due at 09:02 and comes first.
	now = time.Date(2026, 11, 12, 9, 21, 0, 0, loc)
	job := rt.nextCommandJob()
	if job == nil || job.def.ID != 2 || job.scheduledFor != time.Date(2026, 11, 12, 9, 2, 0, 0, loc).Unix() {
		t.Fatalf("first due: %+v", job)
	}
	requireBash(t)
	rt.runCommandJob(job)
	job = rt.nextCommandJob()
	if job == nil || job.def.ID != 1 || job.scheduledFor != time.Date(2026, 11, 12, 9, 20, 0, 0, loc).Unix() {
		t.Fatalf("missed occurrences run once: %+v", job)
	}
	rt.runCommandJob(job)
	if job := rt.nextCommandJob(); job != nil {
		t.Fatalf("nothing more is due: %+v", job)
	}
	runs := runReports(queuedReports(t, rt))
	if runs[0].Trigger != TriggerSchedule || runs[0].ScheduledFor == 0 || runs[0].StartedAt == 0 {
		t.Fatalf("run of a schedule: %+v", runs[0])
	}
	// A manual request comes before due schedules.
	now = time.Date(2026, 11, 12, 9, 25, 0, 0, loc)
	rt.receiveCommandRequests([]CommandRunRequest{{ID: 1, SetID: 3, RequestedAt: now.Unix(), ExpiresAt: now.Unix() + 60}})
	if job := rt.nextCommandJob(); job == nil || job.trigger != TriggerManual {
		t.Fatalf("requests first: %+v", job)
	}
	if job := rt.nextCommandJob(); job == nil || job.def.ID != 1 {
		t.Fatalf("then the schedule: %+v", job)
	}
	// Unchanged definitions delivered again keep the schedule state; a
	// changed schedule is armed again.
	rt.storeCommandDefinitions(defsOf(every, daily, manual))
	if st, _ := models.GetCommandSchedule(rt.DB.Read(), 1); st.LastDue != time.Date(2026, 11, 12, 9, 20, 0, 0, loc).Unix() {
		t.Fatalf("schedule state: %+v", st)
	}
	every.Schedule.Every = 10
	rt.storeCommandDefinitions(defsOf(every, daily))
	if st, _ := models.GetCommandSchedule(rt.DB.Read(), 1); st.LastDue != 0 || st.ArmedAt != now.Unix() {
		t.Fatalf("re-armed: %+v", st)
	}
	if job := rt.nextCommandJob(); job != nil {
		t.Fatalf("a changed schedule does not run past occurrences: %+v", job)
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
	rt.defaultInterval = 1
	rt.lastMonitor = time.Time{}
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
	rt.lastMonitor = time.Time{}
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
	rt.lastMonitor = time.Time{}
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
	rt.Config = &AgentConfig{AgentID: "id", CentralHost: "192.0.2.1", CentralPort: 8443, Token: "old", TokenName: "a"}
	var saved []string
	rt.OnConfigSaved = func(c *AgentConfig) {
		if stored := models.GetLocalState(rt.DB.Read(), agentCfgToken); stored != c.Token {
			t.Errorf("called before the configuration was saved: stored token %q", stored)
		}
		saved = append(saved, c.Token)
	}
	tr.resp.TokenRenewal = &TokenRenewal{Token: "new", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	rt.Cycle()
	if len(saved) != 1 || saved[0] != "new" || rt.Config.Token != "new" {
		t.Fatalf("renewed token: %v, config %q", saved, rt.Config.Token)
	}
	rt.Cycle()
	if len(saved) != 1 {
		t.Fatalf("a sync without changes must not call OnConfigSaved: %v", saved)
	}
}
