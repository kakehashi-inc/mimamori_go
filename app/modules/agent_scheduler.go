package modules

import (
	"cmp"
	"slices"
	"sync"
	"time"

	"mimamori/app/models"
)

// The queue of the agent's work. An entry is the monitoring step or one run
// of a command set; the agent's workers take one entry at a time and process
// it from its start to its end (agent_runtime.go). The communication with
// the central is not an entry: it runs in its own flow (agentComm) whatever
// the workers do.
//
// Every entry may carry a time "may start at" (startAt; zero: at once). A
// free worker takes, among the entries that may start now and whose command
// set does not run, the one that became startable first (the later of the
// time it was queued and its startAt; on equal times the one queued first).
// With nothing startable it waits until the earliest startAt, a change of
// the queue or the stop.
//
// The queue never holds the same thing twice: there is exactly one
// monitoring entry, except while a worker processes the step; a command set
// has at most one entry and never runs twice at the same time. A scheduled
// set has one entry at its next occurrence (CommandSchedule.Plan), queued
// again only after its run ended. A manual run request is an entry without a
// time that replaces a waiting scheduled entry of its set; while its set
// runs it waits.
//
// Who owns which state of the agent (nothing else is shared):
//
//   - Fixed before Run starts a flow, then only read: the database handle,
//     the logger, the worker count, the clock, the time zone, the kill grace,
//     the retry wait of run records, the scan limit, the rule removal of tests
//     and the stop channel.
//   - The communication flow (AgentRuntime.comm): the transport, the
//     agent's configuration (token, name, CAs) and the state key
//     check_interval.
//   - The monitoring step (AgentRuntime.mon): the applied rule and the
//     application results, the state keys of the rule, the audit log and the
//     file scans, the tables findings and scan_roots, the kernel's audit rules
//     and the rules file. Only the worker that processes the monitoring entry
//     touches them; the queue has one such entry.
//   - The hand-over between the two (AgentRuntime.link, its own mutex): what
//     the monitoring step applied (for the sync) and what the last sync
//     brought (the rule assignment, for the next monitoring step, and the
//     check interval of the rounds and the monitoring entry).
//   - The queue (this file, its one mutex): the entries, the sets that run,
//     the requests taken by a worker, the sets kept by a surviving process
//     group, the command set definitions and the valid sets, the worker
//     count.
//   - The outbox (agent_outbox.go): the table outbox and the state key
//     outbox_batch, through its methods, each one Write.
//   - Each run (agent_commands.go): its definition, run UID, results, its own
//     rows of command_executions and last_due of its set in
//     command_schedules (written with the start of the run).
//   - The held output pipes (leftoverPipes, its own mutex): each has its own
//     reader, which reads and drops what arrives and closes the pipe at its
//     end; Run closes the pipes left after the workers ended.
//
// The queue never calls Write while it holds its mutex: the answers to run
// requests it finds are returned to the caller, which queues them in the
// outbox after it left the mutex. It reads command_executions and
// command_schedules on the read side under the mutex (a read waits for
// nothing).

// heldSetPoll is how often a waiting worker looks again whether the process
// group that keeps a set from starting is gone.
const heldSetPoll = time.Second

// queueEntry is one entry of the queue.
type queueEntry struct {
	seq      uint64
	queuedAt time.Time
	// startAt is when the entry may start (zero: at once).
	startAt time.Time
	monitor bool
	setID   int64
	// request is the run request of a manual entry; scheduledFor the
	// occurrence a scheduled entry runs.
	request      *CommandRunRequest
	scheduledFor int64
}

// startable returns when the entry may start: the later of the time it was
// queued and its startAt.
func (e *queueEntry) startable() time.Time {
	if e.startAt.After(e.queuedAt) {
		return e.startAt
	}
	return e.queuedAt
}

// agentUnit is what a worker processes: the monitoring step or a run.
type agentUnit struct {
	monitor bool        // the monitoring step
	job     *commandJob // otherwise: the run of a command set
}

// agentScheduler is the queue of the agent's work.
type agentScheduler struct {
	db *DB
	// stop is the agent's stop channel (set by Run before a flow starts).
	stop <-chan struct{}
	now  func() time.Time
	loc  func() *time.Location

	mu sync.Mutex
	// workers is the agent's worker count (AgentRuntime.SetWorkers).
	workers int
	// changed is closed and replaced at every change; waiting workers wait
	// on it.
	changed chan struct{}
	seq     uint64
	entries []*queueEntry
	// monitoring: a worker processes the monitoring step; monitorSoon: a
	// changed rule assignment came meanwhile, so the step is queued again
	// without a time.
	monitoring  bool
	monitorSoon bool
	// running are the sets whose run a worker processes; taken the manual
	// requests a worker took (until its run ended); held the sets kept from
	// starting by the process group of a run that survived the agent.
	running map[int64]bool
	taken   map[int64]bool
	held    map[int64]*groupProbe
	// defs are the stored definitions (valid or not), valid the sets that
	// passed validation on the agent.
	defs  *CommandDefinitions
	valid map[int64]*CommandSetDef
}

func newAgentScheduler(db *DB, now func() time.Time, loc func() *time.Location) *agentScheduler {
	return &agentScheduler{db: db, now: now, loc: loc, workers: 1, changed: make(chan struct{}),
		running: map[int64]bool{}, taken: map[int64]bool{}, held: map[int64]*groupProbe{}, valid: map[int64]*CommandSetDef{}}
}

// changedLocked wakes the waiting workers.
func (s *agentScheduler) changedLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

// addLocked queues an entry.
func (s *agentScheduler) addLocked(e *queueEntry, now time.Time) {
	s.seq++
	e.seq, e.queuedAt = s.seq, now
	s.entries = append(s.entries, e)
	s.changedLocked()
}

// removeLocked takes the entry at index i out of the queue.
func (s *agentScheduler) removeLocked(i int) {
	s.entries = slices.Delete(s.entries, i, i+1)
}

// entryOfSetLocked returns the index of the entry of a set (-1: none).
func (s *agentScheduler) entryOfSetLocked(setID int64) int {
	return slices.IndexFunc(s.entries, func(e *queueEntry) bool { return !e.monitor && e.setID == setID })
}

// start queues the monitoring entry without a time and then the entries of
// the valid scheduled sets (the agent starts).
func (s *agentScheduler) start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	// The monitoring step comes first (it applies the stored rule again).
	s.entries = nil
	s.addLocked(&queueEntry{monitor: true}, now)
	for _, id := range s.scheduledIDsLocked() {
		s.planLocked(id, now)
	}
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

// planLocked queues the entry of a scheduled set for its next occurrence
// (CommandSchedule.Plan), replacing a waiting scheduled entry of the set
// unless that one is the same (it keeps its place). A set that runs, or that
// has a manual entry waiting, gets none now (its entry is computed again
// when the run ended); a set without a schedule state or without a next
// occurrence gets none.
func (s *agentScheduler) planLocked(id int64, now time.Time) {
	old := s.entryOfSetLocked(id)
	if old >= 0 && s.entries[old].request != nil {
		return
	}
	var plan *queueEntry
	def := s.valid[id]
	if !s.running[id] && def != nil && def.RunMode == RunModeSchedule && def.Schedule != nil {
		if st, err := models.GetCommandSchedule(s.db.Read(), id); err == nil && st != nil {
			if due, startAt, ok := def.Schedule.Plan(now, time.Unix(st.ArmedAt, 0), time.Unix(st.LastDue, 0), s.loc()); ok {
				plan = &queueEntry{setID: id, startAt: startAt, scheduledFor: due.Unix()}
			}
		}
	}
	if old >= 0 {
		if e := s.entries[old]; plan != nil && e.scheduledFor == plan.scheduledFor && e.startAt.Equal(plan.startAt) {
			return
		}
		s.removeLocked(old)
	}
	if plan != nil {
		s.addLocked(plan, now)
	}
}

// heldLocked reports whether a set is kept from starting by the process
// group of a run that survived the agent; a group without a process that
// has not ended (groupProbe) lets it go.
func (s *agentScheduler) heldLocked(id int64) bool {
	g, ok := s.held[id]
	if !ok {
		return false
	}
	if g.exists() {
		return true
	}
	delete(s.held, id)
	return false
}

// blockedLocked reports whether an entry may not start now for its set: the
// set runs or is held.
func (s *agentScheduler) blockedLocked(e *queueEntry) bool {
	return !e.monitor && (s.running[e.setID] || s.heldLocked(e.setID))
}

// next returns the unit the calling worker processes next, and the answers
// to manual requests found on the way, which the worker queues. With nothing
// startable it waits (see the file comment). It returns no unit and no
// answer only after the stop.
func (s *agentScheduler) next() (*agentUnit, []CommandRequestAck) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		if isClosed(s.stop) {
			return nil, nil
		}
		now := s.now()
		u, acks := s.pickLocked(now)
		if u != nil || len(acks) > 0 {
			return u, acks
		}
		wait := s.waitLocked(now)
		ch := s.changed
		s.mu.Unlock()
		t := time.NewTimer(wait)
		select {
		case <-s.stop:
		case <-ch:
		case <-t.C:
		}
		t.Stop()
		s.mu.Lock()
	}
}

// pickLocked takes the entry to process now by the rule of the file comment.
// A manual entry is answered instead of started when a run is already
// recorded for its request, its expiry has passed or its set is not valid;
// the answers are returned and the next entry is looked at.
func (s *agentScheduler) pickLocked(now time.Time) (*agentUnit, []CommandRequestAck) {
	var acks []CommandRequestAck
	for {
		best := -1
		for i, e := range s.entries {
			if e.startAt.After(now) || s.blockedLocked(e) {
				continue
			}
			if best < 0 || e.startable().Before(s.entries[best].startable()) ||
				(e.startable().Equal(s.entries[best].startable()) && e.seq < s.entries[best].seq) {
				best = i
			}
		}
		if best < 0 {
			return nil, acks
		}
		e := s.entries[best]
		s.removeLocked(best)
		if e.monitor {
			s.monitoring = true
			return &agentUnit{monitor: true}, acks
		}
		def := s.valid[e.setID]
		if r := e.request; r != nil {
			if ack, done := s.answerLocked(r, now); done {
				acks = append(acks, ack)
				continue
			}
			s.running[def.ID], s.taken[r.ID] = true, true
			return &agentUnit{job: &commandJob{def: def, trigger: TriggerManual, requestID: r.ID}}, acks
		}
		if def == nil || def.RunMode != RunModeSchedule {
			continue
		}
		s.running[def.ID] = true
		return &agentUnit{job: &commandJob{def: def, trigger: TriggerSchedule, scheduledFor: e.scheduledFor}}, acks
	}
}

// answerLocked returns the answer to a manual request that is not started,
// from what is stored: a run is recorded for it (started, with that run),
// its expiry has passed (expired) or its set is not among the valid
// definitions (unknown_set). done is false when the request may start.
func (s *agentScheduler) answerLocked(r *CommandRunRequest, now time.Time) (CommandRequestAck, bool) {
	at := now.Unix()
	if e, err := models.CommandExecutionByRequest(s.db.Read(), r.ID); err == nil && e != nil {
		return CommandRequestAck{RequestID: r.ID, Status: RequestAckStarted, RunUID: e.RunUID, At: at}, true
	}
	if r.ExpiresAt > 0 && at > r.ExpiresAt {
		return CommandRequestAck{RequestID: r.ID, Status: RequestAckExpired, At: at}, true
	}
	if s.valid[r.SetID] == nil {
		return CommandRequestAck{RequestID: r.ID, Status: RequestAckUnknownSet, At: at}, true
	}
	return CommandRequestAck{}, false
}

// waitLocked is how long a waiting worker waits at most: until the earliest
// startAt, or heldSetPoll while an entry that may start waits only for a
// set kept by a surviving process group.
func (s *agentScheduler) waitLocked(now time.Time) time.Duration {
	wait := time.Hour
	for _, e := range s.entries {
		switch {
		case e.startAt.After(now):
			wait = min(wait, max(e.startAt.Sub(now), time.Millisecond))
		case !e.monitor && !s.running[e.setID] && s.held[e.setID] != nil:
			wait = min(wait, heldSetPoll)
		}
	}
	return wait
}

// doneMonitor ends the monitoring step: it is queued again to start one
// check interval later (without a time when a changed rule assignment came
// while it ran).
func (s *agentScheduler) doneMonitor(interval time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	e := &queueEntry{monitor: true, startAt: now.Add(interval)}
	if s.monitorSoon {
		e.startAt = time.Time{}
	}
	s.monitoring, s.monitorSoon = false, false
	s.addLocked(e, now)
}

// monitorNow removes the time of the queued monitoring entry (a sync brought
// a rule assignment that differs from the applied one); while the step runs,
// it is queued again without a time.
func (s *agentScheduler) monitorNow() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.entries {
		if e.monitor {
			e.startAt = time.Time{}
			s.changedLocked()
			return
		}
	}
	if s.monitoring {
		s.monitorSoon = true
	}
}

// doneRun ends the run of job: a scheduled set gets the entry of its next
// occurrence, and a manual entry of the set that waited may start.
func (s *agentScheduler) doneRun(job *commandJob) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.running, job.def.ID)
	delete(s.taken, job.requestID)
	s.planLocked(job.def.ID, s.now())
	s.changedLocked()
}

// acceptDefinitions takes stored definitions and their valid sets into use (a
// running set keeps the definition it started with): the entries of the
// scheduled sets are computed again, and the scheduled entries of sets that
// are no longer valid scheduled sets are removed.
func (s *agentScheduler) acceptDefinitions(defs *CommandDefinitions, valid map[int64]*CommandSetDef) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.defs, s.valid = defs, valid
	now := s.now()
	for i := 0; i < len(s.entries); {
		e := s.entries[i]
		if !e.monitor && e.request == nil {
			if d := valid[e.setID]; d == nil || d.RunMode != RunModeSchedule || d.Schedule == nil {
				s.removeLocked(i)
				continue
			}
		}
		i++
	}
	for _, id := range s.scheduledIDsLocked() {
		s.planLocked(id, now)
	}
	s.changedLocked()
}

// receive takes the manual run requests of a sync and returns the answers to
// queue, made from what is stored every time a request is delivered: a run
// recorded for it is answered started with that run; a request that waits in
// the queue or is being started gets no new answer (a waiting one whose
// expiry has passed is taken out and answered expired); an expired request
// is answered expired and one whose set is not valid unknown_set. The
// others are queued; a manual entry replaces a waiting scheduled entry of
// its set. A request for a set that has another manual entry waiting is
// left for a later delivery. The requests of one sync are queued oldest
// first.
func (s *agentScheduler) receive(reqs []CommandRunRequest) []CommandRequestAck {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	reqs = slices.Clone(reqs)
	slices.SortStableFunc(reqs, func(a, b CommandRunRequest) int {
		if c := cmp.Compare(a.RequestedAt, b.RequestedAt); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})
	var acks []CommandRequestAck
	for _, r := range reqs {
		if e, err := models.CommandExecutionByRequest(s.db.Read(), r.ID); err == nil && e != nil {
			acks = append(acks, CommandRequestAck{RequestID: r.ID, Status: RequestAckStarted, RunUID: e.RunUID, At: now.Unix()})
			continue
		}
		if s.taken[r.ID] {
			continue
		}
		if i := slices.IndexFunc(s.entries, func(e *queueEntry) bool { return e.request != nil && e.request.ID == r.ID }); i >= 0 {
			if ack, done := s.answerLocked(s.entries[i].request, now); done && ack.Status == RequestAckExpired {
				s.removeLocked(i)
				acks = append(acks, ack)
			}
			continue
		}
		if ack, done := s.answerLocked(&r, now); done {
			acks = append(acks, ack)
			continue
		}
		if i := s.entryOfSetLocked(r.SetID); i >= 0 {
			if s.entries[i].request != nil {
				continue
			}
			s.removeLocked(i)
		}
		req := r
		s.addLocked(&queueEntry{setID: r.SetID, request: &req}, now)
	}
	return acks
}

// hold keeps a set from starting while the process group pgid exists (a run
// of it that survived the agent).
func (s *agentScheduler) hold(setID int64, pgid int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.held[setID] = &groupProbe{pgid: pgid}
}

// setWorkers sets the agent's worker count.
func (s *agentScheduler) setWorkers(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.workers = n
}

// freeWorker reports whether the agent can take a manual run request now
// (free_worker of the sync): a worker is idle once the entries that may
// start now are taken, and no manual entry waits in the queue.
func (s *agentScheduler) freeWorker() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	busy := len(s.running)
	if s.monitoring {
		busy++
	}
	for _, e := range s.entries {
		if e.request != nil {
			return false
		}
		if !e.startAt.After(now) && !s.blockedLocked(e) {
			busy++
		}
	}
	return busy < s.workers
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
