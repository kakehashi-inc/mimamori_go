package modules

import (
	"cmp"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"mimamori/app/models"
)

// Command sets on the agent: storing the delivered definitions and run
// requests, deciding when a set runs (manual requests first, then due
// schedules by due time, one set at a time), running a set command by
// command and queueing its results in the outbox. Everything runs in the
// agent's single flow; while a command runs, its wait loop calls the
// monitoring step when it is due.

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

func (rt *AgentRuntime) isStopping() bool {
	return rt.stopping != nil && rt.stopping()
}

// commandSignature is the signature of the definitions the agent holds.
func (rt *AgentRuntime) commandSignature() string {
	if rt.cmdDefs == nil {
		return ""
	}
	return rt.cmdDefs.Signature
}

// loadCommandDefinitions restores the stored definitions at start.
func (rt *AgentRuntime) loadCommandDefinitions() {
	v := models.GetLocalState(rt.DB, StateCommandDefinitions)
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
// changed.
func (rt *AgentRuntime) acceptCommandDefinitions(defs *CommandDefinitions) []CommandApplyProblem {
	problems := []CommandApplyProblem{}
	valid := map[int64]*CommandSetDef{}
	var scheduled []int64
	now := rt.now().Unix()
	for i := range defs.Sets {
		d := defs.Sets[i]
		if errs := d.Validate(0); len(errs) > 0 {
			problems = append(problems, CommandApplyProblem{SetID: d.ID, SetName: d.Name, Problems: errs})
			continue
		}
		valid[d.ID] = &d
		if d.RunMode == RunModeSchedule && d.Schedule != nil {
			scheduled = append(scheduled, d.ID)
			if _, err := models.ArmCommandSchedule(rt.DB, d.ID, ScheduleSignature(&d), now); err != nil {
				rt.logf("command set %q: storing the schedule state failed: %v", d.Name, err)
			}
		}
	}
	if err := models.DeleteCommandSchedulesExcept(rt.DB, scheduled); err != nil {
		rt.logf("removing old schedule states failed: %v", err)
	}
	rt.cmdDefs = defs
	rt.cmdValid = valid
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
	if err := models.SetLocalState(rt.DB, StateCommandDefinitions, string(b)); err != nil {
		// Not taken into use; the central delivers them again.
		rt.logf("command set definitions not stored: %v", err)
		return
	}
	problems := rt.acceptCommandDefinitions(defs)
	for _, p := range problems {
		rt.logf("command set %q is not run: %s", p.SetName, joinMax(p.Problems, 5))
	}
	rt.logf("command set definitions received: %d set(s)", len(defs.Sets))
	rt.queueCommandReport(&ReportRequest{CommandApply: &CommandApplyResult{
		Signature: defs.Signature, At: rt.now().Unix(), Problems: problems,
	}})
}

// receiveCommandRequests takes the manual run requests of a sync. A request
// that arrives after its expiry is not run; one that was already run (it can
// be delivered again until the central has the answer) is answered with its
// run.
func (rt *AgentRuntime) receiveCommandRequests(reqs []CommandRunRequest) {
	now := rt.now().Unix()
	var acks []CommandRequestAck
	for _, r := range reqs {
		if rt.cmdAcked[r.ID] || slices.ContainsFunc(rt.cmdRequests, func(q CommandRunRequest) bool { return q.ID == r.ID }) {
			continue
		}
		if e, err := models.CommandExecutionByRequest(rt.DB, r.ID); err == nil && e != nil {
			acks = append(acks, CommandRequestAck{RequestID: r.ID, Status: RequestAckStarted, RunUID: e.RunUID, At: now})
			continue
		}
		if r.ExpiresAt > 0 && now > r.ExpiresAt {
			acks = append(acks, CommandRequestAck{RequestID: r.ID, Status: RequestAckExpired, At: now})
			continue
		}
		rt.cmdRequests = append(rt.cmdRequests, r)
	}
	slices.SortStableFunc(rt.cmdRequests, func(a, b CommandRunRequest) int {
		if c := cmp.Compare(a.RequestedAt, b.RequestedAt); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})
	rt.queueAcks(acks)
}

func (rt *AgentRuntime) queueAcks(acks []CommandRequestAck) {
	if len(acks) == 0 {
		return
	}
	for _, a := range acks {
		rt.cmdAcked[a.RequestID] = true
	}
	rt.queueCommandReport(&ReportRequest{CommandRequestAcks: acks})
}

// nextCommandJob returns the set to run now: the oldest manual request, else
// the scheduled set whose due occurrence is the oldest. nil when nothing is
// due.
func (rt *AgentRuntime) nextCommandJob() *commandJob {
	now := rt.now()
	for len(rt.cmdRequests) > 0 {
		r := rt.cmdRequests[0]
		rt.cmdRequests = rt.cmdRequests[1:]
		if e, err := models.CommandExecutionByRequest(rt.DB, r.ID); err == nil && e != nil {
			rt.queueAcks([]CommandRequestAck{{RequestID: r.ID, Status: RequestAckStarted, RunUID: e.RunUID, At: now.Unix()}})
			continue
		}
		def := rt.cmdValid[r.SetID]
		if def == nil {
			rt.queueAcks([]CommandRequestAck{{RequestID: r.ID, Status: RequestAckUnknownSet, At: now.Unix()}})
			continue
		}
		return &commandJob{def: def, trigger: TriggerManual, requestID: r.ID}
	}
	var best *commandJob
	for _, id := range rt.scheduledSetIDs() {
		def := rt.cmdValid[id]
		st, err := models.GetCommandSchedule(rt.DB, id)
		if err != nil || st == nil {
			continue
		}
		due, ok := def.Schedule.DueToRun(now, time.Unix(st.ArmedAt, 0), time.Unix(st.LastDue, 0), rt.location())
		if !ok {
			continue
		}
		if best == nil || due.Unix() < best.scheduledFor {
			best = &commandJob{def: def, trigger: TriggerSchedule, scheduledFor: due.Unix()}
		}
	}
	return best
}

// scheduledSetIDs returns the valid scheduled sets in ID order.
func (rt *AgentRuntime) scheduledSetIDs() []int64 {
	var ids []int64
	for id, d := range rt.cmdValid {
		if d.RunMode == RunModeSchedule && d.Schedule != nil {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

// nextScheduleDue returns the next due time of any scheduled set after now.
func (rt *AgentRuntime) nextScheduleDue() (time.Time, bool) {
	now := rt.now()
	var next time.Time
	for _, id := range rt.scheduledSetIDs() {
		if t, ok := rt.cmdValid[id].Schedule.NextDue(now, rt.location()); ok && (next.IsZero() || t.Before(next)) {
			next = t
		}
	}
	return next, !next.IsZero()
}

// runCommandJob runs a set to its end with the definition of the job. It
// returns false when the run could not be started.
func (rt *AgentRuntime) runCommandJob(job *commandJob) bool {
	def := job.def
	now := rt.now().Unix()
	if job.trigger == TriggerSchedule {
		// Recorded before the run, so that an occurrence is never run twice
		// (a run cut short by a crash is reported as interrupted).
		if err := models.SetCommandScheduleLastDue(rt.DB, def.ID, job.scheduledFor); err != nil {
			rt.logf("command set %q: recording the schedule failed, not run: %v", def.Name, err)
			return false
		}
	}
	uid, err := RandomHex(16)
	if err != nil {
		rt.logf("command set %q not run: %v", def.Name, err)
		return false
	}
	exec := &models.CommandExecution{RunUID: uid, SetID: def.ID, SetRevision: def.Revision, RequestID: job.requestID,
		Trigger: job.trigger, ScheduledFor: job.scheduledFor, StartedAt: now}
	if err := models.InsertCommandExecution(rt.DB, exec); err != nil {
		if errors.Is(err, models.ErrAlreadyExecuted) {
			if e, err := models.CommandExecutionByRequest(rt.DB, job.requestID); err == nil && e != nil {
				rt.queueAcks([]CommandRequestAck{{RequestID: job.requestID, Status: RequestAckStarted, RunUID: e.RunUID, At: now}})
			}
			return true
		}
		rt.logf("command set %q not run: recording the run failed: %v", def.Name, err)
		return false
	}
	base := CommandRunReport{
		RunUID: uid, SetID: def.ID, SetRevision: def.Revision, SetName: def.Name, Purpose: def.Purpose, Joiner: def.Joiner,
		OSUser: def.OSUser, WorkDir: def.WorkDir, Trigger: job.trigger, RequestID: job.requestID, ScheduledFor: job.scheduledFor,
		CommandsTotal: len(def.Commands), StartedAt: now, Results: []CommandResultReport{},
	}
	start := &ReportRequest{CommandRuns: []CommandRunReport{base}}
	if job.requestID > 0 {
		start.CommandRequestAcks = []CommandRequestAck{{RequestID: job.requestID, Status: RequestAckStarted, RunUID: uid, At: now}}
		rt.cmdAcked[job.requestID] = true
	}
	rt.queueCommandReport(start)
	rt.logf("command set %q started (%s, run %s)", def.Name, job.trigger, uid)

	hooks := CommandExecHooks{MonitorDue: rt.monitorDue, Monitor: rt.monitorStep, Stopping: rt.isStopping}
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
	closed := false
	for i := range def.Commands {
		if i > 0 && rt.isStopping() {
			break
		}
		c := &def.Commands[i]
		res := ExecuteCommand(CommandExecInput{Position: i, OSUser: def.OSUser, WorkDir: def.WorkDir, Command: c,
			CollectOutput: collect, KillGrace: rt.cmdKillGrace}, hooks)
		kinds = append(kinds, res.Kind)
		if res.Values != nil {
			values = append(values, *res.Values)
		}
		last := i == len(def.Commands)-1 || !c.ContinuesAfter(res.Kind) || res.Reason == ReasonInterrupted
		rep := base
		rep.Results = []CommandResultReport{*res}
		if last {
			finish(&rep, "")
			closed = true
		}
		rt.queueCommandReport(&ReportRequest{CommandRuns: []CommandRunReport{rep}})
		if last {
			break
		}
	}
	if !closed {
		// The agent stopped between two commands (or the set has none).
		rep := base
		reason := ""
		if len(def.Commands) > 0 {
			reason = ReasonInterrupted
		}
		finish(&rep, reason)
		rt.queueCommandReport(&ReportRequest{CommandRuns: []CommandRunReport{rep}})
	}
	if err := models.FinishCommandExecution(rt.DB, uid, rt.now().Unix()); err != nil {
		rt.logf("command set %q: recording the end of run %s failed: %v", def.Name, uid, err)
	}
	rt.logf("command set %q finished: %s", def.Name, RunState(kinds))
	return true
}

// recoverInterruptedRuns reports the runs found unfinished at start (the
// agent ended while they ran) as interrupted.
func (rt *AgentRuntime) recoverInterruptedRuns() {
	runs, err := models.ListUnfinishedCommandExecutions(rt.DB)
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
		if d := rt.findCommandSet(e.SetID); d != nil {
			rep.SetName, rep.Purpose, rep.Joiner, rep.OSUser, rep.WorkDir = d.Name, d.Purpose, d.Joiner, d.OSUser, d.WorkDir
			rep.CommandsTotal = len(d.Commands)
		}
		rt.queueCommandReport(&ReportRequest{CommandRuns: []CommandRunReport{rep}})
		if err := models.FinishCommandExecution(rt.DB, e.RunUID, now); err != nil {
			rt.logf("recording the end of run %s failed: %v", e.RunUID, err)
		}
		rt.logf("command set run %s was interrupted (the agent ended while it ran)", e.RunUID)
	}
}

// findCommandSet returns a stored definition, valid or not (nil when absent).
func (rt *AgentRuntime) findCommandSet(id int64) *CommandSetDef {
	if rt.cmdDefs == nil {
		return nil
	}
	for i := range rt.cmdDefs.Sets {
		if rt.cmdDefs.Sets[i].ID == id {
			return &rt.cmdDefs.Sets[i]
		}
	}
	return nil
}

// queueCommandReport queues a report about command sets in the outbox; it is
// sent with the next report.
func (rt *AgentRuntime) queueCommandReport(req *ReportRequest) {
	req.CreatedAt = rt.now().Unix()
	b, err := json.Marshal(req)
	if err != nil {
		rt.logf("failed to queue a command set report: %v", err)
		return
	}
	if err := models.InsertOutbox(rt.DB, &models.OutboxItem{Payload: string(b), CommandBytes: commandBytesOf(req, len(b))}); err != nil {
		rt.logf("failed to queue a command set report: %v", err)
		return
	}
	rt.enforceCommandOutboxLimit()
}

// commandBytesOf is the size counted against the outbox limit of command set
// reports: the payload size when the report carries runs.
func commandBytesOf(req *ReportRequest, size int) int64 {
	if len(req.CommandRuns) == 0 {
		return 0
	}
	return int64(size)
}

// enforceCommandOutboxLimit keeps the queued command set reports under
// MaxOutboxCommandBytes. Oldest first, it drops the console output of
// run_only sets, then of collect_output sets (their values are kept, the
// results are marked truncated), then the reports of commands that do not
// close a run, and last the closing reports.
func (rt *AgentRuntime) enforceCommandOutboxLimit() {
	limit := int64(MaxOutboxCommandBytes)
	if rt.cmdOutboxLimit > 0 {
		limit = rt.cmdOutboxLimit
	}
	total := models.OutboxCommandBytes(rt.DB)
	if total <= limit {
		return
	}
	items, err := models.ListOutbox(rt.DB)
	if err != nil {
		return
	}
	dropped := 0
	for pass := 0; pass < 4 && total > limit; pass++ {
		for _, it := range items {
			if total <= limit {
				break
			}
			if it.CommandBytes == 0 {
				continue
			}
			var req ReportRequest
			if json.Unmarshal([]byte(it.Payload), &req) != nil {
				continue
			}
			changed := false
			kept := req.CommandRuns[:0]
			for _, run := range req.CommandRuns {
				switch pass {
				case 0, 1:
					if (pass == 0) != (run.Purpose == PurposeRunOnly) {
						break
					}
					for j := range run.Results {
						if len(run.Results[j].Output) > 0 {
							run.Results[j].Output = nil
							run.Results[j].Truncated = true
							changed = true
						}
					}
				case 2, 3:
					if (pass == 3) == run.Finished {
						changed = true
						dropped++
						continue
					}
				}
				kept = append(kept, run)
			}
			req.CommandRuns = kept
			if !changed {
				continue
			}
			if len(req.CommandRuns) == 0 && len(req.Detections) == 0 && len(req.CommandRequestAcks) == 0 &&
				req.CommandApply == nil && req.Dropped == 0 && req.HostInfo == nil && req.Apply == nil {
				_ = models.DeleteOutbox(rt.DB, it.ID)
				total -= it.CommandBytes
				it.CommandBytes = 0
				continue
			}
			b, err := json.Marshal(&req)
			if err != nil {
				continue
			}
			nb := commandBytesOf(&req, len(b))
			total -= it.CommandBytes - nb
			it.Payload, it.CommandBytes = string(b), nb
			_ = models.UpdateOutbox(rt.DB, it)
		}
	}
	if dropped > 0 {
		rt.logf("report storage limit reached: dropped %d command set report(s)", dropped)
	} else {
		rt.logf("report storage limit reached: dropped the console output of queued command set results")
	}
}

// pruneCommandExecutions removes old records of finished runs.
func (rt *AgentRuntime) pruneCommandExecutions() {
	_, _ = models.DeleteFinishedCommandExecutionsBefore(rt.DB, rt.now().Add(-commandExecutionKeep).Unix())
}
