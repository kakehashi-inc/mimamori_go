package modules

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"runtime/debug"
	"strconv"
	"sync"
	"time"

	"mimamori/app/models"
)

// AgentTransport delivers the agent's requests to the central. The remote
// agent uses HTTPS; the central's built-in agent hands them over in process.
type AgentTransport interface {
	Sync(req *SyncRequest) (*SyncResponse, error)
	Report(batch *ReportBatch) error
}

// Local state keys of the agent database.
const (
	stateRule           = "rule"
	stateApply          = "apply"
	stateLastFullScan   = "last_full_scan"
	stateScanSignature  = "scan_signature"
	stateScanPrevRoots  = "scan_prev_roots"
	stateDroppedPending = "dropped_pending"
	stateLastAuditEvent = "last_audit_event"
	// The check interval the last sync answer gave the agent, in seconds
	// (SyncResponse.CheckInterval), used until the first sync after a start.
	stateCheckInterval = "check_interval"
	// The batch of queued items being sent: its identity and the ids of its
	// items (outboxBatch as JSON), until the central answered it.
	stateOutboxBatch = "outbox_batch"
	// A writable scan split over several checks: where the next check
	// continues and the pass in progress (start time, roots found so far).
	stateWritableResume = "writable_resume"
	stateWritablePass   = "writable_pass"
	// The executed-file scan: where the next check continues, the start of
	// the split pass, the since times of the parts of the walk order and
	// those of roots not walked to the end yet (see scanFiles).
	stateExecResume    = "exec_resume"
	stateExecPassStart = "exec_pass_start"
	stateExecSince     = "exec_since"
	stateRootSince     = "exec_root_since"
	// The kernel's global audit settings before the first application of
	// audit rules (AuditGlobals as JSON), set back when the rules are
	// removed.
	stateAuditGlobals = "audit_globals"
)

// AgentRuntime performs the work of an agent. Its workers, as many as its
// worker count, take the entries of its queue (agent_scheduler.go) one at a
// time and process each to its end: the monitoring step (applying rules,
// reading the audit log, scanning files, queuing the periodic report) and
// the runs of command sets. Its communication flow (agent_comm.go) talks to
// the central every check interval (sync, then the report of what is
// queued), whatever the workers do. The file comment of agent_scheduler.go
// says who owns which state.
//
// DB is the handle of the agent's local database. What the runtime reads it
// reads through DB.Read; every change is a DB.Write, and what decides a
// change is read inside that Write. A request to the central, an external
// program, a file walk or a running command never happens inside a Write.
type AgentRuntime struct {
	DB     *DB
	Logger *log.Logger

	// mon is the state of the monitoring step, touched only by the worker
	// that processes the step.
	mon monitorState
	// comm is the state of the communication flow.
	comm commState
	// link is what the communication flow and the monitoring step hand
	// each other.
	link syncLink
	// sched is the queue of the workers.
	sched *agentScheduler
	// out is the outbox, the only owner of the queued reports.
	out *outbox
	// leftovers holds the output pipes that processes left behind by
	// commands still hold open, each read by its own reader.
	leftovers leftoverPipes
	// early asks the communication flow for the next round at once (a
	// manually requested run ended); it holds at most one request.
	early chan struct{}

	// Fixed before Run starts a flow, then only read. workers is the number
	// of entries processed at the same time (SetWorkers; 1 unless set);
	// stop is the channel given to Run, closed when the agent stops.
	workers int
	stop    <-chan struct{}
	// clock, loc, cmdKillGrace and recordRetry replace the time, the host's
	// time zone, the grace before SIGKILL and the first wait before a run
	// record is written again (RunRecordRetryFirst) in tests.
	clock        func() time.Time
	loc          *time.Location
	cmdKillGrace time.Duration
	recordRetry  time.Duration
	// scanLimit replaces MaxScanEntries and removeAudit replaces
	// RemoveAuditRules in tests.
	scanLimit   int
	removeAudit func(restore *AuditGlobals) error
}

// monitorState is what only the monitoring step uses.
type monitorState struct {
	rule      *RuleAssignment
	lastApply *ApplyResult
	// failedRule is the rule version whose application failed. It is not
	// retried every step (each attempt replaces the loaded rules); a new
	// version or a restart of the agent tries again.
	failedRule string
	// lastOK is the result of the last successful application, restored when
	// the central assigns the running version again after a failed one.
	lastOK *ApplyResult
	// started is set by the first step, which applies the stored rule again.
	started bool
}

// syncLink is what the communication flow and the monitoring step hand each
// other, under its own mutex: the monitoring step publishes what it applied
// (sent with every sync); the communication flow puts what the last sync
// brought (the rule assignment, which the next monitoring step applies, and
// the check interval of the rounds and the monitoring entry).
type syncLink struct {
	mu sync.Mutex
	// Published by the monitoring step. ruleInterval is the applied rule's
	// check interval, used only while no sync gave one (syncInterval 0).
	ruleID       int64
	ruleVersion  int
	ruleInterval int
	holds        bool
	failedRule   string
	lastApply    *ApplyResult
	// Put by the communication flow. known: a sync brought an assignment;
	// cas are the CAs the agent trusts; syncInterval is the check interval
	// the last sync answer gave (SyncResponse.CheckInterval; at the start
	// the one stored from the last sync, 0 when none is stored).
	known        bool
	cas          []string
	assigned     *RuleAssignment
	syncInterval int
}

// publish takes over what the monitoring step applied.
func (l *syncLink) publish(m *monitorState) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ruleID, l.ruleVersion, l.ruleInterval = 0, 0, 0
	if m.rule != nil {
		l.ruleID, l.ruleVersion, l.ruleInterval = m.rule.ID, m.rule.Version, m.rule.Content.CheckInterval()
	}
	l.holds, l.failedRule = holdsRule(m.rule, m.lastApply), m.failedRule
	l.lastApply = nil
	if m.lastApply != nil {
		a := *m.lastApply
		l.lastApply = &a
	}
}

// applied returns what the monitoring step applied: the rule and its last
// application result (for the sync).
func (l *syncLink) applied() (ruleID int64, ruleVersion int, lastApply *ApplyResult) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.ruleID, l.ruleVersion, l.lastApply
}

// interval is the check interval of the rounds and of the monitoring entry:
// the one the last sync answer gave (that of the assigned rule version, else
// the central's default of that answer), whatever the application of the
// assignment gave, so that the agent uses the interval by which the central
// judges it. Before the first sync after a start without a stored one: the
// applied rule's, else DefaultCheckIntervalSec.
func (l *syncLink) interval() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case l.syncInterval > 0:
		return time.Duration(l.syncInterval) * time.Second
	case l.ruleInterval > 0:
		return time.Duration(l.ruleInterval) * time.Second
	}
	return DefaultCheckIntervalSec * time.Second
}

// offer puts what a sync brought: the rule assignment and the check interval
// the answer gives (0 keeps the interval). It reports whether the assignment
// differs from what is applied (a version whose application failed does not
// count: it is not tried again) and whether the interval changed.
func (l *syncLink) offer(rule *RuleAssignment, interval int) (differs, intervalChanged bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if interval > 0 && interval != l.syncInterval {
		l.syncInterval, intervalChanged = interval, true
	}
	l.known, l.assigned = true, rule
	if rule == nil {
		return l.holds, intervalChanged
	}
	return (rule.ID != l.ruleID || rule.Version != l.ruleVersion) && ruleKey(rule) != l.failedRule, intervalChanged
}

// setCAs puts the CAs the agent trusts (the communication flow saved them).
func (l *syncLink) setCAs(pems []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cas = append([]string(nil), pems...)
}

// trustedCAs returns the CAs the agent trusts (for the host information).
func (l *syncLink) trustedCAs() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cas
}

// assignment returns the rule assignment of the last sync that brought one
// (known is false before the first).
func (l *syncLink) assignment() (rule *RuleAssignment, known bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.assigned, l.known
}

// NewAgentRuntime loads the stored state. tr is the transport to the central.
func NewAgentRuntime(db *DB, tr AgentTransport, logger *log.Logger) *AgentRuntime {
	rt := &AgentRuntime{DB: db, Logger: logger, workers: 1, comm: commState{transport: tr}, early: make(chan struct{}, 1)}
	// The program wrote the stored interval itself (a positive number).
	if n, err := strconv.Atoi(models.GetLocalState(db.Read(), stateCheckInterval)); err == nil && n > 0 {
		rt.link.syncInterval = n
	}
	rt.out = newOutbox(db, rt.logf)
	if v := models.GetLocalState(db.Read(), stateRule); v != "" {
		var r RuleAssignment
		if json.Unmarshal([]byte(v), &r) == nil {
			r.Content.Normalize()
			rt.mon.rule = &r
		}
	}
	if v := models.GetLocalState(db.Read(), stateApply); v != "" {
		var a ApplyResult
		if json.Unmarshal([]byte(v), &a) == nil {
			rt.mon.lastApply = &a
		}
	}
	rt.link.publish(&rt.mon)
	rt.sched = newAgentScheduler(db, rt.now, rt.location)
	rt.loadCommandDefinitions()
	return rt
}

// SetWorkers sets the number of entries processed at the same time (1 to
// MaxAgentWorkers, see LoadAgentWorkers). It is called before Run.
func (rt *AgentRuntime) SetWorkers(n int) {
	rt.workers = max(1, min(n, MaxAgentWorkers))
	rt.sched.setWorkers(rt.workers)
}

func (rt *AgentRuntime) logf(format string, a ...any) {
	if rt.Logger != nil {
		rt.Logger.Printf(format, a...)
	}
}

// recovered runs fn and recovers a panic of it: the panic is logged with its
// stack after the prefix what ("worker panic", "communication panic") and
// recovered reports true. The agent goes on.
func (rt *AgentRuntime) recovered(what string, fn func()) (panicked bool) {
	defer func() {
		if v := recover(); v != nil {
			panicked = true
			rt.logf("%s: %v\n%s", what, v, debug.Stack())
		}
	}()
	fn()
	return false
}

// ReapplyStored re-applies the stored rule, because a reboot or an auditd
// restart reloads the OS defaults (the first monitoring step does it).
func (rt *AgentRuntime) ReapplyStored() {
	if rt.mon.rule == nil {
		return
	}
	rt.logf("re-applying rule %q version %d", rt.mon.rule.Name, rt.mon.rule.Version)
	rt.applyRule(rt.mon.rule, true)
}

// Run runs the agent until stop is closed (a nil stop never is). The runs
// found unfinished are reported first (recoverInterruptedRuns); then the
// queue gets the monitoring entry and the entries of the scheduled sets, the
// communication flow starts and so do the workers (the calling flow is the
// first of them).
//
// After the stop no entry starts; a monitoring step in progress completes
// (see monitorStep), and a running command is ended (SIGTERM to its process
// group, SIGKILL to what is left after the grace) and its run is recorded.
// The communication flow does no round after the stop; when every worker has
// returned, the output pipes that processes left behind by commands still
// hold are closed (their readers end before Run returns), and the
// communication flow sends one last report that says that the agent stops
// (stopping true) and carries what is queued (one attempt; what fails stays
// queued for the next start). Then Run returns. While it runs, the state key
// running_workers holds the worker count in use (for agent status and agent
// set-workers).
//
// A panic inside an entry or a round is recovered there (see process and
// agentComm); the agent goes on and no command is signalled for it.
func (rt *AgentRuntime) Run(stop <-chan struct{}) {
	rt.stop = stop
	rt.sched.stop = stop
	if err := recordRunningWorkers(rt.DB, rt.workers); err != nil {
		rt.logf("failed to record the worker count in use: %v", err)
	}
	defer func() {
		if err := clearRunningWorkers(rt.DB); err != nil {
			rt.logf("failed to clear the worker count in use: %v", err)
		}
	}()
	rt.recovered("panic", rt.recoverInterruptedRuns)
	rt.sched.start()
	workersDone := make(chan struct{})
	commDone := make(chan struct{})
	go func() {
		defer close(commDone)
		rt.communicate(workersDone)
	}()
	var wg sync.WaitGroup
	for i := 1; i < rt.workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rt.work()
		}()
	}
	rt.work()
	wg.Wait()
	rt.leftovers.closeAll()
	close(workersDone)
	<-commDone
}

// work is one worker: it processes the entries the queue gives it until the
// stop.
func (rt *AgentRuntime) work() {
	for {
		u, acks := rt.sched.next()
		rt.queueAcks(acks)
		if u == nil {
			if len(acks) == 0 {
				return
			}
			continue
		}
		rt.process(u)
	}
}

// process processes one entry. The monitoring step is queued again after it
// ended (also after a panic, which ends it there). A run of a command set
// recovers its own panics (runCommandJob); the end of a manually requested
// run asks for the next round at once, so that its result reaches the
// central soon.
func (rt *AgentRuntime) process(u *agentUnit) {
	if u.monitor {
		rt.recovered("worker panic", rt.monitorStep)
		rt.link.publish(&rt.mon)
		rt.sched.doneMonitor(rt.link.interval())
		return
	}
	started := rt.runCommandJob(u.job)
	rt.sched.doneRun(u.job)
	if started && u.job.trigger == TriggerManual {
		select {
		case rt.early <- struct{}{}:
		default:
		}
	}
}

// monitorStep performs one monitoring step: it applies or removes the audit
// rules when the assignment the last sync brought differs from what is
// applied (the first step applies the stored rule again), reads the audit
// log, scans files, queues one periodic report (detections, host
// information, application result, dropped count) for the next round and
// prunes old run records. It does not talk to the central. A stop does not
// interrupt it: the step completes and saves its state, with these
// exceptions. A tool the step waits for when the stop comes is ended
// (RunTool) and its part fails as a failure of that tool does. After the stop
// the step starts no tool: the audit log is not read (the position stays),
// a rule application or removal that has not begun is not begun, and the
// report is queued without host information. An application or removal that
// has begun runs to its end.
func (rt *AgentRuntime) monitorStep() {
	cycleStart := time.Now()
	if !isClosed(rt.stop) {
		rt.applyAssignment()
	}
	var detections []DetectionReport
	if rt.mon.rule != nil {
		c := &rt.mon.rule.Content
		res := ResolveRule(c)
		if c.ExecEnabled() || c.DeniedEnabled() || c.ReadEnabled() || c.NetworkEnabled() {
			detections = append(detections, rt.collectAudit(c, res.UserByID)...)
		}
		if c.WritableEnabled() || c.ExecFilesEnabled() {
			detections = append(detections, rt.scanFiles(c, res, cycleStart)...)
		}
	}
	rt.report(detections)
	rt.pruneCommandExecutions()
}

// applyAssignment applies what the last sync assigned: it removes the audit
// rules when no rule is assigned, restores the last successful result when
// the running version is assigned again (after a newer one failed), and
// applies a new version unless its application failed before. The first
// step of the agent applies the stored rule again when no sync brought an
// assignment yet or the assignment is the stored rule.
func (rt *AgentRuntime) applyAssignment() {
	assigned, known := rt.link.assignment()
	if !rt.mon.started {
		rt.mon.started = true
		if r := rt.mon.rule; r != nil && (!known || (assigned != nil && assigned.ID == r.ID && assigned.Version == r.Version)) {
			rt.ReapplyStored()
			return
		}
	}
	if !known {
		return
	}
	switch {
	case assigned == nil:
		rt.removeRule()
	case rt.mon.rule != nil && assigned.ID == rt.mon.rule.ID && assigned.Version == rt.mon.rule.Version:
		// The running version is assigned again (for example after a newer
		// version failed): report its successful result again.
		rt.mon.failedRule = ""
		if rt.mon.lastApply != nil && rt.mon.lastApply.Status == "error" && rt.mon.lastOK != nil {
			rt.setApply(rt.mon.lastOK)
		}
	case ruleKey(assigned) != rt.mon.failedRule:
		rt.applyRule(assigned, false)
	}
}

func ruleKey(r *RuleAssignment) string {
	return fmt.Sprintf("%d:%d", r.ID, r.Version)
}

// applyRule validates and applies a rule. On failure the previous rule stays
// in effect and the error is reported.
func (rt *AgentRuntime) applyRule(ra *RuleAssignment, reapply bool) {
	now := time.Now().Unix()
	result := &ApplyResult{RuleID: ra.ID, Version: ra.Version, At: now, Status: "ok"}
	if ps := ra.Content.Problems(); len(ps) > 0 {
		result.Status = "error"
		result.setMessage(M("apply.validation_failed", "problems", firstProblems(ps, 5)))
		rt.mon.failedRule = ruleKey(ra)
		rt.setApply(result)
		rt.logf("rule %q version %d rejected: %s", ra.Name, ra.Version, result.Message)
		return
	}
	res := ResolveRule(&ra.Content)
	lines := GenerateAuditRules(res)
	applyWarnings, err := ApplyAuditRules(lines, func(g AuditGlobals) error { return recordAuditGlobals(rt.DB, g) })
	if err != nil {
		result.Status = "error"
		result.setMessage(AsMsg(err))
		rt.mon.failedRule = ruleKey(ra)
		rt.setApply(result)
		rt.logf("rule %q version %d not applied: %v", ra.Name, ra.Version, err)
		return
	}
	rt.mon.failedRule = ""
	result.setWarnings(append(res.Warnings, applyWarnings...))
	if len(result.Warnings) > 0 {
		result.Status = "warning"
		result.setMessage(M("apply.applied_with_warnings", "count", len(result.Warnings)))
	} else {
		result.setMessage(M("apply.applied", "count", len(lines)))
	}
	rt.noteScanConfig(ra.Content.Writable.Base + "|" + fmt.Sprint(ra.Content.Writable.AllowedPaths))
	b, _ := json.Marshal(ra)
	if err := rt.DB.Write(func(tx models.Writer) error { return models.SetLocalState(tx, stateRule, string(b)) }); err != nil {
		rt.logf("failed to store the applied rule: %v", err)
	}
	rt.mon.rule = ra
	rt.mon.lastOK = result
	rt.setApply(result)
	if !reapply {
		rt.logf("rule %q version %d applied: %s", ra.Name, ra.Version, result.Message)
	}
}

// ApplyStatusNone is the application result of an agent without a rule.
const ApplyStatusNone = "none"

// holdsRule reports whether a stored rule or application result says that
// audit rules may be loaded (a failed first application or "agent init
// --overwrite" leave only a result).
func holdsRule(rule *RuleAssignment, apply *ApplyResult) bool {
	return rule != nil || (apply != nil && apply.Status != ApplyStatusNone)
}

// removeRule takes away what the agent applied when the central assigns no
// rule: the loaded audit rules with this system's key and the rules file.
// The stored rule and application result are cleared, and the result
// "none" is reported so that the central shows that nothing is applied. On
// failure the rule stays in effect, the error is reported and the next
// check tries again.
func (rt *AgentRuntime) removeRule() {
	restore := storedAuditGlobals(rt.DB.Read())
	if !holdsRule(rt.mon.rule, rt.mon.lastApply) && restore == nil {
		return
	}
	remove := rt.removeAudit
	if remove == nil {
		remove = RemoveAuditRules
	}
	now := time.Now().Unix()
	if err := remove(restore); err != nil {
		failed := &ApplyResult{Status: "error", At: now}
		failed.setMessage(M("apply.none_remove_failed", "cause", AsMsg(err)))
		if rt.mon.lastApply == nil || rt.mon.lastApply.Status != "error" || rt.mon.lastApply.Message != failed.Message {
			rt.setApply(failed)
			rt.logf("%s", failed.Message)
		}
		return
	}
	// The audit rules were removed by an external program, so the records
	// are cleared in a transaction of their own after it (by key: nothing
	// read before decides what is deleted).
	if err := rt.DB.Write(func(tx models.Writer) error {
		return models.DeleteLocalState(tx, stateRule, stateApply, stateAuditGlobals)
	}); err != nil {
		rt.logf("failed to clear the stored rule: %v", err)
	}
	rt.mon.rule, rt.mon.lastOK, rt.mon.failedRule = nil, nil, ""
	rt.mon.lastApply = &ApplyResult{Status: ApplyStatusNone, At: now}
	rt.mon.lastApply.setMessage(M("apply.none_removed"))
	rt.logf("no rule is assigned: the audit rules with key %s and %s were removed%s", AuditKey, AuditRulesFile, restoredNote(restore))
}

// restoredNote describes the global audit settings set back by a removal
// ("" when nothing was recorded), for the log line of the removal.
func restoredNote(g *AuditGlobals) string {
	if g == nil {
		return ""
	}
	return fmt.Sprintf("; the global audit settings were set back to backlog_limit %d, backlog_wait_time %d, failure %d", g.BacklogLimit, g.BacklogWaitTime, g.Failure)
}

// recordAuditGlobals stores the global audit settings before the first
// application of audit rules. An existing record is kept: it holds the
// values from before this system changed them (an unreadable one is
// replaced). The check for a record and the write are one transaction.
func recordAuditGlobals(db *DB, g AuditGlobals) error {
	b, err := json.Marshal(g)
	if err != nil {
		return err
	}
	return db.Write(func(tx models.Writer) error {
		if storedAuditGlobals(tx) != nil {
			return nil
		}
		return models.SetLocalState(tx, stateAuditGlobals, string(b))
	})
}

// storedAuditGlobals returns the recorded global audit settings, or nil when
// there is no (readable) record.
func storedAuditGlobals(db models.Reader) *AuditGlobals {
	v := models.GetLocalState(db, stateAuditGlobals)
	if v == "" {
		return nil
	}
	var g AuditGlobals
	if json.Unmarshal([]byte(v), &g) != nil {
		return nil
	}
	return &g
}

// ClearStoredRule removes the audit rules and the rules file when the agent
// database db records a rule, an application result or the global audit
// settings before the first application, sets those settings back, and then
// clears the records. It reports whether anything was removed; without a
// record nothing is touched. The central uses it for the built-in agent
// while it is disabled.
//
// The records are read on the read side: an external program removes the
// audit rules between the read and the write, and the write deletes by key.
func ClearStoredRule(db *DB) (bool, error) {
	rd := db.Read()
	var rule *RuleAssignment
	if v := models.GetLocalState(rd, stateRule); v != "" {
		rule = &RuleAssignment{}
	}
	var apply *ApplyResult
	if v := models.GetLocalState(rd, stateApply); v != "" {
		var a ApplyResult
		if json.Unmarshal([]byte(v), &a) == nil {
			apply = &a
		} else {
			apply = &ApplyResult{}
		}
	}
	restore := storedAuditGlobals(rd)
	if !holdsRule(rule, apply) && restore == nil {
		return false, nil
	}
	if err := RemoveAuditRules(restore); err != nil {
		return false, err
	}
	return true, db.Write(func(tx models.Writer) error {
		return models.DeleteLocalState(tx, stateRule, stateApply, stateAuditGlobals)
	})
}

// resetStoredRule is used by "agent init --overwrite": the stored rule and
// the check interval stored from the last sync are cleared, and when audit
// rules may be loaded the application result is replaced by a pending one,
// so that the next sync decides (it applies the assigned rule, or removes
// the audit rules when none is assigned). It runs inside the write
// transaction tx, which reads what it decides on.
func resetStoredRule(tx models.Writer) error {
	if err := models.DeleteLocalState(tx, stateCheckInterval); err != nil {
		return err
	}
	var apply *ApplyResult
	if v := models.GetLocalState(tx, stateApply); v != "" {
		apply = &ApplyResult{}
		_ = json.Unmarshal([]byte(v), apply)
	}
	var rule *RuleAssignment
	if models.GetLocalState(tx, stateRule) != "" {
		rule = &RuleAssignment{}
	}
	if !holdsRule(rule, apply) {
		return nil
	}
	pending := &ApplyResult{Status: "pending", At: time.Now().Unix()}
	pending.setMessage(M("apply.cleared_by_init"))
	b, err := json.Marshal(pending)
	if err != nil {
		return err
	}
	if err := models.DeleteLocalState(tx, stateRule); err != nil {
		return err
	}
	return models.SetLocalState(tx, stateApply, string(b))
}

// firstProblems returns at most n problems followed by "and N more" when
// there are more (a list of messages: joined with "; " in English).
func firstProblems(ps []Problem, n int) []any {
	out := make([]any, 0, min(len(ps), n)+1)
	for i, p := range ps {
		if i == n {
			out = append(out, M("apply.and_more", "count", len(ps)-n))
			break
		}
		out = append(out, p)
	}
	return out
}

func (rt *AgentRuntime) setApply(r *ApplyResult) {
	rt.mon.lastApply = r
	if b, err := json.Marshal(r); err == nil {
		_ = rt.DB.Write(func(tx models.Writer) error { return models.SetLocalState(tx, stateApply, string(b)) })
	}
}

// collectAudit reads the audit log from the stored position on. The position
// is read on the read side and written after the audit tool has run: only
// one monitoring step runs at a time, so nothing else moves it between.
// After the stop the audit log is not read; a stop while the tool runs ends
// it. In both cases the position stays where it is.
func (rt *AgentRuntime) collectAudit(c *RuleContent, users map[int]string) []DetectionReport {
	if isClosed(rt.stop) {
		return nil
	}
	var last AuditEventID
	_ = json.Unmarshal([]byte(models.GetLocalState(rt.DB.Read(), stateLastAuditEvent)), &last)
	events, next, err := CollectAuditEvents(rt.stop, last)
	if errors.Is(err, ErrToolStopped) {
		rt.logf("reading the audit log was ended by the stop; the next start reads from the same position")
		return nil
	}
	if err != nil {
		rt.logf("reading the audit log failed: %v", err)
		return nil
	}
	if b, err := json.Marshal(next); err == nil {
		_ = rt.DB.Write(func(tx models.Writer) error { return models.SetLocalState(tx, stateLastAuditEvent, string(b)) })
	}
	return AggregateAuditEvents(events, c, users)
}

// noteScanConfig records the scan configuration (base directory and allowed
// paths) of the applied rule. When it changed, a fresh full scan is needed:
// the roots known so far are remembered, so that the next full scan treats
// every other root as new (see scanFiles), and a split writable scan of the
// previous configuration is abandoned. The known roots stay in scan_roots
// until the first full scan of the new configuration completes, so that the
// executed-file scan keeps walking them meanwhile.
func (rt *AgentRuntime) noteScanConfig(sig string) {
	// The configuration is unchanged on almost every application: checked on
	// the read side first, and again inside the transaction that changes it.
	if models.GetLocalState(rt.DB.Read(), stateScanSignature) == sig {
		return
	}
	_ = rt.DB.Write(func(tx models.Writer) error {
		if models.GetLocalState(tx, stateScanSignature) == sig {
			return nil
		}
		if models.GetLocalState(tx, stateScanPrevRoots) == "" {
			if old, err := models.ListScanRoots(tx); err == nil {
				if b, err := json.Marshal(old); err == nil {
					if err := models.SetLocalState(tx, stateScanPrevRoots, string(b)); err != nil {
						return err
					}
				}
			}
		}
		if err := models.SetLocalState(tx, stateScanSignature, sig); err != nil {
			return err
		}
		if err := models.SetLocalState(tx, stateLastFullScan, "0"); err != nil {
			return err
		}
		return models.DeleteLocalState(tx, stateWritableResume, stateWritablePass)
	})
}

// scanEntryLimit is the number of entries one walk visits in one check.
func (rt *AgentRuntime) scanEntryLimit() int {
	if rt.scanLimit > 0 {
		return rt.scanLimit
	}
	return MaxScanEntries
}

// writablePass is the state of a writable scan split over several checks.
type writablePass struct {
	StartedAt int64    `json:"started_at"`
	Roots     []string `json:"roots"`
}

// sinceSegment is a part of the walk order of the executed-file scan, from
// the path From (empty: the beginning) to the next segment, last scanned at
// Since (status changes after it are new).
type sinceSegment struct {
	From  string `json:"from"`
	Since int64  `json:"since"`
}

// sinceAt returns the since time of the segment containing p.
func sinceAt(segs []sinceSegment, p string) int64 {
	var v int64
	for _, g := range segs {
		if g.From != "" && walkLess(p, g.From) {
			break
		}
		v = g.Since
	}
	return v
}

// markScanned sets the since time of the range [from, to) to t (from empty:
// the beginning, to empty: the end). Segments after to keep their times;
// adjacent segments with the same time are merged.
func markScanned(segs []sinceSegment, from, to string, t int64) []sinceSegment {
	var out []sinceSegment
	if from != "" {
		for _, g := range segs {
			if g.From == "" || walkLess(g.From, from) {
				out = append(out, g)
			}
		}
		if len(out) == 0 {
			out = append(out, sinceSegment{From: "", Since: 0})
		}
	}
	out = append(out, sinceSegment{From: from, Since: t})
	if to != "" {
		out = append(out, sinceSegment{From: to, Since: sinceAt(segs, to)})
		for _, g := range segs {
			if g.From != "" && walkLess(to, g.From) {
				out = append(out, g)
			}
		}
	}
	merged := out[:1]
	for _, g := range out[1:] {
		if g.Since != merged[len(merged)-1].Since {
			merged = append(merged, g)
		}
	}
	return merged
}

// loadJSONState reads the state key into v through db (the read side or a
// write transaction).
func loadJSONState(db models.Reader, key string, v any) {
	if s := models.GetLocalState(db, key); s != "" {
		_ = json.Unmarshal([]byte(s), v)
	}
}

// saveJSONState stores v as JSON under the state key inside the write
// transaction tx.
func saveJSONState(tx models.Writer, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return models.SetLocalState(tx, key, string(b))
}

// incompleteScans returns the walks whose pass is split over several checks
// because they reached the entry limit.
func (rt *AgentRuntime) incompleteScans() []IncompleteScan {
	var out []IncompleteScan
	rd := rt.DB.Read()
	if at := models.GetLocalState(rd, stateWritableResume); at != "" {
		var pass writablePass
		loadJSONState(rd, stateWritablePass, &pass)
		out = append(out, IncompleteScan{Kind: ScanKindWritable, ResumeAt: at, Limit: rt.scanEntryLimit(), StartedAt: pass.StartedAt})
	}
	if at := models.GetLocalState(rd, stateExecResume); at != "" {
		started, _ := strconv.ParseInt(models.GetLocalState(rd, stateExecPassStart), 10, 64)
		out = append(out, IncompleteScan{Kind: ScanKindExecFiles, ResumeAt: at, Limit: rt.scanEntryLimit(), StartedAt: started})
	}
	return out
}

// ctimeMargin is how many seconds before the start of a check the since
// times of the file scans end. The kernel stamps a ctime from its coarse
// clock, which lags time.Now by up to one tick: a file changed a few
// milliseconds after a check started in a new second can carry a ctime of
// the second before. One second would therefore miss it; two cover the lag.
const ctimeMargin = 2

// scanFiles runs the file scans of one check. Each walk visits at most the
// entry limit; a walk that reaches it continues at the same place in the
// next check, and starts at the beginning again once it reached the end
// (see chapter 10 of the system specification in Documents).
//
// The state is read on the read side before the walks: a walk lies between
// the read and the write, and only one monitoring step runs at a time. The
// findings of a walk and the state it leaves are written together in one
// transaction after the walk, which also reads what decides them (whether a
// finding is known, the roots known so far).
func (rt *AgentRuntime) scanFiles(c *RuleContent, res *ResolvedRule, cycleStart time.Time) []DetectionReport {
	var out []DetectionReport
	now := cycleStart.Unix()
	limit := rt.scanEntryLimit()
	_, users := ReadPasswd()
	groups := ReadGroups()
	uids := map[int]bool{}
	for _, u := range res.UIDs {
		uids[u] = true
	}
	rd := rt.DB.Read()
	lastFull, _ := strconv.ParseInt(models.GetLocalState(rd, stateLastFullScan), 10, 64)
	// Since times of allowed roots that the executed-file scan has not
	// walked to the end yet: roots found for the first time by a full scan
	// (files created in them before they became roots are older than the
	// since time of their part, so they are scanned from the previous full
	// scan) and roots that appear after the scan configuration changed
	// (scanned like the roots of a first scan: the files already in them are
	// reported).
	//
	// What a walk's transaction decides (its findings to report and its
	// changes of these times) is computed on copies and taken over only
	// when the transaction committed: nothing of a rolled back one is
	// reported or saved.
	rootSince := map[string]int64{}
	loadJSONState(rd, stateRootSince, &rootSince)
	rootSinceChanged := false
	// touchFindings records the fingerprints of a walk inside tx and returns
	// the reports of those not known before.
	touchFindings := func(tx models.Writer, found []FileFinding) ([]DetectionReport, error) {
		var fresh []DetectionReport
		for _, f := range found {
			fp := f.Fingerprint()
			if !models.FindingExists(tx, fp) {
				fresh = append(fresh, f.ToReport(users, groups, now))
			}
			if err := models.TouchFinding(tx, fp, now); err != nil {
				return nil, err
			}
		}
		return fresh, nil
	}
	wResume := models.GetLocalState(rd, stateWritableResume)
	if wResume != "" || now-lastFull >= int64(c.ScanInterval()) {
		var pass writablePass
		if wResume != "" {
			loadJSONState(rd, stateWritablePass, &pass)
			rt.logf("writable scan of %s continues at %s", c.Writable.Base, wResume)
		} else {
			pass = writablePass{StartedAt: now}
		}
		wr, err := ScanWritableFrom(c.Writable.Base, c.Writable.AllowedPaths, uids, c.Writable.Category, wResume, limit)
		if err != nil {
			rt.logf("writable scan of %s failed: %v", c.Writable.Base, err)
		} else {
			var fresh []DetectionReport
			var newSince map[string]int64
			newPass := pass
			err := rt.DB.Write(func(tx models.Writer) error {
				fresh = nil
				newSince = maps.Clone(rootSince)
				setRootSince := func(r string, v int64) {
					if old, ok := newSince[r]; !ok || v < old {
						newSince[r] = v
					}
				}
				pass := newPass
				if c.WritableEnabled() {
					var err error
					if fresh, err = touchFindings(tx, wr.Findings); err != nil {
						return err
					}
				}
				known := map[string]bool{}
				if old, err := models.ListScanRoots(tx); err == nil {
					for _, r := range old {
						known[r] = true
					}
				}
				if prev := models.GetLocalState(tx, stateScanPrevRoots); prev != "" {
					var old []string
					_ = json.Unmarshal([]byte(prev), &old)
					before := map[string]bool{}
					for _, r := range old {
						before[r] = true
					}
					for _, r := range wr.AllowedRoots {
						if !before[r] {
							setRootSince(r, 0)
						}
					}
				} else {
					// From ctimeMargin seconds before the start of the previous
					// full pass: a root made in that same second after the pass
					// had walked past its place has files of that second, or of
					// the second before (as the since times of the
					// executed-file scan).
					rootFrom := lastFull
					if rootFrom > 0 {
						rootFrom -= ctimeMargin
					}
					for _, r := range wr.AllowedRoots {
						if !known[r] {
							setRootSince(r, rootFrom)
						}
					}
				}
				seen := map[string]bool{}
				var passRoots []string
				for _, r := range append(pass.Roots, wr.AllowedRoots...) {
					if !seen[r] {
						seen[r] = true
						passRoots = append(passRoots, r)
					}
				}
				pass.Roots = passRoots
				newPass = pass
				if wr.Truncated {
					// The roots of the previous pass stay until this one is complete.
					all := passRoots
					for r := range known {
						if !seen[r] {
							all = append(all, r)
						}
					}
					if err := models.ReplaceScanRoots(tx, all); err != nil {
						return err
					}
					if err := saveJSONState(tx, stateWritablePass, &pass); err != nil {
						return err
					}
					return models.SetLocalState(tx, stateWritableResume, wr.ResumeAt)
				}
				if err := models.ReplaceScanRoots(tx, passRoots); err != nil {
					return err
				}
				if c.WritableEnabled() {
					if err := models.PruneFindings(tx, TypeWritable+"|", pass.StartedAt); err != nil {
						return err
					}
				}
				if err := models.SetLocalState(tx, stateScanPrevRoots, ""); err != nil {
					return err
				}
				if err := models.SetLocalState(tx, stateLastFullScan, strconv.FormatInt(pass.StartedAt, 10)); err != nil {
					return err
				}
				return models.DeleteLocalState(tx, stateWritableResume, stateWritablePass)
			})
			switch {
			case err != nil:
				// Nothing of the walk was stored: the next check walks the
				// same part again.
				rt.logf("writable scan of %s: storing the result failed: %v", c.Writable.Base, err)
			case wr.Truncated:
				rt.logf("writable scan of %s stopped after %d entries; the next check continues at %s", c.Writable.Base, limit, wr.ResumeAt)
			case wResume != "":
				rt.logf("writable scan of %s completed the pass started at %s", c.Writable.Base,
					time.Unix(newPass.StartedAt, 0).Local().Format("2006-01-02 15:04:05 MST"))
			}
			if err == nil {
				out = append(out, fresh...)
				rootSince, rootSinceChanged = newSince, true
			}
		}
	}
	if c.ExecFilesEnabled() {
		// Read after the writable scan, which may have changed the roots.
		roots, err := models.ListScanRoots(rd)
		if err == nil {
			var segs []sinceSegment
			loadJSONState(rd, stateExecSince, &segs)
			if len(segs) == 0 {
				// Nothing scanned yet: every part is new.
				segs = []sinceSegment{{From: "", Since: 0}}
			}
			eResume := models.GetLocalState(rd, stateExecResume)
			since := func(root, p string) int64 {
				v := sinceAt(segs, p)
				if o, ok := rootSince[root]; ok && o < v {
					v = o
				}
				return v
			}
			if eResume != "" {
				rt.logf("executed-file scan continues at %s", eResume)
			}
			found, skipped, stop, err := ScanExecFilesFrom(c.Writable.Base, roots, c, since, eResume, limit)
			if err != nil {
				rt.logf("executed-file scan failed: %v", err)
			} else {
				// The walked part is scanned up to ctimeMargin seconds before
				// the cycle start: files whose status changed at or after it
				// are picked up again next time (duplicates are filtered by
				// the findings).
				segs = markScanned(segs, eResume, stop, now-ctimeMargin)
				// A root walked to its end in this check needs no own since
				// time any more.
				newSince := maps.Clone(rootSince)
				for r := range newSince {
					if (eResume == "" || !entirelyBefore(r, eResume)) && (stop == "" || entirelyBefore(r, stop)) {
						delete(newSince, r)
					}
				}
				var fresh []DetectionReport
				err := rt.DB.Write(func(tx models.Writer) error {
					var err error
					if fresh, err = touchFindings(tx, found); err != nil {
						return err
					}
					// Files of a first scan that are not reported are
					// remembered, so the next scan (which also looks at the
					// second of this one) does not report them either.
					for _, f := range skipped {
						if err := models.TouchFinding(tx, f.Fingerprint(), now); err != nil {
							return err
						}
					}
					if err := saveJSONState(tx, stateExecSince, segs); err != nil {
						return err
					}
					if stop != "" {
						if eResume == "" {
							if err := models.SetLocalState(tx, stateExecPassStart, strconv.FormatInt(now, 10)); err != nil {
								return err
							}
						}
						if err := models.SetLocalState(tx, stateExecResume, stop); err != nil {
							return err
						}
					} else if eResume != "" {
						if err := models.DeleteLocalState(tx, stateExecResume, stateExecPassStart); err != nil {
							return err
						}
					}
					return models.PruneFindings(tx, TypeExecFile+"|", now-7*24*3600)
				})
				switch {
				case err != nil:
					// Nothing of the walk was stored: the next check walks
					// the same part again.
					rt.logf("executed-file scan: storing the result failed: %v", err)
				case stop != "":
					rt.logf("executed-file scan stopped after %d entries; the next check continues at %s", limit, stop)
				case eResume != "":
					rt.logf("executed-file scan completed its pass")
				}
				if err == nil {
					out = append(out, fresh...)
					rootSince, rootSinceChanged = newSince, true
				}
			}
		}
	}
	if rootSinceChanged {
		if err := rt.DB.Write(func(tx models.Writer) error { return saveJSONState(tx, stateRootSince, rootSince) }); err != nil {
			rt.logf("file scans: storing the since times of the roots failed: %v", err)
		}
	}
	return out
}

// report queues the periodic report of a monitoring step; the next round
// sends it.
func (rt *AgentRuntime) report(detections []DetectionReport) {
	// The directory structure is reported only for the scan origin of the
	// applied rule; there is no default origin.
	in := HostInfoInput{LastApply: rt.mon.lastApply, Workers: rt.workers, TrustedCAPEMs: rt.link.trustedCAs()}
	if rt.mon.rule != nil {
		in.Rule = &RuleRef{ID: rt.mon.rule.ID, Version: rt.mon.rule.Version}
		in.ScanBase = rt.mon.rule.Content.Writable.Base
		in.IncompleteScans = rt.incompleteScans()
	}
	in.Stop = rt.stop
	// After the stop, or when the stop cut its collection, the report goes
	// without host information (its audit tools would not run); the central
	// keeps the host information it has.
	var hi *HostInfo
	if !isClosed(rt.stop) {
		hi, _ = CollectHostInfo(in)
	}

	// The dropped count is read, reported in the queued report and reset in
	// the outbox's one transaction, so that a count is neither lost nor
	// reported twice.
	req := &ReportRequest{CreatedAt: time.Now().Unix(), Detections: detections, HostInfo: hi, Apply: rt.mon.lastApply}
	dropped, err := rt.out.queuePeriodic(req)
	switch {
	case err != nil:
		rt.logf("failed to queue the report: %v", err)
	case dropped > 0:
		rt.logf("report storage limit reached: dropped %d detection(s)", dropped)
	}
}
