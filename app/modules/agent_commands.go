package modules

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"mimamori/app/models"
)

// Command sets on the agent: storing the delivered definitions, running a set
// command by command and queueing its results in the outbox. The scheduler
// (agent_scheduler.go) holds the definitions and the run requests and decides
// when a set runs; each run belongs to the worker that runs it. While a
// command runs, its wait loop runs the monitoring step when the scheduler
// gives it to that worker.

// commandJob is a set waiting to run.
type commandJob struct {
	def          *CommandSetDef
	trigger      string
	requestID    int64
	scheduledFor int64
}

// commandExecutionKeep is how long the record of a finished run stays in the
// agent's database. It is longer than the expiry of a manual request, so a
// request delivered again is still recognized as executed.
const commandExecutionKeep = 2 * CommandRequestExpiry

// now returns the agent's current time (replaceable by tests).
func (rt *AgentRuntime) now() time.Time {
	if rt.clock != nil {
		return rt.clock()
	}
	return time.Now()
}

// location is the time zone of recurring schedules: the host's local time.
func (rt *AgentRuntime) location() *time.Location {
	if rt.loc != nil {
		return rt.loc
	}
	return time.Local
}

// loadCommandDefinitions restores the stored definitions at start.
func (rt *AgentRuntime) loadCommandDefinitions() {
	v := models.GetLocalState(rt.DB.Read(), StateCommandDefinitions)
	if v == "" {
		return
	}
	var defs CommandDefinitions
	if err := json.Unmarshal([]byte(v), &defs); err != nil {
		rt.logf("the stored command set definitions cannot be read (they are delivered again): %v", err)
		return
	}
	rt.acceptCommandDefinitions(&defs)
}

// acceptCommandDefinitions takes definitions into use: the valid sets can run
// (a set that fails validation on the agent is not run and is reported), and
// the schedule state of each scheduled set is armed when it is new or
// changed. The schedule states are armed and the old ones removed in one
// transaction (arming reads the stored state to decide) before the
// scheduler gets the definitions.
func (rt *AgentRuntime) acceptCommandDefinitions(defs *CommandDefinitions) []CommandApplyProblem {
	problems := []CommandApplyProblem{}
	valid := map[int64]*CommandSetDef{}
	var scheduled []*CommandSetDef
	now := rt.now().Unix()
	for i := range defs.Sets {
		d := defs.Sets[i]
		if ps := d.Problems(0); len(ps) > 0 {
			problems = append(problems, CommandApplyProblem{SetID: d.ID, SetName: d.Name, Problems: ProblemTexts(ps), ProblemsI18n: ps})
			continue
		}
		valid[d.ID] = &d
		if d.RunMode == RunModeSchedule && d.Schedule != nil {
			scheduled = append(scheduled, &d)
		}
	}
	var failedSet *CommandSetDef // the set whose schedule state could not be stored
	err := rt.DB.Write(func(tx models.Writer) error {
		ids := make([]int64, 0, len(scheduled))
		for _, d := range scheduled {
			ids = append(ids, d.ID)
			if _, err := models.ArmCommandSchedule(tx, d.ID, ScheduleSignature(d), now); err != nil {
				failedSet = d
				return err
			}
		}
		return models.DeleteCommandSchedulesExcept(tx, ids)
	})
	switch {
	case err != nil && failedSet != nil:
		rt.logf("command set %q: storing the schedule state failed: %v", failedSet.Name, err)
	case err != nil:
		rt.logf("removing old schedule states failed: %v", err)
	}
	rt.sched.acceptDefinitions(defs, valid)
	return problems
}

// storeCommandDefinitions stores definitions delivered by the central (also
// while a set runs; the running set keeps the definition it started with)
// and queues the outcome for the central.
func (rt *AgentRuntime) storeCommandDefinitions(defs *CommandDefinitions) {
	if defs.Sets == nil {
		defs.Sets = []CommandSetDef{}
	}
	b, err := json.Marshal(defs)
	if err != nil {
		rt.logf("command set definitions not stored: %v", err)
		return
	}
	if err := rt.DB.Write(func(tx models.Writer) error {
		return models.SetLocalState(tx, StateCommandDefinitions, string(b))
	}); err != nil {
		// Not taken into use; the central delivers them again.
		rt.logf("command set definitions not stored: %v", err)
		return
	}
	problems := rt.acceptCommandDefinitions(defs)
	for _, p := range problems {
		rt.logf("command set %q is not run: %s", p.SetName, M("common.problems", "problems", firstProblems(p.ProblemsI18n, 5)).Text())
	}
	rt.logf("command set definitions received: %d set(s)", len(defs.Sets))
	rt.queueCommandReport(&ReportRequest{CommandApply: &CommandApplyResult{
		Signature: defs.Signature, At: rt.now().Unix(), Problems: problems,
	}})
}

// queueAcks queues answers to manual requests (the scheduler has entered
// them as answered).
func (rt *AgentRuntime) queueAcks(acks []CommandRequestAck) {
	if len(acks) == 0 {
		return
	}
	rt.queueCommandReport(&ReportRequest{CommandRequestAcks: acks})
}

// runCommandJob runs a set to its end with the definition of the job, on the
// calling worker. It returns false when the run could not be started.
//
// The records of the run are written so that none is lost (the commands run
// between them): the start (the schedule mark of a scheduled set, the
// execution row and the start report, which answers a manual request) in
// one transaction; before each command the command that runs; after it the
// command's report with the end of that command and, for the last one, the
// end of the run, in one transaction. A record that cannot be written is
// written again (storeRunRecord) until it is stored or the stop comes, so
// the run is not over for its worker until its end is stored. When the stop
// came first, the execution stays unfinished and the next start reports the
// run as interrupted.
func (rt *AgentRuntime) runCommandJob(job *commandJob) bool {
	def := job.def
	now := rt.now().Unix()
	uid, err := RandomHex(16)
	if err != nil {
		rt.logf("command set %q not run: %v", def.Name, err)
		return false
	}
	exec := &models.CommandExecution{RunUID: uid, SetID: def.ID, SetRevision: def.Revision, RequestID: job.requestID,
		Trigger: job.trigger, ScheduledFor: job.scheduledFor, StartedAt: now}
	base := CommandRunReport{
		RunUID: uid, SetID: def.ID, SetRevision: def.Revision, SetName: def.Name, Purpose: def.Purpose, Joiner: def.Joiner,
		OSUser: def.OSUser, WorkDir: def.WorkDir, Trigger: job.trigger, RequestID: job.requestID, ScheduledFor: job.scheduledFor,
		CommandsTotal: len(def.Commands), StartedAt: now, Results: []CommandResultReport{},
	}
	// The schedule mark is written with the run, so that an occurrence is
	// never run twice and never lost (a run cut short by a crash is reported
	// as interrupted).
	err = rt.storeRunRecord(fmt.Sprintf("command set %q: the start of run %s", def.Name, uid), func() error {
		start := &ReportRequest{CommandRuns: []CommandRunReport{base}}
		if job.requestID > 0 {
			start.CommandRequestAcks = []CommandRequestAck{{RequestID: job.requestID, Status: RequestAckStarted, RunUID: uid, At: now}}
		}
		return rt.out.queueWith(rt.stamped(start), func(tx models.Writer) error {
			if job.trigger == TriggerSchedule {
				if err := models.SetCommandScheduleLastDue(tx, def.ID, job.scheduledFor); err != nil {
					return err
				}
			}
			return models.InsertCommandExecution(tx, exec)
		})
	})
	switch {
	case errors.Is(err, models.ErrAlreadyExecuted):
		// Only for the answer: the run that was recorded first.
		if e, err := models.CommandExecutionByRequest(rt.DB.Read(), job.requestID); err == nil && e != nil {
			rt.queueAcks([]CommandRequestAck{{RequestID: job.requestID, Status: RequestAckStarted, RunUID: e.RunUID, At: now}})
		}
		return true
	case err != nil:
		rt.logf("command set %q not run: recording its start failed: %v", def.Name, err)
		return false
	}
	rt.logf("command set %q started (%s, run %s)", def.Name, job.trigger, uid)

	hooks := CommandExecHooks{MonitorHere: rt.monitorHere, Stop: rt.stop, Logf: rt.logf}
	collect := def.Purpose == PurposeCollectOutput
	var kinds []string
	var values []CommandValues
	finish := func(rep *CommandRunReport, reason string) {
		rep.Finished = true
		rep.FinishedAt = rt.now().Unix()
		rep.State = RunState(kinds)
		rep.Reason = reason
		if reason != "" {
			rep.State = WorseResult(rep.State, ResultError)
		}
		if collect {
			dv := BuildDisplayValue(values, def.Joiner)
			rep.Display = &dv
		}
	}
	// storeReport writes rep with the end of the running command and, when
	// rep closes the run, the end of the run, in one transaction.
	storeReport := func(what string, rep CommandRunReport) error {
		return rt.storeRunRecord(what, func() error {
			return rt.out.queueWith(rt.stamped(&ReportRequest{CommandRuns: []CommandRunReport{rep}}), func(tx models.Writer) error {
				if err := models.ClearCommandExecutionRunning(tx, uid); err != nil {
					return err
				}
				if rep.Finished {
					return models.FinishCommandExecution(tx, uid, rep.FinishedAt)
				}
				return nil
			})
		})
	}
	// unstored are the results whose report could not be written before the
	// stop; the report that closes the run carries them.
	var unstored []CommandResultReport
	closed := false
	for i := range def.Commands {
		if i > 0 && isClosed(rt.stop) {
			break
		}
		c := &def.Commands[i]
		// Recorded while the command runs, so that a run cut short by the end
		// of the agent reports this command as interrupted at the next start.
		startedAt := time.Now().Unix()
		if err := rt.storeRunRecord(fmt.Sprintf("command set %q: command %d of run %s", def.Name, i+1, uid), func() error {
			return rt.DB.Write(func(tx models.Writer) error { return models.SetCommandExecutionRunning(tx, uid, i, startedAt) })
		}); err != nil {
			rt.logf("command set %q: recording the start of command %d of run %s failed, it is not run: %v", def.Name, i+1, uid, err)
			break
		}
		res := ExecuteCommand(CommandExecInput{Position: i, OSUser: def.OSUser, WorkDir: def.WorkDir, Command: c,
			CollectOutput: collect, KillGrace: rt.cmdKillGrace}, hooks)
		kinds = append(kinds, res.Kind)
		if res.Values != nil {
			values = append(values, *res.Values)
		}
		interrupted := res.Reason == ReasonInterrupted
		last := i == len(def.Commands)-1 || !c.ContinuesAfter(res.Kind) || interrupted
		rep := base
		rep.Results = append(append([]CommandResultReport{}, unstored...), *res)
		if last {
			// A command the stop ended cut the run: the run's reason is
			// interrupted too (the time limit and a failed start are
			// reasons of the command only).
			reason := ""
			if interrupted {
				reason = ReasonInterrupted
			}
			finish(&rep, reason)
			closed = true
		}
		if err := storeReport(fmt.Sprintf("command set %q: the result of command %d of run %s", def.Name, i+1, uid), rep); err != nil {
			if last {
				rt.logf("command set %q: recording the end of run %s failed (the next start reports it as interrupted): %v", def.Name, uid, err)
			} else {
				rt.logf("command set %q: recording the result of command %d of run %s failed: %v", def.Name, i+1, uid, err)
				unstored = rep.Results
			}
		} else {
			unstored = nil
		}
		if last {
			break
		}
	}
	if !closed {
		// The agent stopped between two commands.
		rep := base
		rep.Results = append([]CommandResultReport{}, unstored...)
		reason := ""
		if len(def.Commands) > 0 {
			reason = ReasonInterrupted
		}
		finish(&rep, reason)
		if err := storeReport(fmt.Sprintf("command set %q: the end of run %s", def.Name, uid), rep); err != nil {
			rt.logf("command set %q: recording the end of run %s failed (the next start reports it as interrupted): %v", def.Name, uid, err)
		}
	}
	rt.logf("command set %q finished: %s", def.Name, RunState(kinds))
	return true
}

// storeRunRecord writes one record of a run (write is one transaction) and,
// when that fails, writes it again after RunRecordRetryFirst, doubling the
// wait up to RunRecordRetryMax, until it is stored or the stop comes; after
// the stop it makes one more attempt. A record whose row is gone
// (sql.ErrNoRows) or whose request already ran (models.ErrAlreadyExecuted)
// is not written again. It returns nil when the record was stored, else the
// last error. what names the record in the log line of a repeated attempt.
func (rt *AgentRuntime) storeRunRecord(what string, write func() error) error {
	wait, most := RunRecordRetryFirst, RunRecordRetryMax
	if rt.recordRetry > 0 {
		wait, most = rt.recordRetry, rt.recordRetry*(RunRecordRetryMax/RunRecordRetryFirst)
	}
	for {
		err := write()
		if err == nil || errors.Is(err, sql.ErrNoRows) || errors.Is(err, models.ErrAlreadyExecuted) || isClosed(rt.stop) {
			return err
		}
		rt.logf("%s: storing it failed, tried again in %s: %v", what, wait, err)
		t := time.NewTimer(wait)
		select {
		case <-rt.stop:
		case <-t.C:
		}
		t.Stop()
		wait = min(2*wait, most)
	}
}

// recoverInterruptedRuns reports the runs found unfinished at start (the
// agent ended while they ran) as interrupted. The command that was running
// gets an interrupted result (the central keeps a result already reported
// for its position). The report of a run is queued and the run is recorded
// as finished in one transaction of the outbox, so that a run is never
// reported twice nor finished without its report.
func (rt *AgentRuntime) recoverInterruptedRuns() {
	runs, err := models.ListUnfinishedCommandExecutions(rt.DB.Read())
	if err != nil {
		rt.logf("reading unfinished command set runs failed: %v", err)
		return
	}
	now := rt.now().Unix()
	for _, e := range runs {
		rep := CommandRunReport{
			RunUID: e.RunUID, SetID: e.SetID, SetRevision: e.SetRevision, Trigger: e.Trigger, RequestID: e.RequestID,
			ScheduledFor: e.ScheduledFor, StartedAt: e.StartedAt, Finished: true, FinishedAt: now,
			State: ResultError, Reason: ReasonInterrupted, Results: []CommandResultReport{},
		}
		d := rt.sched.findSet(e.SetID)
		if d != nil {
			rep.SetName, rep.Purpose, rep.Joiner, rep.OSUser, rep.WorkDir = d.Name, d.Purpose, d.Joiner, d.OSUser, d.WorkDir
			rep.CommandsTotal = len(d.Commands)
		}
		if e.RunningPosition >= 0 {
			res := CommandResultReport{Position: e.RunningPosition, Kind: ResultError, Reason: ReasonInterrupted,
				StartedAt: e.RunningStartedAt, FinishedAt: now, Output: []byte{}}
			// The command line and note only when the stored definition is
			// the one the run executed.
			if d != nil && d.Revision == e.SetRevision && e.RunningPosition < len(d.Commands) {
				res.CommandLine, res.Note = d.Commands[e.RunningPosition].CommandLine, d.Commands[e.RunningPosition].Note
			}
			rep.Results = append(rep.Results, res)
		}
		finishing := false
		err := rt.out.queueWith(rt.stamped(&ReportRequest{CommandRuns: []CommandRunReport{rep}}), func(tx models.Writer) error {
			finishing = true
			return models.FinishCommandExecution(tx, e.RunUID, now)
		})
		switch {
		case err != nil && finishing:
			rt.logf("recording the end of run %s failed: %v", e.RunUID, err)
		case err != nil:
			rt.logf("failed to queue a command set report: %v", err)
		}
		rt.logf("command set run %s was interrupted (the agent ended while it ran)", e.RunUID)
	}
}

// queueCommandReport queues a report about command sets in the outbox; it is
// sent with the next report.
func (rt *AgentRuntime) queueCommandReport(req *ReportRequest) {
	if err := rt.out.queue(rt.stamped(req)); err != nil {
		rt.logf("failed to queue a command set report: %v", err)
	}
}

// stamped sets the creation time of a report about command sets to the
// agent's current time and returns it.
func (rt *AgentRuntime) stamped(req *ReportRequest) *ReportRequest {
	req.CreatedAt = rt.now().Unix()
	return req
}

// pruneCommandExecutions removes old records of finished runs.
func (rt *AgentRuntime) pruneCommandExecutions() {
	cut := rt.now().Add(-commandExecutionKeep).Unix()
	_ = rt.DB.Write(func(tx models.Writer) error {
		_, err := models.DeleteFinishedCommandExecutionsBefore(tx, cut)
		return err
	})
}
