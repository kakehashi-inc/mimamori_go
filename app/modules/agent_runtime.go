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
	stateAuditApply     = "audit_apply"
	stateExecWatermark  = "exec_watermark"
	stateLastFullScan   = "last_full_scan"
	stateScanSignature  = "scan_signature"
	stateDroppedPending = "dropped_pending"
	stateLastAuditEvent = "last_audit_event"
)

// AgentRuntime performs the periodic processing of an agent. Everything runs
// sequentially in the calling flow: syncing, applying rules, reading the
// audit log, scanning files and reporting.
type AgentRuntime struct {
	DB        *sql.DB
	Transport AgentTransport
	Builtin   bool
	DataDir   string
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
}

// NewAgentRuntime loads the stored state.
func NewAgentRuntime(db *sql.DB, tr AgentTransport, dataDir string, logger *log.Logger) *AgentRuntime {
	rt := &AgentRuntime{DB: db, Transport: tr, DataDir: dataDir, Logger: logger,
		defaultInterval: DefaultCheckIntervalSec}
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
func (rt *AgentRuntime) Run(wait func(time.Duration) bool, stopping func() bool) {
	rt.ReapplyStored()
	for {
		if stopping() {
			return
		}
		start := time.Now()
		rt.Cycle()
		elapsed := time.Since(start)
		next := rt.Interval() - elapsed
		if next < time.Second {
			next = time.Second
		}
		if !wait(next) {
			return
		}
	}
}

// Cycle performs one periodic processing. A stop request does not interrupt
// it: the cycle completes and saves its state first.
func (rt *AgentRuntime) Cycle() {
	cycleStart := time.Now()
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
}

func (rt *AgentRuntime) sync() {
	req := &SyncRequest{AgentVersion: AppVersion, LastApply: rt.lastApply}
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
	var st AuditApplyState
	_ = json.Unmarshal([]byte(models.GetLocalState(rt.DB, stateAuditApply)), &st)
	if err := ApplyAuditRules(lines, &st); err != nil {
		result.Status = "error"
		result.Message = err.Error()
		rt.failedRule = ruleKey(ra)
		rt.setApply(result)
		rt.logf("rule %q version %d not applied: %v", ra.Name, ra.Version, err)
		return
	}
	if b, err := json.Marshal(st); err == nil {
		_ = models.SetLocalState(rt.DB, stateAuditApply, string(b))
	}
	rt.failedRule = ""
	result.Warnings = res.Warnings
	if len(res.Warnings) > 0 {
		result.Status = "warning"
		result.Message = fmt.Sprintf("applied with %d warning(s)", len(res.Warnings))
	} else {
		result.Message = fmt.Sprintf("applied (%d audit rules)", len(lines))
	}
	// A changed scan configuration needs a fresh full scan.
	sig := ra.Content.Writable.Base + "|" + fmt.Sprint(ra.Content.Writable.AllowedPaths)
	if models.GetLocalState(rt.DB, stateScanSignature) != sig {
		_ = models.SetLocalState(rt.DB, stateScanSignature, sig)
		_ = models.SetLocalState(rt.DB, stateLastFullScan, "0")
		_ = models.ReplaceScanRoots(rt.DB, nil)
	}
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
			_ = models.ReplaceScanRoots(rt.DB, wr.AllowedRoots)
			_ = models.SetLocalState(rt.DB, stateLastFullScan, strconv.FormatInt(now, 10))
		}
	}
	if c.ExecFilesEnabled() {
		roots, err := models.ListScanRoots(rt.DB)
		if err == nil {
			wm, _ := strconv.ParseInt(models.GetLocalState(rt.DB, stateExecWatermark), 10, 64)
			found, err := ScanExecFiles(c.Writable.Base, roots, c, wm, wm == 0)
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

func (rt *AgentRuntime) enqueue(req *ReportRequest) error {
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	a, n := countCategories(req.Detections)
	return models.InsertOutbox(rt.DB, &models.OutboxItem{Payload: string(b), AlertCount: a, NormalCount: n})
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
