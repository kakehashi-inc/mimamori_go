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
// command (SIGTERM, grace, SIGKILL) and its run is still reported.
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
// every other root as new (see scanFiles).
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
	_ = models.ReplaceScanRoots(rt.DB, nil)
}

func (rt *AgentRuntime) scanFiles(c *RuleContent, res *ResolvedRule, cycleStart time.Time) []DetectionReport {
	var out []DetectionReport
	now := cycleStart.Unix()
	_, users := ReadPasswd()
	groups := ReadGroups()
	uids := map[int]bool{}
	for _, u := range res.UIDs {
		uids[u] = true
	}
	lastFull, _ := strconv.ParseInt(models.GetLocalState(rt.DB, stateLastFullScan), 10, 64)
	// Allowed roots that a full scan of this cycle found for the first time.
	// Files created in them before they became roots are older than the
	// watermark, so they are scanned from the previous full scan.
	var newRoots map[string]bool
	// Allowed roots that appear after the scan configuration changed (another
	// base directory, other allowed paths). They are scanned like the roots
	// of a first scan: the files already in them are reported.
	var freshRoots map[string]bool
	if now-lastFull >= int64(c.ScanInterval()) {
		wr, err := ScanWritable(c.Writable.Base, c.Writable.AllowedPaths, uids, c.Writable.Category)
		if err != nil {
			rt.logf("writable scan of %s failed: %v", c.Writable.Base, err)
		} else {
			if wr.Truncated {
				rt.logf("writable scan of %s stopped after %d entries", c.Writable.Base, MaxScanEntries)
			}
			if c.WritableEnabled() {
				for _, f := range wr.Findings {
					fp := f.Fingerprint()
					if !models.FindingExists(rt.DB, fp) {
						out = append(out, f.ToReport(users, groups, now))
					}
					_ = models.TouchFinding(rt.DB, fp, now)
				}
				_ = models.PruneFindings(rt.DB, TypeWritable+"|", now)
			}
			if lastFull > 0 {
				if old, err := models.ListScanRoots(rt.DB); err == nil {
					known := map[string]bool{}
					for _, r := range old {
						known[r] = true
					}
					newRoots = map[string]bool{}
					for _, r := range wr.AllowedRoots {
						if !known[r] {
							newRoots[r] = true
						}
					}
				}
			}
			if prev := models.GetLocalState(rt.DB, stateScanPrevRoots); prev != "" {
				var old []string
				_ = json.Unmarshal([]byte(prev), &old)
				known := map[string]bool{}
				for _, r := range old {
					known[r] = true
				}
				freshRoots = map[string]bool{}
				for _, r := range wr.AllowedRoots {
					if !known[r] {
						freshRoots[r] = true
					}
				}
				_ = models.SetLocalState(rt.DB, stateScanPrevRoots, "")
			}
			_ = models.ReplaceScanRoots(rt.DB, wr.AllowedRoots)
			_ = models.SetLocalState(rt.DB, stateLastFullScan, strconv.FormatInt(now, 10))
		}
	}
	if c.ExecFilesEnabled() {
		roots, err := models.ListScanRoots(rt.DB)
		if err == nil {
			wm, _ := strconv.ParseInt(models.GetLocalState(rt.DB, stateExecWatermark), 10, 64)
			since := func(root string) int64 {
				if freshRoots[root] {
					return 0
				}
				if newRoots[root] && lastFull < wm {
					return lastFull
				}
				return wm
			}
			found, skipped, err := ScanExecFiles(c.Writable.Base, roots, c, since)
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
				// Files whose status changed at or after the cycle start are
				// picked up again next time (duplicates are filtered above).
				_ = models.SetLocalState(rt.DB, stateExecWatermark, strconv.FormatInt(now-1, 10))
				_ = models.PruneFindings(rt.DB, TypeExecFile+"|", now-7*24*3600)
			}
		}
	}
	return out
}

func (rt *AgentRuntime) report(detections []DetectionReport) {
	// The directory structure is reported only for the scan origin of the
	// applied rule; there is no default origin.
	in := HostInfoInput{LastApply: rt.lastApply}
	if rt.rule != nil {
		in.Rule = &RuleRef{ID: rt.rule.ID, Version: rt.rule.Version}
		in.ScanBase = rt.rule.Content.Writable.Base
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
