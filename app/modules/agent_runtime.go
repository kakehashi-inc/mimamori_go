package modules

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"sync"
	"time"

	"mimamori/app/models"
)

// AgentTransport delivers the agent's requests to the central. The remote
// agent uses HTTPS; the central's built-in agent hands them over in process.
type AgentTransport interface {
	Sync(req *SyncRequest) (*SyncResponse, error)
	Report(req *ReportRequest) error
}

// Local state keys of the agent database.
const (
	stateRule           = "rule"
	stateApply          = "apply"
	stateExecWatermark  = "exec_watermark"
	stateLastFullScan   = "last_full_scan"
	stateScanSignature  = "scan_signature"
	stateScanPrevRoots  = "scan_prev_roots"
	stateDroppedPending = "dropped_pending"
	stateLastAuditEvent = "last_audit_event"
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

// AgentRuntime performs the work of an agent: the monitoring step (syncing,
// applying rules, reading the audit log, scanning files and reporting) and
// the runs of command sets, on as many flows as its worker count (with one
// worker the calling flow only). The scheduler (agent_scheduler.go) decides
// what runs next; its file comment says who owns which state.
//
// DB is the handle of the agent's local database. What the runtime reads it
// reads through DB.Read; every change is a DB.Write, and what decides a
// change is read inside that Write. A request to the central, an external
// program, a file walk or a running command never happens inside a Write.
type AgentRuntime struct {
	DB     *DB
	Logger *log.Logger

	// mon is the state of the monitoring step, touched only by the flow that
	// runs the step.
	mon monitorState
	// sched decides what runs next.
	sched *agentScheduler
	// out is the outbox, the only owner of the queued reports.
	out *outbox

	// Fixed before Run starts a worker, then only read. workers is the number
	// of units processed at the same time (SetWorkers; 1 unless set); stop is
	// the channel given to Run, closed when the agent stops.
	workers int
	stop    <-chan struct{}
	// clock, loc and cmdKillGrace replace the time, the host's time zone and
	// the grace before SIGKILL in tests.
	clock        func() time.Time
	loc          *time.Location
	cmdKillGrace time.Duration
	// scanLimit replaces MaxScanEntries and removeAudit replaces
	// RemoveAuditRules in tests.
	scanLimit   int
	removeAudit func(restore *AuditGlobals) error
}

// monitorState is what only the monitoring step uses.
type monitorState struct {
	transport AgentTransport
	config    *AgentConfig // nil for the built-in agent
	// onConfigSaved is called with the configuration after a change from the
	// central (renewed token, name, expiry, CAs) was saved, so that the
	// transport uses the saved token and CAs from the next request on.
	onConfigSaved func(cfg *AgentConfig)

	defaultInterval int
	rule            *RuleAssignment
	lastApply       *ApplyResult
	// failedRule is the rule version whose application failed. It is not
	// retried every cycle (each attempt replaces the loaded rules); a new
	// version or a restart of the agent tries again.
	failedRule string
	// lastOK is the result of the last successful application, restored when
	// the central assigns the running version again after a failed one.
	lastOK *ApplyResult
}

// interval is the check interval: the assigned rule's, else the central's
// default from the last sync.
func (m *monitorState) interval() time.Duration {
	if m.rule != nil {
		return time.Duration(m.rule.Content.CheckInterval()) * time.Second
	}
	return time.Duration(m.defaultInterval) * time.Second
}

// NewAgentRuntime loads the stored state. tr is the transport to the central.
func NewAgentRuntime(db *DB, tr AgentTransport, logger *log.Logger) *AgentRuntime {
	rt := &AgentRuntime{DB: db, Logger: logger, workers: 1,
		mon: monitorState{transport: tr, defaultInterval: DefaultCheckIntervalSec}}
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
	rt.sched = newAgentScheduler(db, rt.now, rt.location, rt.mon.interval())
	rt.loadCommandDefinitions()
	return rt
}

// SetWorkers sets the number of units processed at the same time (1 to
// MaxAgentWorkers, see LoadAgentWorkers). It is called before Run.
func (rt *AgentRuntime) SetWorkers(n int) {
	rt.workers = max(1, min(n, MaxAgentWorkers))
}

func (rt *AgentRuntime) logf(format string, a ...any) {
	if rt.Logger != nil {
		rt.Logger.Printf(format, a...)
	}
}

// ReapplyStored re-applies the stored rule at start, because a reboot or an
// auditd restart reloads the OS defaults.
func (rt *AgentRuntime) ReapplyStored() {
	if rt.mon.rule == nil {
		return
	}
	rt.logf("re-applying rule %q version %d", rt.mon.rule.Name, rt.mon.rule.Version)
	rt.applyRule(rt.mon.rule, true)
}

// Run processes the agent's units until stop is closed (a nil stop never
// is): the calling flow is the first worker, and workers-1 flows more are
// started. Each worker asks the scheduler for a unit (the monitoring step
// when due, else the oldest manual request whose set does not run, else the
// due scheduled set), runs it and asks again; with nothing to run it waits
// until the next monitoring step or the next scheduled time. While a command
// runs, its wait loop runs the monitoring step when it is due and no other
// worker is idle.
//
// After the stop no unit starts; a running monitoring step completes (see
// monitorStep), and a running command is ended (SIGTERM, grace, SIGKILL) and
// its run still reported. Run returns when every worker has returned, after
// one attempt to send the queued reports when a run ended after the stop.
// While it runs, the state key running_workers holds the worker count in
// use (for agent status and agent set-workers).
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
	if !isClosed(stop) {
		rt.ReapplyStored()
	}
	rt.recoverInterruptedRuns()
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
	if rt.sched.runsStoppedByStop() {
		// A run was closed because of the stop: one attempt to send its
		// reports, so that the central does not show it running until the
		// next start (what fails stays queued). Every worker has returned, so
		// this is the only flush.
		rt.flushOutbox()
	}
}

// work is one worker: it runs the units the scheduler gives it until the
// stop.
func (rt *AgentRuntime) work() {
	for {
		u, acks := rt.sched.next()
		rt.queueAcks(acks)
		switch {
		case u == nil && len(acks) == 0:
			return
		case u == nil:
		case u.monitor:
			rt.runMonitorStep()
		default:
			started := rt.runCommandJob(u.job)
			rt.sched.doneRun(u.job, started)
		}
	}
}

// runMonitorStep runs the monitoring step the scheduler gave to this flow
// and tells the scheduler that it ended.
func (rt *AgentRuntime) runMonitorStep() {
	rt.monitorStep()
	rt.sched.doneMonitor(rt.mon.interval())
}

// monitorHere is called from the wait loop of a running command: it runs the
// monitoring step on this flow when the scheduler gives it to this worker.
func (rt *AgentRuntime) monitorHere() {
	if rt.sched.inlineMonitor() {
		rt.runMonitorStep()
	}
}

// Cycle performs one monitoring step.
func (rt *AgentRuntime) Cycle() {
	rt.monitorStep()
}

// monitorStep performs one periodic processing: sync, audit collection, file
// scans and the report. It is also called from the wait loop of a running
// command. A stop does not interrupt it: the step completes and saves its
// state, with these exceptions. A tool the step waits for when the stop
// comes is ended (RunTool) and its part fails as a failure of that tool
// does. After the stop the step starts no tool: the audit log is not read
// (the position stays), a rule application or removal that has not begun is
// not begun, and the report is queued without host information. An
// application or removal that has begun runs to its end.
func (rt *AgentRuntime) monitorStep() {
	cycleStart := time.Now()
	rt.sync()

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

func (rt *AgentRuntime) sync() {
	running, err := rt.runningRequests()
	if err != nil {
		// Without the list the central could take a running request for
		// ended; the sync is left out and the step goes on as after a failed
		// sync.
		rt.logf("sync: reading the running command set runs failed: %v", err)
		return
	}
	req := &SyncRequest{AgentVersion: AppVersion, LastApply: rt.mon.lastApply, CommandSig: rt.sched.signature(),
		RunningRequests: running}
	if rt.mon.rule != nil {
		req.RuleID, req.RuleVersion = rt.mon.rule.ID, rt.mon.rule.Version
	}
	if rt.mon.config != nil {
		req.CAFingerprints = CAFingerprintsOf(rt.mon.config.CAPEMs)
	}
	resp, err := rt.mon.transport.Sync(req)
	if err != nil {
		rt.logConnectionError("sync", err)
		return
	}
	if resp.DefaultIntervalSec > 0 {
		rt.mon.defaultInterval = resp.DefaultIntervalSec
	}
	if rt.mon.config != nil {
		rt.saveConfigUpdates(resp)
	}
	// Definitions and requests go to the scheduler; a running set keeps the
	// definition it started with.
	if resp.CommandSets != nil {
		rt.storeCommandDefinitions(resp.CommandSets)
	}
	rt.queueAcks(rt.sched.receiveRequests(resp.CommandRequests))
	if isClosed(rt.stop) {
		// After the stop no rule is applied or removed (both run tools); the
		// next start syncs again.
		return
	}
	switch {
	case resp.Rule == nil:
		rt.removeRule()
	case rt.mon.rule != nil && resp.Rule.ID == rt.mon.rule.ID && resp.Rule.Version == rt.mon.rule.Version:
		// The running version is assigned again (for example after a newer
		// version failed): report its successful result again.
		rt.mon.failedRule = ""
		if rt.mon.lastApply != nil && rt.mon.lastApply.Status == "error" && rt.mon.lastOK != nil {
			rt.setApply(rt.mon.lastOK)
		}
	case ruleKey(resp.Rule) != rt.mon.failedRule:
		resp.Rule.Content.Normalize()
		rt.applyRule(resp.Rule, false)
	}
}

// runningRequests returns the request IDs of the manual runs that were
// started and are not finished, from the local database (never nil).
func (rt *AgentRuntime) runningRequests() ([]int64, error) {
	runs, err := models.ListUnfinishedCommandExecutions(rt.DB.Read())
	if err != nil {
		return nil, err
	}
	ids := []int64{}
	for _, e := range runs {
		if e.RequestID > 0 {
			ids = append(ids, e.RequestID)
		}
	}
	return ids, nil
}

func (rt *AgentRuntime) saveConfigUpdates(resp *SyncResponse) {
	cfg := *rt.mon.config
	cfg.CAPEMs = append([]string(nil), rt.mon.config.CAPEMs...)
	changed := false
	if resp.TokenExpiresAt > 0 && resp.TokenExpiresAt != cfg.TokenExpiresAt {
		cfg.TokenExpiresAt = resp.TokenExpiresAt
		changed = true
	}
	if resp.AgentName != "" && resp.AgentName != cfg.TokenName {
		cfg.TokenName = resp.AgentName
		changed = true
	}
	renewed := false
	if resp.TokenRenewal != nil && resp.TokenRenewal.Token != "" && resp.TokenRenewal.Token != cfg.Token {
		cfg.Token = resp.TokenRenewal.Token
		cfg.TokenExpiresAt = resp.TokenRenewal.ExpiresAt
		changed, renewed = true, true
	}
	if cfg.MergeCAs(resp.CAPEMs, time.Now()) {
		changed = true
	}
	if !changed {
		return
	}
	if err := rt.DB.Write(cfg.Save); err != nil {
		// Keep using the old token; the central hands out the same new value
		// again on the next connection.
		rt.logf("failed to save configuration (token renewal or CA update postponed): %v", err)
		return
	}
	*rt.mon.config = cfg
	if renewed {
		rt.logf("token renewed; new expiry %s", time.Unix(cfg.TokenExpiresAt, 0).Local().Format("2006-01-02 15:04:05 MST"))
	}
	if rt.mon.onConfigSaved != nil {
		rt.mon.onConfigSaved(rt.mon.config)
	}
}

func (rt *AgentRuntime) logConnectionError(op string, err error) {
	var he *HTTPError
	switch {
	case errors.As(err, &he) && he.Status == 401:
		rt.logf("%s: the central rejected the token (expired, revoked, reissued or bound to another agent instance). Re-initialize the agent with a valid token", op)
	case IsSilenceCut(err):
		// The row of a cut report stays queued and is sent again at the next
		// monitoring step.
		rt.logf("%s: the central sent nothing for %d seconds: %v", op, int(IdleTimeout/time.Second), err)
	case IsCertificateError(err):
		rt.logf("%s: the central certificate is not signed by a trusted CA (%v). If the CA was rolled over while this agent was offline, re-initialize the agent", op, err)
	default:
		rt.logf("%s: cannot reach the central: %v", op, err)
	}
	if rt.mon.config != nil && rt.mon.config.TokenExpiresAt > 0 {
		left := time.Until(time.Unix(rt.mon.config.TokenExpiresAt, 0))
		if left < time.Duration(DefaultTokenRenewDays)*24*time.Hour {
			rt.logf("warning: the token expires at %s (in %s) and could not be renewed",
				time.Unix(rt.mon.config.TokenExpiresAt, 0).Local().Format("2006-01-02 15:04:05 MST"), left.Round(time.Minute))
		}
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

// resetStoredRule is used by "agent init --overwrite": the stored rule is
// cleared, and when audit rules may be loaded the application result is
// replaced by a pending one, so that the next sync decides (it applies the
// assigned rule, or removes the audit rules when none is assigned). It runs
// inside the write transaction tx, which reads what it decides on.
func resetStoredRule(tx models.Writer) error {
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
	rootSince := map[string]int64{}
	loadJSONState(rd, stateRootSince, &rootSince)
	setRootSince := func(r string, v int64) {
		if old, ok := rootSince[r]; !ok || v < old {
			rootSince[r] = v
		}
	}
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
			// As before the transaction, a failure to store the result is not
			// logged: the next check walks again.
			_ = rt.DB.Write(func(tx models.Writer) error {
				if c.WritableEnabled() {
					fresh, err := touchFindings(tx, wr.Findings)
					if err != nil {
						return err
					}
					out = append(out, fresh...)
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
					// From the second before the start of the previous full
					// pass: a root made in that same second after the pass had
					// walked past its place has files of that second (as the
					// since times of the executed-file scan, which end one
					// second before the check).
					rootFrom := lastFull
					if rootFrom > 0 {
						rootFrom--
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
			if wr.Truncated {
				rt.logf("writable scan of %s stopped after %d entries; the next check continues at %s", c.Writable.Base, limit, wr.ResumeAt)
			} else if wResume != "" {
				rt.logf("writable scan of %s completed the pass started at %s", c.Writable.Base,
					time.Unix(pass.StartedAt, 0).Local().Format("2006-01-02 15:04:05 MST"))
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
				// The watermark of earlier versions covers everything.
				wm, _ := strconv.ParseInt(models.GetLocalState(rd, stateExecWatermark), 10, 64)
				segs = []sinceSegment{{From: "", Since: wm}}
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
				// The walked part is scanned up to the second before the cycle
				// start: files whose status changed at or after it are picked
				// up again next time (duplicates are filtered by the findings).
				segs = markScanned(segs, eResume, stop, now-1)
				// A root walked to its end in this check needs no own since
				// time any more.
				for r := range rootSince {
					if (eResume == "" || !entirelyBefore(r, eResume)) && (stop == "" || entirelyBefore(r, stop)) {
						delete(rootSince, r)
					}
				}
				_ = rt.DB.Write(func(tx models.Writer) error {
					fresh, err := touchFindings(tx, found)
					if err != nil {
						return err
					}
					out = append(out, fresh...)
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
				if stop != "" {
					rt.logf("executed-file scan stopped after %d entries; the next check continues at %s", limit, stop)
				} else if eResume != "" {
					rt.logf("executed-file scan completed its pass")
				}
			}
		}
	}
	_ = rt.DB.Write(func(tx models.Writer) error { return saveJSONState(tx, stateRootSince, rootSince) })
	return out
}

func (rt *AgentRuntime) report(detections []DetectionReport) {
	// The directory structure is reported only for the scan origin of the
	// applied rule; there is no default origin.
	in := HostInfoInput{LastApply: rt.mon.lastApply, Workers: rt.workers}
	if rt.mon.rule != nil {
		in.Rule = &RuleRef{ID: rt.mon.rule.ID, Version: rt.mon.rule.Version}
		in.ScanBase = rt.mon.rule.Content.Writable.Base
		in.IncompleteScans = rt.incompleteScans()
	}
	if rt.mon.config != nil {
		in.TrustedCAPEMs = rt.mon.config.CAPEMs
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
	if err != nil {
		rt.logf("failed to queue the report: %v", err)
		return
	}
	if dropped > 0 {
		rt.logf("report storage limit reached: dropped %d detection(s)", dropped)
	}
	rt.flushOutbox()
}

// flushOutbox sends the queued reports (see outbox.flush); a failure is
// logged as a failed report.
func (rt *AgentRuntime) flushOutbox() {
	rt.out.flush(rt.mon.transport, func(err error) { rt.logConnectionError("report", err) })
}
