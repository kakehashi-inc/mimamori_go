package modules

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
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
)

// AgentRuntime performs the periodic processing of an agent. Everything runs
// sequentially in the calling flow: syncing, applying rules, reading the
// audit log, scanning files and reporting.
type AgentRuntime struct {
	DB        *sql.DB
	Transport AgentTransport
	Config    *AgentConfig // nil for the built-in agent
	Logger    *log.Logger
	// OnTokenRenewed is called after a renewed token was saved.
	OnTokenRenewed func(token string)

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

	// stopping is the stop check given to Run (also used while a command
	// runs); lastMonitor is the start of the last monitoring step.
	stopping    func() bool
	lastMonitor time.Time
	// Command sets: the stored definitions, the valid sets by ID, the manual
	// requests waiting to run (oldest first) and the requests answered in
	// this process.
	cmdDefs     *CommandDefinitions
	cmdValid    map[int64]*CommandSetDef
	cmdRequests []CommandRunRequest
	cmdAcked    map[int64]bool
	// clock, loc, cmdKillGrace and cmdOutboxLimit replace the time, the
	// host's time zone, the grace before SIGKILL and MaxOutboxCommandBytes
	// in tests.
	clock          func() time.Time
	loc            *time.Location
	cmdKillGrace   time.Duration
	cmdOutboxLimit int64
	// scanLimit replaces MaxScanEntries and removeAudit replaces
	// RemoveAuditRules in tests.
	scanLimit   int
	removeAudit func() error
}

// NewAgentRuntime loads the stored state.
func NewAgentRuntime(db *sql.DB, tr AgentTransport, logger *log.Logger) *AgentRuntime {
	rt := &AgentRuntime{DB: db, Transport: tr, Logger: logger,
		defaultInterval: DefaultCheckIntervalSec, cmdValid: map[int64]*CommandSetDef{}, cmdAcked: map[int64]bool{}}
	if v := models.GetLocalState(db, stateRule); v != "" {
		var r RuleAssignment
		if json.Unmarshal([]byte(v), &r) == nil {
			r.Content.Normalize()
			rt.rule = &r
		}
	}
	if v := models.GetLocalState(db, stateApply); v != "" {
		var a ApplyResult
		if json.Unmarshal([]byte(v), &a) == nil {
			rt.lastApply = &a
		}
	}
	rt.loadCommandDefinitions()
	return rt
}

func (rt *AgentRuntime) logf(format string, a ...any) {
	if rt.Logger != nil {
		rt.Logger.Printf(format, a...)
	}
}

// Interval returns the wait before the next cycle.
func (rt *AgentRuntime) Interval() time.Duration {
	if rt.rule != nil {
		return time.Duration(rt.rule.Content.CheckInterval()) * time.Second
	}
	return time.Duration(rt.defaultInterval) * time.Second
}

// ReapplyStored re-applies the stored rule at start, because a reboot or an
// auditd restart reloads the OS defaults.
func (rt *AgentRuntime) ReapplyStored() {
	if rt.rule == nil {
		return
	}
	rt.logf("re-applying rule %q version %d", rt.rule.Name, rt.rule.Version)
	rt.applyRule(rt.rule, true)
}

// Run loops until stopping returns true. wait sleeps for the given duration
// and returns false when a stop was requested meanwhile. The stop request is
// checked between steps of a cycle; a running cycle always completes and
// saves its state before returning.
//
// Each pass runs the monitoring step when the check interval has passed,
// then the command sets that are due, one at a time (manual requests first,
// then schedules by due time), and waits until the next monitoring step or
// the next scheduled time, whichever comes first. While a command runs, its
// wait loop runs the monitoring step when it is due; a stop ends the running
// command (SIGTERM, grace, SIGKILL) and its run is still reported: one
// attempt to send the queued reports is made before returning.
func (rt *AgentRuntime) Run(wait func(time.Duration) bool, stopping func() bool) {
	rt.stopping = stopping
	rt.ReapplyStored()
	rt.recoverInterruptedRuns()
	for {
		if stopping() {
			return
		}
		if rt.monitorDue() {
			rt.monitorStep()
		}
		for !stopping() {
			job := rt.nextCommandJob()
			if job == nil || !rt.runCommandJob(job) {
				break
			}
			if stopping() {
				// The run was closed because of the stop: one attempt to send
				// its reports, so that the central does not show it running
				// until the next start (what fails stays queued).
				rt.flushOutbox()
				return
			}
			if rt.monitorDue() && !stopping() {
				rt.monitorStep()
			}
		}
		if stopping() {
			return
		}
		if !wait(rt.nextWait()) {
			return
		}
	}
}

// monitorDue reports whether the check interval has passed since the start
// of the last monitoring step.
func (rt *AgentRuntime) monitorDue() bool {
	return rt.lastMonitor.IsZero() || rt.now().Sub(rt.lastMonitor) >= rt.Interval()
}

// nextWait is the time until the next monitoring step or the next scheduled
// time of a command set, whichever comes first.
func (rt *AgentRuntime) nextWait() time.Duration {
	now := rt.now()
	next := rt.lastMonitor.Add(rt.Interval()).Sub(now)
	if next < time.Second {
		next = time.Second
	}
	if due, ok := rt.nextScheduleDue(); ok {
		if d := due.Sub(now); d < next {
			next = max(d, commandPollMax)
		}
	}
	return next
}

// Cycle performs one monitoring step.
func (rt *AgentRuntime) Cycle() {
	rt.monitorStep()
}

// monitorStep performs one periodic processing: sync, audit collection, file
// scans and the report. It is also called from the wait loop of a running
// command. A stop request does not interrupt it: the step completes and
// saves its state first.
func (rt *AgentRuntime) monitorStep() {
	cycleStart := time.Now()
	rt.lastMonitor = rt.now()
	rt.sync()

	var detections []DetectionReport
	if rt.rule != nil {
		c := &rt.rule.Content
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
	req := &SyncRequest{AgentVersion: AppVersion, LastApply: rt.lastApply, CommandSig: rt.commandSignature()}
	if rt.rule != nil {
		req.RuleID, req.RuleVersion = rt.rule.ID, rt.rule.Version
	}
	if rt.Config != nil {
		req.CAFingerprints = CAFingerprintsOf(rt.Config.CAPEMs)
	}
	resp, err := rt.Transport.Sync(req)
	if err != nil {
		rt.logConnectionError("sync", err)
		return
	}
	if resp.DefaultIntervalSec > 0 {
		rt.defaultInterval = resp.DefaultIntervalSec
	}
	if rt.Config != nil {
		rt.saveConfigUpdates(resp)
	}
	// Definitions and requests are only stored here; they are used when no
	// set runs (a running set keeps the definition it started with).
	if resp.CommandSets != nil {
		rt.storeCommandDefinitions(resp.CommandSets)
	}
	rt.receiveCommandRequests(resp.CommandRequests)
	switch {
	case resp.Rule == nil:
		rt.removeRule()
	case rt.rule != nil && resp.Rule.ID == rt.rule.ID && resp.Rule.Version == rt.rule.Version:
		// The running version is assigned again (for example after a newer
		// version failed): report its successful result again.
		rt.failedRule = ""
		if rt.lastApply != nil && rt.lastApply.Status == "error" && rt.lastOK != nil {
			rt.setApply(rt.lastOK)
		}
	case ruleKey(resp.Rule) != rt.failedRule:
		resp.Rule.Content.Normalize()
		rt.applyRule(resp.Rule, false)
	}
}

func (rt *AgentRuntime) saveConfigUpdates(resp *SyncResponse) {
	cfg := *rt.Config
	cfg.CAPEMs = append([]string(nil), rt.Config.CAPEMs...)
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
	if err := cfg.Save(rt.DB); err != nil {
		// Keep using the old token; the central hands out the same new value
		// again on the next connection.
		rt.logf("failed to save configuration (token renewal or CA update postponed): %v", err)
		return
	}
	*rt.Config = cfg
	if renewed {
		rt.logf("token renewed; new expiry %s", time.Unix(cfg.TokenExpiresAt, 0).Local().Format("2006-01-02 15:04:05 MST"))
		if rt.OnTokenRenewed != nil {
			rt.OnTokenRenewed(cfg.Token)
		}
	}
}

func (rt *AgentRuntime) logConnectionError(op string, err error) {
	var he *HTTPError
	switch {
	case errors.As(err, &he) && he.Status == 401:
		rt.logf("%s: the central rejected the token (expired, revoked, reissued or bound to another agent instance). Re-initialize the agent with a valid token", op)
	case IsCertificateError(err):
		rt.logf("%s: the central certificate is not signed by a trusted CA (%v). If the CA was rolled over while this agent was offline, re-initialize the agent", op, err)
	default:
		rt.logf("%s: cannot reach the central: %v", op, err)
	}
	if rt.Config != nil && rt.Config.TokenExpiresAt > 0 {
		left := time.Until(time.Unix(rt.Config.TokenExpiresAt, 0))
		if left < time.Duration(DefaultTokenRenewDays)*24*time.Hour {
			rt.logf("warning: the token expires at %s (in %s) and could not be renewed",
				time.Unix(rt.Config.TokenExpiresAt, 0).Local().Format("2006-01-02 15:04:05 MST"), left.Round(time.Minute))
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
	if errs := ra.Content.Validate(); len(errs) > 0 {
		result.Status = "error"
		result.Message = "validation failed: " + joinMax(errs, 5)
		rt.failedRule = ruleKey(ra)
		rt.setApply(result)
		rt.logf("rule %q version %d rejected: %s", ra.Name, ra.Version, result.Message)
		return
	}
	res := ResolveRule(&ra.Content)
	lines := GenerateAuditRules(res)
	applyWarnings, err := ApplyAuditRules(lines)
	if err != nil {
		result.Status = "error"
		result.Message = err.Error()
		rt.failedRule = ruleKey(ra)
		rt.setApply(result)
		rt.logf("rule %q version %d not applied: %v", ra.Name, ra.Version, err)
		return
	}
	rt.failedRule = ""
	result.Warnings = append(res.Warnings, applyWarnings...)
	if len(result.Warnings) > 0 {
		result.Status = "warning"
		result.Message = fmt.Sprintf("applied with %d warning(s)", len(result.Warnings))
	} else {
		result.Message = fmt.Sprintf("applied (%d audit rules)", len(lines))
	}
	rt.noteScanConfig(ra.Content.Writable.Base + "|" + fmt.Sprint(ra.Content.Writable.AllowedPaths))
	b, _ := json.Marshal(ra)
	if err := models.SetLocalState(rt.DB, stateRule, string(b)); err != nil {
		rt.logf("failed to store the applied rule: %v", err)
	}
	rt.rule = ra
	rt.lastOK = result
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
	if !holdsRule(rt.rule, rt.lastApply) {
		return
	}
	remove := rt.removeAudit
	if remove == nil {
		remove = RemoveAuditRules
	}
	now := time.Now().Unix()
	if err := remove(); err != nil {
		msg := "no rule is assigned, but removing the audit rules failed: " + err.Error()
		if rt.lastApply == nil || rt.lastApply.Status != "error" || rt.lastApply.Message != msg {
			rt.setApply(&ApplyResult{Status: "error", Message: msg, At: now})
			rt.logf("%s", msg)
		}
		return
	}
	if err := models.DeleteLocalState(rt.DB, stateRule, stateApply); err != nil {
		rt.logf("failed to clear the stored rule: %v", err)
	}
	rt.rule, rt.lastOK, rt.failedRule = nil, nil, ""
	rt.lastApply = &ApplyResult{Status: ApplyStatusNone, Message: "no rule is assigned; the audit rules were removed", At: now}
	rt.logf("no rule is assigned: the audit rules with key %s and %s were removed", AuditKey, AuditRulesFile)
}

// ClearStoredRule removes the audit rules and the rules file when the agent
// database db records a rule or an application result, and then clears that
// record. It reports whether anything was removed; without a record nothing
// is touched. The central uses it for the built-in agent while it is
// disabled.
func ClearStoredRule(db *sql.DB) (bool, error) {
	var rule *RuleAssignment
	if v := models.GetLocalState(db, stateRule); v != "" {
		rule = &RuleAssignment{}
	}
	var apply *ApplyResult
	if v := models.GetLocalState(db, stateApply); v != "" {
		var a ApplyResult
		if json.Unmarshal([]byte(v), &a) == nil {
			apply = &a
		} else {
			apply = &ApplyResult{}
		}
	}
	if !holdsRule(rule, apply) {
		return false, nil
	}
	if err := RemoveAuditRules(); err != nil {
		return false, err
	}
	return true, models.DeleteLocalState(db, stateRule, stateApply)
}

// resetStoredRule is used by "agent init --overwrite": the stored rule is
// cleared, and when audit rules may be loaded the application result is
// replaced by a pending one, so that the next sync decides (it applies the
// assigned rule, or removes the audit rules when none is assigned).
func resetStoredRule(db *sql.DB) error {
	var apply *ApplyResult
	if v := models.GetLocalState(db, stateApply); v != "" {
		apply = &ApplyResult{}
		_ = json.Unmarshal([]byte(v), apply)
	}
	var rule *RuleAssignment
	if models.GetLocalState(db, stateRule) != "" {
		rule = &RuleAssignment{}
	}
	if !holdsRule(rule, apply) {
		return nil
	}
	b, err := json.Marshal(&ApplyResult{Status: "pending", Message: "the stored rule was cleared by agent init; the next sync decides", At: time.Now().Unix()})
	if err != nil {
		return err
	}
	if err := models.DeleteLocalState(db, stateRule); err != nil {
		return err
	}
	return models.SetLocalState(db, stateApply, string(b))
}

func joinMax(s []string, n int) string {
	if len(s) > n {
		s = append(s[:n:n], fmt.Sprintf("and %d more", len(s)-n))
	}
	out := ""
	for i, v := range s {
		if i > 0 {
			out += "; "
		}
		out += v
	}
	return out
}

func (rt *AgentRuntime) setApply(r *ApplyResult) {
	rt.lastApply = r
	if b, err := json.Marshal(r); err == nil {
		_ = models.SetLocalState(rt.DB, stateApply, string(b))
	}
}

func (rt *AgentRuntime) collectAudit(c *RuleContent, users map[int]string) []DetectionReport {
	var last AuditEventID
	_ = json.Unmarshal([]byte(models.GetLocalState(rt.DB, stateLastAuditEvent)), &last)
	events, next, err := CollectAuditEvents(last)
	if err != nil {
		rt.logf("reading the audit log failed: %v", err)
		return nil
	}
	if b, err := json.Marshal(next); err == nil {
		_ = models.SetLocalState(rt.DB, stateLastAuditEvent, string(b))
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
	if models.GetLocalState(rt.DB, stateScanSignature) == sig {
		return
	}
	if models.GetLocalState(rt.DB, stateScanPrevRoots) == "" {
		if old, err := models.ListScanRoots(rt.DB); err == nil {
			if b, err := json.Marshal(old); err == nil {
				_ = models.SetLocalState(rt.DB, stateScanPrevRoots, string(b))
			}
		}
	}
	_ = models.SetLocalState(rt.DB, stateScanSignature, sig)
	_ = models.SetLocalState(rt.DB, stateLastFullScan, "0")
	_ = models.DeleteLocalState(rt.DB, stateWritableResume, stateWritablePass)
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

func (rt *AgentRuntime) loadJSONState(key string, v any) {
	if s := models.GetLocalState(rt.DB, key); s != "" {
		_ = json.Unmarshal([]byte(s), v)
	}
}

func (rt *AgentRuntime) saveJSONState(key string, v any) {
	if b, err := json.Marshal(v); err == nil {
		_ = models.SetLocalState(rt.DB, key, string(b))
	}
}

// incompleteScans returns the walks whose pass is split over several checks
// because they reached the entry limit.
func (rt *AgentRuntime) incompleteScans() []IncompleteScan {
	var out []IncompleteScan
	if at := models.GetLocalState(rt.DB, stateWritableResume); at != "" {
		var pass writablePass
		rt.loadJSONState(stateWritablePass, &pass)
		out = append(out, IncompleteScan{Kind: ScanKindWritable, ResumeAt: at, Limit: rt.scanEntryLimit(), StartedAt: pass.StartedAt})
	}
	if at := models.GetLocalState(rt.DB, stateExecResume); at != "" {
		started, _ := strconv.ParseInt(models.GetLocalState(rt.DB, stateExecPassStart), 10, 64)
		out = append(out, IncompleteScan{Kind: ScanKindExecFiles, ResumeAt: at, Limit: rt.scanEntryLimit(), StartedAt: started})
	}
	return out
}

// scanFiles runs the file scans of one check. Each walk visits at most the
// entry limit; a walk that reaches it continues at the same place in the
// next check, and starts at the beginning again once it reached the end
// (see chapter 10 of the system specification in Documents).
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
	lastFull, _ := strconv.ParseInt(models.GetLocalState(rt.DB, stateLastFullScan), 10, 64)
	// Since times of allowed roots that the executed-file scan has not
	// walked to the end yet: roots found for the first time by a full scan
	// (files created in them before they became roots are older than the
	// since time of their part, so they are scanned from the previous full
	// scan) and roots that appear after the scan configuration changed
	// (scanned like the roots of a first scan: the files already in them are
	// reported).
	rootSince := map[string]int64{}
	rt.loadJSONState(stateRootSince, &rootSince)
	setRootSince := func(r string, v int64) {
		if old, ok := rootSince[r]; !ok || v < old {
			rootSince[r] = v
		}
	}
	wResume := models.GetLocalState(rt.DB, stateWritableResume)
	if wResume != "" || now-lastFull >= int64(c.ScanInterval()) {
		var pass writablePass
		if wResume != "" {
			rt.loadJSONState(stateWritablePass, &pass)
			rt.logf("writable scan of %s continues at %s", c.Writable.Base, wResume)
		} else {
			pass = writablePass{StartedAt: now}
		}
		wr, err := ScanWritableFrom(c.Writable.Base, c.Writable.AllowedPaths, uids, c.Writable.Category, wResume, limit)
		if err != nil {
			rt.logf("writable scan of %s failed: %v", c.Writable.Base, err)
		} else {
			if c.WritableEnabled() {
				for _, f := range wr.Findings {
					fp := f.Fingerprint()
					if !models.FindingExists(rt.DB, fp) {
						out = append(out, f.ToReport(users, groups, now))
					}
					_ = models.TouchFinding(rt.DB, fp, now)
				}
			}
			known := map[string]bool{}
			if old, err := models.ListScanRoots(rt.DB); err == nil {
				for _, r := range old {
					known[r] = true
				}
			}
			if prev := models.GetLocalState(rt.DB, stateScanPrevRoots); prev != "" {
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
				for _, r := range wr.AllowedRoots {
					if !known[r] {
						setRootSince(r, lastFull)
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
				_ = models.ReplaceScanRoots(rt.DB, all)
				rt.saveJSONState(stateWritablePass, &pass)
				_ = models.SetLocalState(rt.DB, stateWritableResume, wr.ResumeAt)
				rt.logf("writable scan of %s stopped after %d entries; the next check continues at %s", c.Writable.Base, limit, wr.ResumeAt)
			} else {
				_ = models.ReplaceScanRoots(rt.DB, passRoots)
				if c.WritableEnabled() {
					_ = models.PruneFindings(rt.DB, TypeWritable+"|", pass.StartedAt)
				}
				_ = models.SetLocalState(rt.DB, stateScanPrevRoots, "")
				_ = models.SetLocalState(rt.DB, stateLastFullScan, strconv.FormatInt(pass.StartedAt, 10))
				_ = models.DeleteLocalState(rt.DB, stateWritableResume, stateWritablePass)
				if wResume != "" {
					rt.logf("writable scan of %s completed the pass started at %s", c.Writable.Base,
						time.Unix(pass.StartedAt, 0).Local().Format("2006-01-02 15:04:05 MST"))
				}
			}
		}
	}
	if c.ExecFilesEnabled() {
		roots, err := models.ListScanRoots(rt.DB)
		if err == nil {
			var segs []sinceSegment
			rt.loadJSONState(stateExecSince, &segs)
			if len(segs) == 0 {
				// The watermark of earlier versions covers everything.
				wm, _ := strconv.ParseInt(models.GetLocalState(rt.DB, stateExecWatermark), 10, 64)
				segs = []sinceSegment{{From: "", Since: wm}}
			}
			eResume := models.GetLocalState(rt.DB, stateExecResume)
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
				for _, f := range found {
					fp := f.Fingerprint()
					if !models.FindingExists(rt.DB, fp) {
						out = append(out, f.ToReport(users, groups, now))
					}
					_ = models.TouchFinding(rt.DB, fp, now)
				}
				// Files of a first scan that are not reported are remembered,
				// so the next scan (which also looks at the second of this one)
				// does not report them either.
				for _, f := range skipped {
					_ = models.TouchFinding(rt.DB, f.Fingerprint(), now)
				}
				// The walked part is scanned up to the second before the cycle
				// start: files whose status changed at or after it are picked
				// up again next time (duplicates are filtered above).
				segs = markScanned(segs, eResume, stop, now-1)
				rt.saveJSONState(stateExecSince, segs)
				// A root walked to its end in this check needs no own since
				// time any more.
				for r := range rootSince {
					if (eResume == "" || !entirelyBefore(r, eResume)) && (stop == "" || entirelyBefore(r, stop)) {
						delete(rootSince, r)
					}
				}
				if stop != "" {
					if eResume == "" {
						_ = models.SetLocalState(rt.DB, stateExecPassStart, strconv.FormatInt(now, 10))
					}
					_ = models.SetLocalState(rt.DB, stateExecResume, stop)
					rt.logf("executed-file scan stopped after %d entries; the next check continues at %s", limit, stop)
				} else if eResume != "" {
					_ = models.DeleteLocalState(rt.DB, stateExecResume, stateExecPassStart)
					rt.logf("executed-file scan completed its pass")
				}
				_ = models.PruneFindings(rt.DB, TypeExecFile+"|", now-7*24*3600)
			}
		}
	}
	rt.saveJSONState(stateRootSince, rootSince)
	return out
}

func (rt *AgentRuntime) report(detections []DetectionReport) {
	// The directory structure is reported only for the scan origin of the
	// applied rule; there is no default origin.
	in := HostInfoInput{LastApply: rt.lastApply}
	if rt.rule != nil {
		in.Rule = &RuleRef{ID: rt.rule.ID, Version: rt.rule.Version}
		in.ScanBase = rt.rule.Content.Writable.Base
		in.IncompleteScans = rt.incompleteScans()
	}
	if rt.Config != nil {
		in.TrustedCAPEMs = rt.Config.CAPEMs
	}
	hi := CollectHostInfo(in)

	dropped, _ := strconv.Atoi(models.GetLocalState(rt.DB, stateDroppedPending))
	req := &ReportRequest{CreatedAt: time.Now().Unix(), Detections: detections, HostInfo: hi, Apply: rt.lastApply, Dropped: dropped}
	if err := rt.enqueue(req); err != nil {
		rt.logf("failed to queue the report: %v", err)
		return
	}
	_ = models.SetLocalState(rt.DB, stateDroppedPending, "0")
	rt.enforceOutboxLimit()
	rt.flushOutbox()
}

func countCategories(ds []DetectionReport) (alerts, normals int) {
	for _, d := range ds {
		if d.Category == CategoryAlert {
			alerts++
		} else {
			normals++
		}
	}
	return
}

// enqueue stores a report. Only the newest report needs the host
// information, so it is removed from the queued ones, and a queued report
// with nothing else to deliver (no detections, no dropped count, nothing
// about command sets and the same application result as the new one) is
// replaced by the new report.
func (rt *AgentRuntime) enqueue(req *ReportRequest) error {
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	apply, _ := json.Marshal(req.Apply)
	items, err := models.ListOutbox(rt.DB)
	if err != nil {
		return err
	}
	tx, err := rt.DB.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, it := range items {
		var old ReportRequest
		if json.Unmarshal([]byte(it.Payload), &old) != nil {
			continue
		}
		oldApply, _ := json.Marshal(old.Apply)
		if len(old.Detections) == 0 && old.Dropped == 0 && string(oldApply) == string(apply) &&
			len(old.CommandRuns) == 0 && len(old.CommandRequestAcks) == 0 && old.CommandApply == nil {
			if err := models.DeleteOutbox(tx, it.ID); err != nil {
				return err
			}
			continue
		}
		if old.HostInfo != nil {
			old.HostInfo = nil
			ob, err := json.Marshal(&old)
			if err != nil {
				return err
			}
			it.Payload = string(ob)
			it.CommandBytes = commandBytesOf(&old, len(ob))
			if err := models.UpdateOutbox(tx, it); err != nil {
				return err
			}
		}
	}
	a, n := countCategories(req.Detections)
	if err := models.InsertOutbox(tx, &models.OutboxItem{Payload: string(b), AlertCount: a, NormalCount: n}); err != nil {
		return err
	}
	return tx.Commit()
}

// enforceOutboxLimit keeps the stored detections under the limit by dropping
// normal detections oldest first (then alerts oldest first if still needed)
// and remembers how many were dropped so the count is reported.
func (rt *AgentRuntime) enforceOutboxLimit() {
	total := models.OutboxDetectionCount(rt.DB)
	if total <= DefaultOutboxMaxDetection {
		return
	}
	excess := total - DefaultOutboxMaxDetection
	items, err := models.ListOutbox(rt.DB)
	if err != nil {
		return
	}
	dropped := 0
	for _, pass := range []string{CategoryNormal, CategoryAlert} {
		for _, it := range items {
			if excess <= 0 {
				break
			}
			var req ReportRequest
			if json.Unmarshal([]byte(it.Payload), &req) != nil {
				continue
			}
			kept := req.Detections[:0]
			for _, d := range req.Detections {
				isPass := (pass == CategoryAlert && d.Category == CategoryAlert) || (pass == CategoryNormal && d.Category != CategoryAlert)
				if excess > 0 && isPass {
					excess--
					dropped++
					continue
				}
				kept = append(kept, d)
			}
			req.Detections = kept
			b, _ := json.Marshal(&req)
			it.Payload = string(b)
			it.CommandBytes = commandBytesOf(&req, len(b))
			it.AlertCount, it.NormalCount = countCategories(kept)
			_ = models.UpdateOutbox(rt.DB, it)
		}
	}
	if dropped > 0 {
		prev, _ := strconv.Atoi(models.GetLocalState(rt.DB, stateDroppedPending))
		_ = models.SetLocalState(rt.DB, stateDroppedPending, strconv.Itoa(prev+dropped))
		rt.logf("report storage limit reached: dropped %d detection(s)", dropped)
	}
}

// flushOutbox sends queued reports in order and stops at the first failure.
func (rt *AgentRuntime) flushOutbox() {
	items, err := models.ListOutbox(rt.DB)
	if err != nil {
		return
	}
	for i, it := range items {
		var req ReportRequest
		if err := json.Unmarshal([]byte(it.Payload), &req); err != nil {
			_ = models.DeleteOutbox(rt.DB, it.ID)
			continue
		}
		// Only the newest report needs the host information.
		if i < len(items)-1 {
			req.HostInfo = nil
		}
		if err := rt.Transport.Report(&req); err != nil {
			rt.logConnectionError("report", err)
			return
		}
		_ = models.DeleteOutbox(rt.DB, it.ID)
	}
}
