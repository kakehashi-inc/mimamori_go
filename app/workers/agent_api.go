package workers

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"mimamori/app/models"
	"mimamori/app/modules"
)

// agentHandler serves the agent and management listener (HTTPS).
func (c *core) agentHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /agent/v1/ca", c.handleAgentCA)
	mux.HandleFunc("POST /agent/v1/sync", c.handleAgentSync)
	mux.HandleFunc("POST /agent/v1/report", c.handleAgentReport)
	admin := http.NewServeMux()
	c.registerAPI(admin, "/admin/v1", true)
	mux.Handle("/admin/", c.adminAPIAuth(admin))
	return mux
}

func (c *core) handleAgentCA(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, modules.CAResponse{CAPEMs: c.currentCAPEMs()})
}

func (c *core) handleAgentSync(w http.ResponseWriter, r *http.Request) {
	tok, agent, err := c.authenticateAgent(r)
	if err != nil {
		c.agentAuthFailed(w, r, err)
		return
	}
	var req modules.SyncRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	resp, err := c.centralSync(agent, tok, &req)
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (c *core) handleAgentReport(w http.ResponseWriter, r *http.Request) {
	_, agent, err := c.authenticateAgent(r)
	if err != nil {
		c.agentAuthFailed(w, r, err)
		return
	}
	var req modules.ReportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if err := c.centralReport(agent, &req); err != nil {
		c.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (c *core) agentAuthFailed(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, errAgentAuth) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	c.internalError(w, r, err)
}

// centralSync answers a sync request of an agent (tok is nil for the
// built-in agent, which needs neither token renewal nor CA distribution).
func (c *core) centralSync(agent *models.Agent, tok *models.Token, req *modules.SyncRequest) (*modules.SyncResponse, error) {
	now := time.Now().Unix()
	fps, _ := json.Marshal(req.CAFingerprints)
	if err := models.RecordAgentSync(c.db, agent.ID, req.AgentVersion, req.RuleID, req.RuleVersion, string(fps), now); err != nil {
		return nil, err
	}
	resp := &modules.SyncResponse{
		AgentName:          modules.BuiltinAgentName,
		DefaultIntervalSec: c.conf().DefaultCheckIntervalSec,
	}
	// The assigned version is always sent, so the agent can tell whether it
	// runs the assigned version (it applies only a different one).
	if agent.AssignedRuleID.Valid {
		ra, err := c.ruleAssignment(agent.AssignedRuleID.Int64, agent.AssignedRuleVersion)
		if err != nil {
			return nil, err
		}
		resp.Rule = ra
	}
	if tok == nil {
		return resp, nil
	}
	resp.AgentName = tok.Name
	resp.TokenExpiresAt = tok.ExpiresAt
	renewBefore := int64(c.conf().TokenRenewBeforeDays) * 86400
	if tok.ExpiresAt-now < renewBefore {
		offer := true
		if tok.PendingHash == "" || tok.PendingExpiresAt <= now {
			prevPending, prevCounter := tok.PendingHash, tok.RenewCounter
			tok.RenewCounter++
			v := modules.DeriveRenewalToken(c.serverKey, tok.ID, tok.RenewCounter)
			tok.PendingHash = modules.HashSecret(v)
			tok.PendingExpiresAt = now + int64(c.conf().TokenValidDays)*86400
			// Stored only while the token is unchanged since it was read; when
			// it was revoked, reissued or renewed meanwhile, no value is handed
			// out this time.
			err := models.SetTokenRenewal(c.db, tok, prevPending, prevCounter)
			switch {
			case errors.Is(err, sql.ErrNoRows):
				offer = false
			case err != nil:
				return nil, err
			default:
				c.history(nil, models.HistToken, "renewal_issued", tok.Name,
					"new token value handed to the agent; expires "+time.Unix(tok.PendingExpiresAt, 0).UTC().Format(time.RFC3339))
			}
		}
		// The same value is handed out again until the agent uses it.
		if offer {
			resp.TokenRenewal = &modules.TokenRenewal{
				Token:     modules.DeriveRenewalToken(c.serverKey, tok.ID, tok.RenewCounter),
				ExpiresAt: tok.PendingExpiresAt,
			}
		}
	}
	missing := false
	for _, ca := range c.trustedCAs() {
		if !slices.Contains(req.CAFingerprints, ca.Fingerprint) {
			missing = true
		}
	}
	if missing {
		for _, ca := range c.trustedCAs() {
			resp.CAPEMs = append(resp.CAPEMs, ca.CertPEM)
		}
	}
	return resp, nil
}

func (c *core) ruleAssignment(ruleID int64, version int) (*modules.RuleAssignment, error) {
	rule, err := models.GetRule(c.db, ruleID)
	if err != nil {
		return nil, err
	}
	rv, err := models.GetRuleVersion(c.db, ruleID, version)
	if err != nil {
		return nil, err
	}
	content, err := modules.ParseRuleContent([]byte(rv.Content))
	if err != nil {
		return nil, err
	}
	return &modules.RuleAssignment{ID: rule.ID, Name: rule.Name, Version: rv.Version, Content: *content}, nil
}

// detectionFingerprint identifies "the same detection" of an agent.
func detectionFingerprint(d *modules.DetectionReport) string {
	key := d.Type + "\x00" + d.OSUser + "\x00" + d.Exe + "\x00" + d.Target
	if d.Type == modules.TypeWritable || d.Type == modules.TypeExecFile {
		key = d.Type + "\x00" + d.Target
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:16])
}

func matchSuppression(s *models.Suppression, agentID int64, typ, user, exe, target string) bool {
	if s.AgentID.Valid && s.AgentID.Int64 != agentID {
		return false
	}
	if s.Type != "" && s.Type != typ {
		return false
	}
	if s.OSUser != "" && s.OSUser != user {
		return false
	}
	return modules.GlobMatch(s.Exe, exe) && modules.GlobMatch(s.Target, target)
}

func findSuppression(list []*models.Suppression, agentID int64, typ, user, exe, target string) *models.Suppression {
	for _, s := range list {
		if matchSuppression(s, agentID, typ, user, exe, target) {
			return s
		}
	}
	return nil
}

// centralReport stores a report: detections into the target's own database
// (aggregated with the open row of the same detection), host information and
// the rule application result.
func (c *core) centralReport(agent *models.Agent, req *modules.ReportRequest) error {
	tdb, err := c.targetDB(agent)
	if err != nil {
		return err
	}
	sups, err := models.ListSuppressions(c.db)
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	dedupe := int64(c.conf().Notify.DedupeHours) * 3600
	tx, err := tdb.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i := range req.Detections {
		d := &req.Detections[i]
		if !slices.Contains(modules.DetectionTypes, d.Type) {
			continue
		}
		if d.Category != modules.CategoryAlert && d.Category != modules.CategoryNormal {
			d.Category = modules.CategoryAlert
		}
		if d.Count < 1 {
			d.Count = 1
		}
		if d.FirstAt == 0 {
			d.FirstAt = now
		}
		if d.LastAt == 0 {
			d.LastAt = d.FirstAt
		}
		d.Target = truncate(d.Target, modules.MaxTargetLength)
		d.Exe = truncate(d.Exe, 1024)
		fp := detectionFingerprint(d)
		sup := findSuppression(sups, agent.ID, d.Type, d.OSUser, d.Exe, d.Target)
		existing, err := models.FindOpenDetection(tx, fp)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if existing != nil {
			existing.Count += d.Count
			existing.FirstAt = min(existing.FirstAt, d.FirstAt)
			existing.LastAt = max(existing.LastAt, d.LastAt)
			existing.Category = d.Category
			if d.FileCtime > 0 {
				existing.FileCtime, existing.FileOwner, existing.FileMode = d.FileCtime, d.FileOwner, d.FileMode
			}
			existing.Suppressed, existing.SuppressionID = sup != nil, 0
			if sup != nil {
				existing.SuppressionID = sup.ID
				if existing.NotifyState == models.NotifyPending {
					existing.NotifyState = models.NotifySkipped
				}
			} else if existing.NotifyState != models.NotifyPending && now-models.LastNotifiedAt(tx, fp) >= dedupe {
				existing.NotifyState = models.NotifyPending
			}
			if err := models.UpdateDetectionOccurrence(tx, existing); err != nil {
				return err
			}
			continue
		}
		row := &models.Detection{
			Fingerprint: fp, Type: d.Type, Category: d.Category, OSUser: d.OSUser, Exe: d.Exe, Target: d.Target,
			Count: d.Count, FirstAt: d.FirstAt, LastAt: d.LastAt, FileCtime: d.FileCtime, FileOwner: d.FileOwner,
			FileMode: d.FileMode, NotifyState: models.NotifyPending,
		}
		if sup != nil {
			row.Suppressed, row.SuppressionID, row.NotifyState = true, sup.ID, models.NotifySkipped
		} else if last := models.LastNotifiedAt(tx, fp); last > 0 && now-last < dedupe {
			row.NotifyState = models.NotifySkipped
		}
		if err := models.InsertDetection(tx, row); err != nil {
			return err
		}
	}
	if req.HostInfo != nil {
		b, _ := json.Marshal(req.HostInfo)
		if err := models.InsertHostReport(tx, string(b), 3); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	f := models.AgentReportFields{Now: now, Dropped: int64(max(req.Dropped, 0))}
	if hi := req.HostInfo; hi != nil {
		f.HasHost = true
		f.Hostname, f.OSName, f.OSVersion, f.AgentVersion = hi.Hostname, hi.OSName, hi.OSVersion, hi.AgentVersion
		b, _ := json.Marshal(hi.Auditd)
		f.AuditdJSON = string(b)
	}
	if ap := req.Apply; ap != nil {
		f.HasApply = true
		f.ApplyStatus, f.ApplyMessage, f.ApplyVersion, f.ApplyAt = ap.Status, truncate(ap.Message, 2000), ap.Version, ap.At
		w, _ := json.Marshal(nonNil(ap.Warnings))
		f.ApplyWarnings = string(w)
		if ap.Status == "ok" || ap.Status == "warning" {
			f.ReportedRuleID, f.ReportedRuleVersion = ap.RuleID, ap.Version
		} else {
			f.ReportedRuleID, f.ReportedRuleVersion = agent.ReportedRuleID, agent.ReportedRuleVersion
		}
		if ap.At != agent.ApplyAt || ap.Status != agent.ApplyStatus {
			ruleName := fmt.Sprintf("rule %d", ap.RuleID)
			if r, err := models.GetRule(c.db, ap.RuleID); err == nil {
				ruleName = r.Name
			}
			c.history(nil, models.HistRule, "apply_result_"+ap.Status, c.agentDisplayName(agent),
				fmt.Sprintf("%s version %d: %s", ruleName, ap.Version, strings.Join(append([]string{ap.Message}, ap.Warnings...), "; ")))
		}
	}
	if req.Dropped > 0 {
		c.history(nil, models.HistDetection, "dropped", c.agentDisplayName(agent), fmt.Sprintf("the agent dropped %d detection(s) at its storage limit", req.Dropped))
	}
	return models.RecordAgentReport(c.db, agent.ID, f)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
