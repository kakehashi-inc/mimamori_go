package modules

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"time"

	"mimamori/app/models"
)

// Command sets on the agent: storing the delivered definitions, running a set
// command by command and queueing its results in the outbox. The queue
// (agent_scheduler.go) holds the definitions and the entries and decides
// when a set runs; each run belongs to the worker that processes it, which
// does nothing else meanwhile.

// commandJob is a run of a set to process.
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

// checkDefinitions validates delivered definitions: it returns the sets the
// agent refuses (they are not run and are reported), the valid sets and the
// valid scheduled sets.
func checkDefinitions(defs *CommandDefinitions) ([]CommandApplyProblem, map[int64]*CommandSetDef, []*CommandSetDef) {
	problems := []CommandApplyProblem{}
	valid := map[int64]*CommandSetDef{}
	var scheduled []*CommandSetDef
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
	return problems, valid, scheduled
}

// armSchedules stores the schedule state of each scheduled set when it is
// new or changed and removes the states of the other sets, inside tx.
func armSchedules(tx models.Writer, scheduled []*CommandSetDef, now int64) error {
	ids := make([]int64, 0, len(scheduled))
	for _, d := range scheduled {
		ids = append(ids, d.ID)
		if _, err := models.ArmCommandSchedule(tx, d.ID, ScheduleSignature(d), now); err != nil {
			return fmt.Errorf("command set %q: storing the schedule state failed: %w", d.Name, err)
		}
	}
	return models.DeleteCommandSchedulesExcept(tx, ids)
}

// loadCommandDefinitions restores the stored definitions at start: the
// schedule states are armed (in one transaction) and the queue gets them.
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
	_, valid, scheduled := checkDefinitions(&defs)
	if err := rt.DB.Write(func(tx models.Writer) error { return armSchedules(tx, scheduled, rt.now().Unix()) }); err != nil {
		rt.logf("the stored command set definitions: storing their schedule states failed (the states stored before are used): %v", err)
	}
	rt.sched.acceptDefinitions(&defs, valid)
}

// storeCommandDefinitions stores definitions delivered by the central (also
// while a set runs; the running set keeps the definition it started with)
// and reports whether they were stored. The definitions, the schedule states
// of the scheduled sets (armed when new or changed) and the outcome for the
// central (the sets the agent refuses) are written in one transaction; then
// the queue takes them into use. When that fails nothing is stored, and the
// central delivers them again.
func (rt *AgentRuntime) storeCommandDefinitions(defs *CommandDefinitions) bool {
	if defs.Sets == nil {
		defs.Sets = []CommandSetDef{}
	}
	b, err := json.Marshal(defs)
	if err != nil {
		rt.logf("command set definitions not stored: %v", err)
		return false
	}
	problems, valid, scheduled := checkDefinitions(defs)
	now := rt.now().Unix()
	outcome := &ReportRequest{CommandApply: &CommandApplyResult{Signature: defs.Signature, At: now, Problems: problems}}
	if err := rt.out.queueWith(rt.stamped(outcome), func(tx models.Writer) error {
		if err := models.SetLocalState(tx, StateCommandDefinitions, string(b)); err != nil {
			return err
		}
		return armSchedules(tx, scheduled, now)
	}); err != nil {
		rt.logf("command set definitions not stored (the central delivers them again): %v", err)
		return false
	}
	for _, p := range problems {
		rt.logf("command set %q is not run: %s", p.SetName, M("common.problems", "problems", firstProblems(p.ProblemsI18n, 5)).Text())
	}
	rt.logf("command set definitions received: %d set(s)", len(defs.Sets))
	rt.sched.acceptDefinitions(defs, valid)
	return true
}

// queueAcks queues answers to manual requests.
func (rt *AgentRuntime) queueAcks(acks []CommandRequestAck) {
	if len(acks) == 0 {
		return
	}
	rt.queueCommandReport(&ReportRequest{CommandRequestAcks: acks})
}

// commandRun is the state of one run while its worker processes it, kept so
// that a run can be closed after a panic (closeAfterPanic).
type commandRun struct {
	job     *commandJob
	uid     string
	base    CommandRunReport
	collect bool
	kinds   []string
	values  []CommandValues
	// unstored are the results whose report could not be written before the
	// stop; the report that closes the run carries them.
	unstored []CommandResultReport
	// started: the start of the run is stored; closed: the report that
	// closes it is stored (or the run is left unfinished on purpose).
	started, closed bool
	// cur is the command being processed (nil between commands) and res its
	// result once it is known.
	cur *commandProcess
	res *CommandResultReport
}

// finish sets the closing values of rep: the state from the kinds of the
// executed commands (error when reason is set), the reason and, for
// collect_output, the display value.
func (r *commandRun) finish(rt *AgentRuntime, rep *CommandRunReport, reason string) {
	rep.Finished = true
	rep.FinishedAt = rt.now().Unix()
	rep.State = RunState(r.kinds)
	rep.Reason = reason
	if reason != "" {
		rep.State = WorseResult(rep.State, ResultError)
	}
	if r.collect {
		dv := BuildDisplayValue(r.values, r.job.def.Joiner)
		rep.Display = &dv
	}
}

// storeReport writes rep with the end of the running command and, when rep
// closes the run, the end of the run, in one transaction (written again until
// stored, see storeRunRecord).
func (rt *AgentRuntime) storeRunReport(r *commandRun, what string, rep CommandRunReport) error {
	return rt.storeRunRecord(what, func() error {
		return rt.out.queueWith(rt.stamped(&ReportRequest{CommandRuns: []CommandRunReport{rep}}), func(tx models.Writer) error {
			if err := models.ClearCommandExecutionRunning(tx, r.uid); err != nil {
				return err
			}
			if rep.Finished {
				return models.FinishCommandExecution(tx, r.uid, rep.FinishedAt)
			}
			return nil
		})
	})
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
//
// Nothing starts after the stop: when it came before the start of the run
// is recorded, nothing is recorded (a manual request is answered at its next
// delivery, the occurrence of a scheduled set stays not executed); when it
// came before a command, that command does not start and the run is closed
// as interrupted. A starting record is not written after the stop
// (storeRunStart).
//
// A panic while the run is processed is logged and the run is closed with
// what it has (closeAfterPanic); its command is not signalled for it.
func (rt *AgentRuntime) runCommandJob(job *commandJob) (started bool) {
	run := &commandRun{job: job, collect: job.def.Purpose == PurposeCollectOutput}
	defer func() {
		if v := recover(); v != nil {
			rt.logf("worker panic: %v\n%s", v, debug.Stack())
			rt.closeAfterPanic(run)
			started = run.started
		}
	}()
	return rt.executeRun(run)
}

// executeRun is runCommandJob without the recovery of a panic.
func (rt *AgentRuntime) executeRun(run *commandRun) bool {
	job := run.job
	def := job.def
	if isClosed(rt.stop) {
		rt.logf("command set %q not started: the agent stops", def.Name)
		return false
	}
	now := rt.now().Unix()
	uid, err := RandomHex(16)
	if err != nil {
		rt.logf("command set %q not run: %v", def.Name, err)
		return false
	}
	run.uid = uid
	exec := &models.CommandExecution{RunUID: uid, SetID: def.ID, SetRevision: def.Revision, RequestID: job.requestID,
		Trigger: job.trigger, ScheduledFor: job.scheduledFor, StartedAt: now}
	run.base = CommandRunReport{
		RunUID: uid, SetID: def.ID, SetRevision: def.Revision, SetName: def.Name, Purpose: def.Purpose, Joiner: def.Joiner,
		OSUser: def.OSUser, WorkDir: def.WorkDir, Trigger: job.trigger, RequestID: job.requestID, ScheduledFor: job.scheduledFor,
		CommandsTotal: len(def.Commands), StartedAt: now, Results: []CommandResultReport{},
	}
	// The schedule mark is written with the run, so that an occurrence is
	// never run twice and never lost (a run cut short by a crash is reported
	// as interrupted). The occurrences skipped before this one are counted
	// from the schedule state read in the same transaction.
	err = rt.storeRunStart(fmt.Sprintf("command set %q: the start of run %s", def.Name, uid), func() error {
		return rt.out.queueBuilt(func(tx models.Writer) (*ReportRequest, error) {
			base := run.base
			if job.trigger == TriggerSchedule {
				st, err := models.GetCommandSchedule(tx, def.ID)
				if err != nil {
					return nil, err
				}
				if st != nil && def.Schedule != nil {
					n, first, last := def.Schedule.Skipped(time.Unix(job.scheduledFor, 0), time.Unix(st.ArmedAt, 0),
						time.Unix(st.LastDue, 0), rt.location())
					if n > 0 {
						base.SkippedCount, base.SkippedFrom, base.SkippedTo = n, first.Unix(), last.Unix()
					}
				}
				if err := models.SetCommandScheduleLastDue(tx, def.ID, job.scheduledFor); err != nil {
					return nil, err
				}
			}
			if err := models.InsertCommandExecution(tx, exec); err != nil {
				return nil, err
			}
			run.base = base
			start := &ReportRequest{CommandRuns: []CommandRunReport{base}}
			if job.requestID > 0 {
				start.CommandRequestAcks = []CommandRequestAck{{RequestID: job.requestID, Status: RequestAckStarted, RunUID: uid, At: now}}
			}
			return rt.stamped(start), nil
		})
	})
	var stopped *stoppedBeforeRecord
	switch {
	case errors.Is(err, models.ErrAlreadyExecuted):
		// Only for the answer: the run that was recorded first.
		if e, err := models.CommandExecutionByRequest(rt.DB.Read(), job.requestID); err == nil && e != nil {
			rt.queueAcks([]CommandRequestAck{{RequestID: job.requestID, Status: RequestAckStarted, RunUID: e.RunUID, At: now}})
		}
		return false
	case errors.As(err, &stopped):
		rt.logf("command set %q not started: the agent stops%s", def.Name, stopped.cause("recording its start failed"))
		return false
	case err != nil:
		rt.logf("command set %q not run: recording its start failed: %v", def.Name, err)
		return false
	}
	run.started = true
	rt.logf("command set %q started (%s, run %s)", def.Name, job.trigger, uid)

	for i := range def.Commands {
		// No command starts after the stop: the run is closed as
		// interrupted below.
		if isClosed(rt.stop) {
			break
		}
		c := &def.Commands[i]
		run.cur = startCommand(CommandExecInput{Position: i, OSUser: def.OSUser, WorkDir: def.WorkDir, Command: c,
			CollectOutput: run.collect, KillGrace: rt.cmdKillGrace, Stop: rt.stop, Logf: rt.logf, Leftover: rt.leftovers.add})
		// The command is known to the run (run.cur) before anything else is
		// done, so that a panic from here on waits for it, or gives it up
		// when it was not released (closeAfterPanic).
		if !rt.recordCommandStart(run, i) {
			run.cur.cancel()
			run.cur = nil
			break
		}
		run.cur.release()
		run.cur.wait()
		res := run.cur.result(run.collect)
		run.res = res
		run.kinds = append(run.kinds, res.Kind)
		if res.Values != nil {
			run.values = append(run.values, *res.Values)
		}
		interrupted := res.Reason == ReasonInterrupted
		last := i == len(def.Commands)-1 || !c.ContinuesAfter(res.Kind) || interrupted
		rep := run.base
		rep.Results = append(append([]CommandResultReport{}, run.unstored...), *res)
		if last {
			// A command the stop ended cut the run: the run's reason is
			// interrupted too (the time limit and a failed start are
			// reasons of the command only).
			reason := ""
			if interrupted {
				reason = ReasonInterrupted
			}
			run.finish(rt, &rep, reason)
		}
		if err := rt.storeRunReport(run, fmt.Sprintf("command set %q: the result of command %d of run %s", def.Name, i+1, uid), rep); err != nil {
			if last {
				rt.logf("command set %q: recording the end of run %s failed (the next start reports it as interrupted): %v", def.Name, uid, err)
			} else {
				rt.logf("command set %q: recording the result of command %d of run %s failed: %v", def.Name, i+1, uid, err)
				run.unstored = rep.Results
			}
		} else {
			run.unstored = nil
			if last {
				rt.logFinished(def, &rep)
			}
		}
		run.cur, run.res = nil, nil
		if last {
			run.closed = true
			break
		}
	}
	if !run.closed {
		// The agent stopped before a command started.
		rep := run.base
		rep.Results = append([]CommandResultReport{}, run.unstored...)
		reason := ""
		if len(def.Commands) > 0 {
			reason = ReasonInterrupted
		}
		run.finish(rt, &rep, reason)
		if err := rt.storeRunReport(run, fmt.Sprintf("command set %q: the end of run %s", def.Name, uid), rep); err != nil {
			rt.logf("command set %q: recording the end of run %s failed (the next start reports it as interrupted): %v", def.Name, uid, err)
		} else {
			rt.logFinished(def, &rep)
		}
		run.closed = true
	}
	return true
}

// logFinished logs the end of a run whose end was stored: its state, and
// its reason when it has one.
func (rt *AgentRuntime) logFinished(def *CommandSetDef, rep *CommandRunReport) {
	if rep.Reason != "" {
		rt.logf("command set %q finished: %s (%s)", def.Name, rep.State, rep.Reason)
		return
	}
	rt.logf("command set %q finished: %s", def.Name, rep.State)
}

// recordCommandStart records the command at position pos of the run that
// starts (run.cur, waiting at its gate) with its process group and the boot
// of the host (hostBoot), in one transaction written again until it is
// stored or the stop comes (storeRunStart): a run cut short by the end of
// the agent then reports this command as interrupted at the next start, and
// its set does not start again while the group exists (DESIGN 1.7). It
// reports whether the command may be released: not when the record was not
// stored or the stop came. A command whose start failed has no group; it is
// not recorded and its result is reported.
func (rt *AgentRuntime) recordCommandStart(run *commandRun, pos int) bool {
	def := run.job.def
	pgid := run.cur.pgid()
	if pgid == 0 {
		return true
	}
	boot, err := currentBoot()
	if err != nil {
		rt.logf("command set %q: command %d of run %s: %v; its process group is recorded without it", def.Name, pos+1, run.uid, err)
	}
	err = rt.storeRunStart(fmt.Sprintf("command set %q: the start of command %d of run %s", def.Name, pos+1, run.uid), func() error {
		return rt.DB.Write(func(tx models.Writer) error {
			// The start time is that of the attempt that stores it: the
			// command is released right after.
			return models.SetCommandExecutionRunning(tx, run.uid, pos, time.Now().Unix(), pgid, boot.String())
		})
	})
	var stopped *stoppedBeforeRecord
	switch {
	case errors.As(err, &stopped):
		rt.logf("command set %q: command %d of run %s not started: the agent stops%s", def.Name, pos+1, run.uid,
			stopped.cause("recording its start failed"))
		return false
	case err != nil:
		rt.logf("command set %q: recording the start of command %d of run %s failed, it is not run: %v", def.Name, pos+1, run.uid, err)
		return false
	case isClosed(rt.stop):
		rt.logf("command set %q: command %d of run %s not started: the agent stops", def.Name, pos+1, run.uid)
		return false
	}
	return true
}

// closeAfterPanic closes a run whose processing panicked. The command that
// was being processed is waited for in the plain way when its main process
// still runs or its output is still read (the time limit and the stop apply
// as always; nothing is sent to it for the panic). That command and the run
// get the state error with the reason internal_error; the run is closed with
// the results it has. When the run cannot be closed this way (another
// panic, or its end cannot be stored before the stop), its execution stays
// unfinished and the next start reports it as interrupted; a command whose
// wait panicked again is let go (leaveCommand).
func (rt *AgentRuntime) closeAfterPanic(run *commandRun) {
	if !run.started || run.closed {
		return
	}
	def := run.job.def
	var hit *CommandResultReport
	switch {
	case run.res != nil:
		r := *run.res
		hit = &r
	case run.cur != nil:
		if rt.recovered("worker panic", func() {
			// A command that was not released is given up: it never ran.
			run.cur.wait()
			if run.cur.ran() {
				hit = run.cur.result(false)
			}
		}) {
			rt.leaveCommand(run)
			rt.logf("command set %q: run %s is left unfinished (the next start reports it as interrupted)", def.Name, run.uid)
			return
		}
	}
	rep := run.base
	rep.Results = append([]CommandResultReport{}, run.unstored...)
	if hit != nil {
		hit.Kind, hit.Reason = ResultError, ReasonInternalError
		rep.Results = append(rep.Results, *hit)
		run.kinds = append(run.kinds, hit.Kind)
	}
	if rt.recovered("worker panic", func() {
		run.finish(rt, &rep, ReasonInternalError)
		if err := rt.storeRunReport(run, fmt.Sprintf("command set %q: the end of run %s", def.Name, run.uid), rep); err != nil {
			rt.logf("command set %q: recording the end of run %s failed (the next start reports it as interrupted): %v", def.Name, run.uid, err)
			return
		}
		rt.logFinished(def, &rep)
	}) {
		rt.logf("command set %q: run %s is left unfinished (the next start reports it as interrupted)", def.Name, run.uid)
	}
	run.closed = true
}

// leaveCommand lets go of the command of a run whose wait could not be
// completed after a panic (the wait panicked again). Nothing is sent to it.
// Its set does not start again while its process group has a process that
// has not ended (as at the next start of the agent, recoverInterruptedRuns),
// and its output pipe, when its reading had not ended, goes to the held
// pipes, whose reader drops what arrives (the command never blocks on its
// output).
func (rt *AgentRuntime) leaveCommand(run *commandRun) {
	p := run.cur
	if p.gate != nil {
		// Not released: the gate ends without running anything.
		_ = p.gate.Close()
		p.gate = nil
	}
	if pgid := p.pgid(); pgid > 0 {
		rt.sched.hold(run.job.def.ID, pgid)
		rt.logf("command set %q: its command may still run as process group %d; the set does not start until that group is gone",
			run.job.def.Name, pgid)
	}
	if p.r != nil && !p.done {
		p.done = true
		_ = p.r.SetReadDeadline(time.Time{})
		rt.leftovers.add(p.r)
	}
}

// storeRunRecord writes a record that ends something of a run (the result
// of a command, the end of the run; write is one transaction) and, when that
// fails, writes it again after RunRecordRetryFirst, doubling the wait up to
// RunRecordRetryMax, until it is stored or the stop comes; after the stop it
// makes one more attempt. A record whose row is gone (sql.ErrNoRows) or
// whose request already ran (models.ErrAlreadyExecuted) is not written
// again. It returns nil when the record was stored, else the last error.
// what names the record in the log line of a repeated attempt.
func (rt *AgentRuntime) storeRunRecord(what string, write func() error) error {
	return rt.writeRunRecord(what, true, write)
}

// storeRunStart writes a record that starts something (the start of a run,
// the command that starts) as storeRunRecord does, except that nothing is
// written once the stop came, also not before the first attempt: it then
// returns a *stoppedBeforeRecord.
func (rt *AgentRuntime) storeRunStart(what string, write func() error) error {
	return rt.writeRunRecord(what, false, write)
}

// stoppedBeforeRecord is the error of a starting record that was not stored
// because the stop came; last is the failure of the last attempt (nil when
// none was made).
type stoppedBeforeRecord struct{ last error }

func (e *stoppedBeforeRecord) Error() string { return "the agent stops" + e.cause("storing it failed") }

// cause returns " (<what>: <last failure>)", or "" when no attempt failed.
func (e *stoppedBeforeRecord) cause(what string) string {
	if e.last == nil {
		return ""
	}
	return fmt.Sprintf(" (%s: %v)", what, e.last)
}

// writeRunRecord is storeRunRecord (afterStop: one more attempt after the
// stop) and storeRunStart (no attempt after the stop).
func (rt *AgentRuntime) writeRunRecord(what string, afterStop bool, write func() error) error {
	wait, most := RunRecordRetryFirst, RunRecordRetryMax
	if rt.recordRetry > 0 {
		wait, most = rt.recordRetry, rt.recordRetry*(RunRecordRetryMax/RunRecordRetryFirst)
	}
	var last error
	for {
		if !afterStop && isClosed(rt.stop) {
			return &stoppedBeforeRecord{last: last}
		}
		err := write()
		if err == nil || errors.Is(err, sql.ErrNoRows) || errors.Is(err, models.ErrAlreadyExecuted) {
			return err
		}
		last = err
		if isClosed(rt.stop) {
			if afterStop {
				return err
			}
			return &stoppedBeforeRecord{last: last}
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

// interruptedRun is a run found unfinished at the start and its report as
// interrupted.
type interruptedRun struct {
	uid   string
	setID int64
	at    int64
	rep   CommandRunReport
}

// recoverInterruptedRuns reports the runs found unfinished at start (the
// agent ended while they ran) as interrupted. The command that was running
// gets an interrupted result (the central keeps a result already reported
// for its position). The report of a run is queued and the run is recorded
// as finished in one transaction of the outbox, so that a run is never
// reported twice nor finished without its report. A record that cannot be
// stored is returned: recordInterrupted writes it again, and its set does
// not start before it is stored. When the process group recorded for the
// running command still exists in the same boot of the host, the set
// counts as running: none of its entries starts until the group is gone.
// The group is never signalled.
func (rt *AgentRuntime) recoverInterruptedRuns() []*interruptedRun {
	runs, err := models.ListUnfinishedCommandExecutions(rt.DB.Read())
	if err != nil {
		rt.logf("reading unfinished command set runs failed: %v", err)
		return nil
	}
	now := rt.now().Unix()
	boot, bootErr := currentBoot()
	var pending []*interruptedRun
	for _, e := range runs {
		if e.RunningPgid > 0 && bootErr == nil && sameBoot(parseHostBoot(e.RunningBoot), boot) && groupExists(e.RunningPgid) {
			rt.sched.hold(e.SetID, e.RunningPgid)
			rt.logf("command set run %s: its command still runs as process group %d; the set does not start until that group is gone",
				e.RunUID, e.RunningPgid)
		}
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
		r := &interruptedRun{uid: e.RunUID, setID: e.SetID, at: now, rep: rep}
		if err := rt.storeInterrupted(r); err != nil {
			rt.logf("command set run %s: recording that it was interrupted failed, it is written again: %v", r.uid, err)
			rt.sched.awaitRecord(r.setID)
			pending = append(pending, r)
			continue
		}
		rt.logf("command set run %s was interrupted (the agent ended while it ran)", e.RunUID)
	}
	return pending
}

// storeInterrupted queues the report of an interrupted run and records the
// run as finished, in one transaction.
func (rt *AgentRuntime) storeInterrupted(r *interruptedRun) error {
	return rt.out.queueWith(rt.stamped(&ReportRequest{CommandRuns: []CommandRunReport{r.rep}}), func(tx models.Writer) error {
		return models.FinishCommandExecution(tx, r.uid, r.at)
	})
}

// recordInterrupted writes the records of interrupted runs that could not be
// stored at the start again (storeRunRecord: 1 s, doubling to 30 s, until
// stored or the stop), on a flow of its own, so that neither the monitoring
// step nor the rounds wait for it. The set of a run may start once its
// record is stored (or the record has nothing left to write).
func (rt *AgentRuntime) recordInterrupted(pending []*interruptedRun) {
	for _, r := range pending {
		err := rt.storeRunRecord(fmt.Sprintf("command set run %s: the record that it was interrupted", r.uid), func() error {
			return rt.storeInterrupted(r)
		})
		switch {
		case err == nil:
			rt.logf("command set run %s was interrupted (the agent ended while it ran)", r.uid)
		case isClosed(rt.stop):
			rt.logf("command set run %s: recording that it was interrupted failed (the next start reports it again): %v", r.uid, err)
			continue
		default:
			rt.logf("command set run %s: recording that it was interrupted failed: %v", r.uid, err)
		}
		rt.sched.recordDone(r.setID)
	}
}

// queueCommandReport queues a report about command sets in the outbox; it is
// sent with the next report.
func (rt *AgentRuntime) queueCommandReport(req *ReportRequest) {
	if err := rt.out.queue(rt.stamped(req)); err != nil {
		rt.logf("queuing the answers to run requests failed (they are made again when the requests are delivered again): %v", err)
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
