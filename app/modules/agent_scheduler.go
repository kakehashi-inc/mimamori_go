package modules

import (
	"cmp"
	"slices"
	"sync"
	"time"

	"mimamori/app/models"
)

// The scheduler of the agent: it decides which unit of the agent's work runs
// next. A unit is one monitoring step (sync, audit records, file scans,
// report, pruning of old run records) or one run of a command set. The
// agent's workers ask it for a unit, run the unit and tell it that the unit
// ended. A monitoring step never runs twice at the same time, nor does a
// command set.
//
// Who owns which state of the agent (nothing else is shared):
//
//   - Fixed before Run starts a worker, then only read: the database handle,
//     the logger, the worker count, the clock, the time zone, the kill grace,
//     the scan limit, the rule removal of tests and the stop channel.
//   - The monitoring step (AgentRuntime.mon): the transport, the agent's
//     configuration, the assigned rule and the application results, the
//     state keys of the rule, the audit log and the file scans, the tables
//     findings and scan_roots, the kernel's audit rules and the rules file.
//     Only the flow that runs the monitoring step touches them, between the
//     scheduler handing it the step (next or inlineMonitor) and the step's
//     doneMonitor; the scheduler's mutex orders one step after the other.
//   - The scheduler (this file): when the monitoring step ran and whether it
//     runs, the check interval in use, the command set definitions, the
//     valid sets, the waiting manual requests, the answered requests and the
//     sets that run. Only through its methods, each under its one mutex.
//   - The outbox (agent_outbox.go): the table outbox, through its methods,
//     each one Write.
//   - Each run (runCommandJob): its definition, run UID, results and its own
//     rows of command_executions, and last_due of its set in
//     command_schedules (written before the run). The scheduler's running
//     sets keep every other worker away from the same set.
//
// The scheduler never calls Write while it holds its mutex: answers it finds
// while picking a unit are returned to the worker, which queues them after
// it left the mutex. It reads command_executions and command_schedules on
// the read side under the mutex (a read waits for nothing).

// monitorGap is the least time between the end of a monitoring step and the
// start of the next one (a step that took longer than the check interval is
// followed by the next one this long after its end).
const monitorGap = time.Second

// agentUnit is one unit of the agent's work.
type agentUnit struct {
	monitor bool        // the monitoring step
	job     *commandJob // otherwise: the command set to run
}

// agentScheduler decides what the agent's workers run.
type agentScheduler struct {
	db *DB
	// stop is the agent's stop channel (set by Run before a worker starts).
	stop <-chan struct{}
	now  func() time.Time
	loc  func() *time.Location

	mu sync.Mutex
	// changed is closed and replaced at every change; idle workers wait on
	// it. idle is the number of workers waiting in next.
	changed chan struct{}
	idle    int
	// interval is the check interval in use (given by the monitoring step
	// when it ends), lastMonitor the start and lastEnd the end of the last
	// monitoring step (zero: none yet), monitoring whether one runs.
	interval    time.Duration
	lastMonitor time.Time
	lastEnd     time.Time
	monitoring  bool
	// defs are the stored definitions (valid or not), valid the sets that
	// passed validation on the agent.
	defs  *CommandDefinitions
	valid map[int64]*CommandSetDef
	// requests are the manual requests waiting, oldest first; acked the
	// requests answered or started in this process.
	requests []CommandRunRequest
	acked    map[int64]bool
	// running are the sets with a run in progress; notStarted the scheduled
	// sets whose run could not be started, offered again after the next
	// monitoring step.
	running    map[int64]bool
	notStarted map[int64]bool
	// stoppedRuns counts the runs that ended after the stop.
	stoppedRuns int
}

func newAgentScheduler(db *DB, now func() time.Time, loc func() *time.Location, interval time.Duration) *agentScheduler {
	return &agentScheduler{db: db, now: now, loc: loc, interval: interval, changed: make(chan struct{}),
		valid: map[int64]*CommandSetDef{}, acked: map[int64]bool{}, running: map[int64]bool{}, notStarted: map[int64]bool{}}
}

// changedLocked wakes the idle workers.
func (s *agentScheduler) changedLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

// monitorDueLocked reports whether the monitoring step is due: it does not
// run, and none ran yet, or the check interval has passed since the start of
// the last one and monitorGap since its end.
func (s *agentScheduler) monitorDueLocked(now time.Time) bool {
	if s.monitoring {
		return false
	}
	return s.lastMonitor.IsZero() || (now.Sub(s.lastMonitor) >= s.interval && now.Sub(s.lastEnd) >= monitorGap)
}

// next returns the unit the calling worker runs next, and the answers to
// manual requests found on the way, which the worker queues. With nothing
// to run it waits until the monitoring step is due, the next occurrence of
// a scheduled set comes, something changed or the stop came. It returns no
// unit and no answer only after the stop.
func (s *agentScheduler) next() (*agentUnit, []CommandRequestAck) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		if isClosed(s.stop) {
			return nil, nil
		}
		now := s.now()
		if s.monitorDueLocked(now) {
			s.monitoring, s.lastMonitor = true, now
			return &agentUnit{monitor: true}, nil
		}
		job, acks := s.pickRunLocked(now)
		if job != nil {
			return &agentUnit{job: job}, acks
		}
		if len(acks) > 0 {
			return nil, acks
		}
		wait := s.wakeLocked(now)
		ch := s.changed
		s.idle++
		s.mu.Unlock()
		t := time.NewTimer(wait)
		select {
		case <-s.stop:
		case <-ch:
		case <-t.C:
		}
		t.Stop()
		s.mu.Lock()
		s.idle--
	}
}

// pickRunLocked takes the run to start now: the oldest manual request whose
// set does not run, else among the valid scheduled sets that do not run the
// one whose due occurrence is the oldest (lower ID first). The picked set is
// entered as running, a picked request as answered (its start report
// carries the answer). Requests that were already executed or name a set
// that is not valid are answered and dropped on the way.
func (s *agentScheduler) pickRunLocked(now time.Time) (*commandJob, []CommandRequestAck) {
	var acks []CommandRequestAck
	for i := 0; i < len(s.requests); {
		r := s.requests[i]
		if s.running[r.SetID] {
			i++
			continue
		}
		s.requests = slices.Delete(s.requests, i, i+1)
		s.acked[r.ID] = true
		if e, err := models.CommandExecutionByRequest(s.db.Read(), r.ID); err == nil && e != nil {
			acks = append(acks, CommandRequestAck{RequestID: r.ID, Status: RequestAckStarted, RunUID: e.RunUID, At: now.Unix()})
			continue
		}
		def := s.valid[r.SetID]
		if def == nil {
			acks = append(acks, CommandRequestAck{RequestID: r.ID, Status: RequestAckUnknownSet, At: now.Unix()})
			continue
		}
		s.running[def.ID] = true
		return &commandJob{def: def, trigger: TriggerManual, requestID: r.ID}, acks
	}
	var best *commandJob
	for _, id := range s.scheduledIDsLocked() {
		if s.running[id] || s.notStarted[id] {
			continue
		}
		def := s.valid[id]
		st, err := models.GetCommandSchedule(s.db.Read(), id)
		if err != nil || st == nil {
			continue
		}
		due, ok := def.Schedule.DueToRun(now, time.Unix(st.ArmedAt, 0), time.Unix(st.LastDue, 0), s.loc())
		if !ok {
			continue
		}
		if best == nil || due.Unix() < best.scheduledFor {
			best = &commandJob{def: def, trigger: TriggerSchedule, scheduledFor: due.Unix()}
		}
	}
	if best != nil {
		s.running[best.def.ID] = true
	}
	return best, acks
}

// scheduledIDsLocked returns the valid scheduled sets in ID order.
func (s *agentScheduler) scheduledIDsLocked() []int64 {
	var ids []int64
	for id, d := range s.valid {
		if d.RunMode == RunModeSchedule && d.Schedule != nil {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

// wakeLocked is how long an idle worker waits: until the monitoring step is
// due (unless it runs) or the next occurrence of a valid scheduled set that
// does not run, at least commandPollMax away, whichever comes first.
func (s *agentScheduler) wakeLocked(now time.Time) time.Duration {
	wait := time.Hour
	if !s.monitoring {
		due := s.lastMonitor.Add(s.interval)
		if gap := s.lastEnd.Add(monitorGap); gap.After(due) {
			due = gap
		}
		wait = min(wait, max(due.Sub(now), time.Millisecond))
	}
	for _, id := range s.scheduledIDsLocked() {
		if s.running[id] || s.notStarted[id] {
			continue
		}
		if t, ok := s.valid[id].Schedule.NextDue(now, s.loc()); ok {
			wait = min(wait, max(t.Sub(now), commandPollMax))
		}
	}
	return wait
}

// inlineMonitor is asked by a worker that waits for a running command: it
// returns true, and the monitoring step is then the caller's, when the stop
// has not come, the step is due and no other worker is idle (an idle worker
// takes the step at its due time instead).
func (s *agentScheduler) inlineMonitor() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if isClosed(s.stop) || s.idle > 0 || !s.monitorDueLocked(now) {
		return false
	}
	s.monitoring, s.lastMonitor = true, now
	return true
}

// doneMonitor ends the monitoring step: interval is the check interval the
// step gives. The scheduled sets that could not be started are offered
// again.
func (s *agentScheduler) doneMonitor(interval time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.monitoring = false
	s.lastEnd = s.now()
	s.interval = interval
	clear(s.notStarted)
	s.changedLocked()
}

// doneRun ends the run of job; started is false when the run could not be
// started. A scheduled set that could not be started is offered again after
// the next monitoring step; a manual request that could not be started is
// forgotten, so that the central's next delivery of it is taken again.
func (s *agentScheduler) doneRun(job *commandJob, started bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.running, job.def.ID)
	switch {
	case !started && job.trigger == TriggerSchedule:
		s.notStarted[job.def.ID] = true
	case !started && job.requestID > 0:
		delete(s.acked, job.requestID)
	case started && isClosed(s.stop):
		s.stoppedRuns++
	}
	s.changedLocked()
}

// acceptDefinitions takes stored definitions and their valid sets into use
// (a running set keeps the definition it started with).
func (s *agentScheduler) acceptDefinitions(defs *CommandDefinitions, valid map[int64]*CommandSetDef) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.defs, s.valid = defs, valid
	s.changedLocked()
}

// receiveRequests takes the manual run requests of a sync and returns the
// answers to queue. A request that arrives after its expiry is not run; one
// that was already run (it can be delivered again until the central has the
// answer) is answered with its run; one already waiting or answered in this
// process is ignored.
func (s *agentScheduler) receiveRequests(reqs []CommandRunRequest) []CommandRequestAck {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().Unix()
	var acks []CommandRequestAck
	added := false
	for _, r := range reqs {
		if s.acked[r.ID] || slices.ContainsFunc(s.requests, func(q CommandRunRequest) bool { return q.ID == r.ID }) {
			continue
		}
		if e, err := models.CommandExecutionByRequest(s.db.Read(), r.ID); err == nil && e != nil {
			acks = append(acks, CommandRequestAck{RequestID: r.ID, Status: RequestAckStarted, RunUID: e.RunUID, At: now})
			s.acked[r.ID] = true
			continue
		}
		if r.ExpiresAt > 0 && now > r.ExpiresAt {
			acks = append(acks, CommandRequestAck{RequestID: r.ID, Status: RequestAckExpired, At: now})
			s.acked[r.ID] = true
			continue
		}
		s.requests = append(s.requests, r)
		added = true
	}
	slices.SortStableFunc(s.requests, func(a, b CommandRunRequest) int {
		if c := cmp.Compare(a.RequestedAt, b.RequestedAt); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})
	if added {
		s.changedLocked()
	}
	return acks
}

// signature is the signature of the definitions the agent holds.
func (s *agentScheduler) signature() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.defs == nil {
		return ""
	}
	return s.defs.Signature
}

// findSet returns a stored definition, valid or not (nil when absent).
func (s *agentScheduler) findSet(id int64) *CommandSetDef {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.defs == nil {
		return nil
	}
	for i := range s.defs.Sets {
		if s.defs.Sets[i].ID == id {
			return &s.defs.Sets[i]
		}
	}
	return nil
}

// runsStoppedByStop reports whether a run ended after the stop.
func (s *agentScheduler) runsStoppedByStop() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stoppedRuns > 0
}
