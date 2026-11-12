package workers

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"mimamori/app/models"
	"mimamori/app/modules"
)

type tokenRefJSON struct {
	ID        int64   `json:"id"`
	Name      string  `json:"name"`
	Status    string  `json:"status"`
	ExpiresAt *string `json:"expires_at"`
}

type agentRuleJSON struct {
	ID              int64  `json:"id"`
	Name            string `json:"name"`
	AssignedVersion int    `json:"assigned_version"`
	ReportedVersion *int   `json:"reported_version"`
	LatestVersion   int    `json:"latest_version"`
	Outdated        bool   `json:"outdated"`
}

type applyJSON struct {
	Status   string   `json:"status"`
	Message  string   `json:"message"`
	Version  *int     `json:"version"`
	At       *string  `json:"at"`
	Warnings []string `json:"warnings"`
}

type agentSummaryJSON struct {
	ID                 int64                 `json:"id"`
	Kind               string                `json:"kind"`
	DisplayName        string                `json:"display_name"`
	Hostname           string                `json:"hostname"`
	Token              *tokenRefJSON         `json:"token"`
	LastReportAt       *string               `json:"last_report_at"`
	SecondsSinceReport *int64                `json:"seconds_since_report"`
	Rule               *agentRuleJSON        `json:"rule"`
	Apply              applyJSON             `json:"apply"`
	UnackedAlerts      int                   `json:"unacked_alerts"`
	Auditd             *modules.AuditdStatus `json:"auditd"`
	CAUpToDate         bool                  `json:"ca_up_to_date"`
	BuiltinRunning     *bool                 `json:"builtin_running"`
}

type agentDetailJSON struct {
	agentSummaryJSON
	HostInfo     *hostInfoJSON `json:"host_info"`
	TokenHistory []historyJSON `json:"token_history"`
	DroppedTotal int64         `json:"dropped_total"`
}

// hostInfoJSON converts Unix times of modules.HostInfo for the API.
type hostInfoJSON struct {
	CollectedAt          *string                 `json:"collected_at"`
	Hostname             string                  `json:"hostname"`
	OSName               string                  `json:"os_name"`
	OSVersion            string                  `json:"os_version"`
	Kernel               string                  `json:"kernel"`
	AgentVersion         string                  `json:"agent_version"`
	Auditd               modules.AuditdStatus    `json:"auditd"`
	ScanBase             string                  `json:"scan_base"`
	Directories          []modules.DirectoryInfo `json:"directories"`
	DirectoriesTruncated bool                    `json:"directories_truncated"`
	Rule                 *modules.RuleRef        `json:"rule"`
	LastApply            applyJSON               `json:"last_apply"`
	TrustedCAs           []trustedCAJSON         `json:"trusted_cas"`
}

type trustedCAJSON struct {
	Fingerprint string  `json:"fingerprint"`
	NotAfter    *string `json:"not_after"`
}

func auditdOut(a modules.AuditdStatus) modules.AuditdStatus {
	a.HaltActions = nonNil(a.HaltActions)
	return a
}

func applyOut(ap *modules.ApplyResult) applyJSON {
	if ap == nil {
		return applyJSON{Status: "none", Warnings: []string{}}
	}
	v := ap.Version
	return applyJSON{Status: ap.Status, Message: ap.Message, Version: &v, At: ts(ap.At), Warnings: nonNil(ap.Warnings)}
}

// agentSummary builds the list entry of an agent.
func (c *core) agentSummary(a *models.Agent, latestCAs []string, rules map[int64]*models.Rule) agentSummaryJSON {
	now := time.Now().Unix()
	s := agentSummaryJSON{ID: a.ID, Kind: a.Kind, DisplayName: c.agentDisplayName(a), Hostname: a.Hostname,
		LastReportAt: ts(a.LastReportAt)}
	if a.LastReportAt > 0 {
		d := now - a.LastReportAt
		s.SecondsSinceReport = &d
	}
	if a.Kind == models.AgentBuiltin {
		running, _ := c.builtinStatus()
		s.BuiltinRunning = &running
		if s.Hostname == "" {
			s.Hostname = c.hostname
		}
		s.CAUpToDate = true
	} else if a.TokenID.Valid {
		if t, err := models.GetToken(c.db, a.TokenID.Int64); err == nil {
			s.Token = &tokenRefJSON{ID: t.ID, Name: t.Name, Status: t.Status(now), ExpiresAt: ts(t.ExpiresAt)}
		}
		var fps []string
		_ = json.Unmarshal([]byte(a.CAFingerprints), &fps)
		s.CAUpToDate = true
		// An agent that never synchronized has reported no CA yet; like the
		// certificate status, it is not flagged as missing the new CA.
		for _, fp := range latestCAs {
			if a.LastSyncAt > 0 && !slices.Contains(fps, fp) {
				s.CAUpToDate = false
			}
		}
	}
	if a.AssignedRuleID.Valid {
		r := rules[a.AssignedRuleID.Int64]
		if r == nil {
			r, _ = models.GetRule(c.db, a.AssignedRuleID.Int64)
		}
		if r != nil {
			ar := &agentRuleJSON{ID: r.ID, Name: r.Name, AssignedVersion: a.AssignedRuleVersion, LatestVersion: r.LatestVersion,
				Outdated: a.AssignedRuleVersion != r.LatestVersion}
			if a.ReportedRuleID == r.ID && a.ReportedRuleVersion > 0 {
				v := a.ReportedRuleVersion
				ar.ReportedVersion = &v
			}
			s.Rule = ar
		}
	}
	s.Apply = applyJSON{Status: a.ApplyStatus, Message: a.ApplyMessage, At: ts(a.ApplyAt), Warnings: []string{}}
	if a.ApplyVersion > 0 {
		v := a.ApplyVersion
		s.Apply.Version = &v
	}
	_ = json.Unmarshal([]byte(a.ApplyWarnings), &s.Apply.Warnings)
	if s.Apply.Warnings == nil {
		s.Apply.Warnings = []string{}
	}
	if a.AuditdJSON != "" {
		var st modules.AuditdStatus
		if json.Unmarshal([]byte(a.AuditdJSON), &st) == nil {
			s.Auditd = &st
			if st.HaltActions == nil {
				st.HaltActions = []string{}
			}
		}
	}
	if tdb, err := c.targetDB(a); err == nil {
		s.UnackedAlerts, _ = models.CountUnackedAlerts(tdb)
	}
	return s
}

// latestCAFingerprints returns the CA fingerprints every agent should have.
func (c *core) latestCAFingerprints() []string {
	var out []string
	for _, ca := range c.trustedCAs() {
		if ca.Status == models.CertActive || ca.Status == models.CertNext {
			out = append(out, ca.Fingerprint)
		}
	}
	return out
}

func (c *core) rulesByID() map[int64]*models.Rule {
	m := map[int64]*models.Rule{}
	if list, err := models.ListRules(c.db); err == nil {
		for _, r := range list {
			m[r.ID] = r
		}
	}
	return m
}

func (c *core) handleListAgents(w http.ResponseWriter, r *http.Request) {
	list, err := models.ListAgents(c.db)
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	cas := c.latestCAFingerprints()
	rules := c.rulesByID()
	out := make([]agentSummaryJSON, 0, len(list))
	for _, a := range list {
		out = append(out, c.agentSummary(a, cas, rules))
	}
	writeJSON(w, http.StatusOK, out)
}

func (c *core) loadAgent(w http.ResponseWriter, r *http.Request, name string) *models.Agent {
	id, ok := pathID(r, name)
	if !ok {
		writeError(w, http.StatusNotFound, "agent not found")
		return nil
	}
	a, err := models.GetAgent(c.db, id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "agent not found")
		return nil
	}
	if err != nil {
		c.internalError(w, r, err)
		return nil
	}
	return a
}

func (c *core) handleGetAgent(w http.ResponseWriter, r *http.Request) {
	a := c.loadAgent(w, r, "id")
	if a == nil {
		return
	}
	out := agentDetailJSON{agentSummaryJSON: c.agentSummary(a, c.latestCAFingerprints(), c.rulesByID()),
		TokenHistory: []historyJSON{}, DroppedTotal: a.DroppedTotal}
	var hi *modules.HostInfo
	if tdb, err := c.targetDB(a); err == nil {
		if rep, err := models.LatestHostReport(tdb); err == nil {
			var h modules.HostInfo
			if json.Unmarshal([]byte(rep.HostInfo), &h) == nil {
				hi = &h
			}
		}
	}
	if hi != nil {
		hj := &hostInfoJSON{CollectedAt: ts(hi.CollectedAt), Hostname: hi.Hostname, OSName: hi.OSName, OSVersion: hi.OSVersion,
			Kernel: hi.Kernel, AgentVersion: hi.AgentVersion, Auditd: auditdOut(hi.Auditd),
			ScanBase: hi.ScanBase, Directories: nonNil(hi.Directories), DirectoriesTruncated: hi.DirectoriesTruncated,
			Rule: hi.Rule, LastApply: applyOut(hi.LastApply), TrustedCAs: []trustedCAJSON{}}
		for _, t := range hi.TrustedCAs {
			hj.TrustedCAs = append(hj.TrustedCAs, trustedCAJSON{Fingerprint: t.Fingerprint, NotAfter: ts(t.NotAfter)})
		}
		out.HostInfo = hj
	}
	name := out.DisplayName
	if a.Kind == models.AgentToken {
		items, _, err := models.ListHistory(c.db, models.HistToken, name, 100, 0)
		if err == nil {
			for _, h := range items {
				out.TokenHistory = append(out.TokenHistory, historyOut(h))
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// --- tokens ---

type tokenJSON struct {
	ID                  int64   `json:"id"`
	Name                string  `json:"name"`
	Value               string  `json:"value"` // full for the CLI, shortened for the browser
	Status              string  `json:"status"`
	ExpiresAt           *string `json:"expires_at"`
	CreatedAt           *string `json:"created_at"`
	CreatedBy           string  `json:"created_by"`
	LastSeenAt          *string `json:"last_seen_at"`
	InstanceID          string  `json:"instance_id"`
	BoundAt             *string `json:"bound_at"`
	ReissuedAt          *string `json:"reissued_at"`
	RenewedAt           *string `json:"renewed_at"`
	PendingRenewal      bool    `json:"pending_renewal"`
	ExpiringUnconnected bool    `json:"expiring_unconnected"`
	AgentID             int64   `json:"agent_id"`
}

// tokenOut converts a token for the API. The value is given in full only to
// the management CLI; the browser screens receive a shortened form.
func (c *core) tokenOut(r *http.Request, t *models.Token) tokenJSON {
	now := time.Now().Unix()
	out := tokenJSON{ID: t.ID, Name: t.Name, Value: tokenValueFor(r, t.Value), Status: t.Status(now), ExpiresAt: ts(t.ExpiresAt), CreatedAt: ts(t.CreatedAt),
		CreatedBy: t.CreatedBy, LastSeenAt: ts(t.LastSeenAt), InstanceID: t.InstanceID, BoundAt: ts(t.BoundAt),
		ReissuedAt: ts(t.ReissuedAt), RenewedAt: ts(t.RenewedAt), PendingRenewal: t.PendingHash != "" && t.PendingExpiresAt > now}
	if out.Status == models.TokenUnused || out.Status == models.TokenActive {
		near := t.ExpiresAt-now < int64(c.conf().TokenRenewBeforeDays)*86400
		stale := t.LastSeenAt == 0 || now-t.LastSeenAt > int64(3*c.conf().DefaultCheckIntervalSec)+600
		out.ExpiringUnconnected = near && stale
	}
	if a, err := models.GetAgentByToken(c.db, t.ID); err == nil {
		out.AgentID = a.ID
	}
	return out
}

func (c *core) handleListTokens(w http.ResponseWriter, r *http.Request) {
	list, err := models.ListTokens(c.db)
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	out := make([]tokenJSON, 0, len(list))
	for _, t := range list {
		out = append(out, c.tokenOut(r, t))
	}
	writeJSON(w, http.StatusOK, out)
}

func (c *core) loadToken(w http.ResponseWriter, r *http.Request) *models.Token {
	id, ok := pathID(r, "id")
	if !ok {
		writeError(w, http.StatusNotFound, "token not found")
		return nil
	}
	t, err := models.GetToken(c.db, id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "token not found")
		return nil
	}
	if err != nil {
		c.internalError(w, r, err)
		return nil
	}
	return t
}

func (c *core) handleGetToken(w http.ResponseWriter, r *http.Request) {
	if t := c.loadToken(w, r); t != nil {
		writeJSON(w, http.StatusOK, c.tokenOut(r, t))
	}
}

func (c *core) validDays(v *int) (int, error) {
	if v == nil || *v == 0 {
		return c.conf().TokenValidDays, nil
	}
	if *v < 1 || *v > 3650 {
		return 0, errors.New("valid_days must be between 1 and 3650")
	}
	return *v, nil
}

func (c *core) handleIssueToken(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name        string `json:"name"`
		ValidDays   *int   `json:"valid_days"`
		RuleID      *int64 `json:"rule_id"`
		RuleVersion *int   `json:"rule_version"`
	}
	if err := readJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if !tokenNameRe.MatchString(in.Name) {
		writeError(w, http.StatusBadRequest, "token name must be 1-64 characters of A-Z a-z 0-9 . _ -")
		return
	}
	days, err := c.validDays(in.ValidDays)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	agent := &models.Agent{Kind: models.AgentToken}
	if in.RuleID != nil && *in.RuleID > 0 {
		rule, err := models.GetRule(c.db, *in.RuleID)
		if err != nil {
			writeError(w, http.StatusBadRequest, "rule not found")
			return
		}
		ver := rule.LatestVersion
		if in.RuleVersion != nil && *in.RuleVersion > 0 {
			ver = *in.RuleVersion
		}
		if _, err := models.GetRuleVersion(c.db, rule.ID, ver); err != nil {
			writeError(w, http.StatusBadRequest, "rule version not found")
			return
		}
		agent.AssignedRuleID = sql.NullInt64{Int64: rule.ID, Valid: true}
		agent.AssignedRuleVersion = ver
		agent.AssignedAt = time.Now().Unix()
	}
	value, err := modules.GenerateSecret(modules.TokenPrefix)
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	p := principalFrom(r)
	tok := &models.Token{Name: in.Name, Hash: modules.HashSecret(value), Value: value, ExpiresAt: time.Now().Unix() + int64(days)*86400, CreatedBy: p.Name}
	if err := models.InsertToken(c.db, tok); err != nil {
		if isUniqueErr(err) {
			writeError(w, http.StatusConflict, "a token with this name already exists")
			return
		}
		c.internalError(w, r, err)
		return
	}
	agent.TokenID = sql.NullInt64{Int64: tok.ID, Valid: true}
	if err := c.createAgent(agent); err != nil {
		// Undo the token, so no token is left without its agent (the agent
		// row, if it was created, goes with it).
		if derr := models.DeleteToken(c.db, tok.ID); derr != nil {
			c.logger.Printf("token %d: undo after a failed issue: %v", tok.ID, derr)
		}
		c.internalError(w, r, err)
		return
	}
	detail := "expires " + time.Unix(tok.ExpiresAt, 0).UTC().Format(time.RFC3339)
	if agent.AssignedRuleID.Valid {
		detail += "; rule id " + itoa64(agent.AssignedRuleID.Int64) + " version " + itoa(agent.AssignedRuleVersion)
	}
	c.history(p, models.HistToken, "issued", tok.Name, detail)
	writeJSON(w, http.StatusOK, map[string]any{"token": c.tokenOut(r, tok), "value": value})
}

func (c *core) handleReissueToken(w http.ResponseWriter, r *http.Request) {
	t := c.loadToken(w, r)
	if t == nil {
		return
	}
	var in struct {
		ValidDays *int `json:"valid_days"`
	}
	if r.ContentLength != 0 {
		if err := readJSON(r, &in); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	days, err := c.validDays(in.ValidDays)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	value, err := modules.GenerateSecret(modules.TokenPrefix)
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	now := time.Now().Unix()
	// Every previous value (current and pending renewal) stops working.
	t.Hash, t.Value = modules.HashSecret(value), value
	t.ExpiresAt = now + int64(days)*86400
	t.PendingHash, t.PendingExpiresAt = "", 0
	t.UsedAt, t.RevokedAt, t.ReissuedAt = 0, 0, now
	if err := models.SaveToken(c.db, t); err != nil {
		c.internalError(w, r, err)
		return
	}
	c.history(principalFrom(r), models.HistToken, "reissued", t.Name, "previous values invalidated; expires "+time.Unix(t.ExpiresAt, 0).UTC().Format(time.RFC3339))
	writeJSON(w, http.StatusOK, map[string]any{"token": c.tokenOut(r, t), "value": value})
}

func (c *core) handleUnbindToken(w http.ResponseWriter, r *http.Request) {
	t := c.loadToken(w, r)
	if t == nil {
		return
	}
	prev := t.InstanceID
	t.InstanceID, t.BoundAt = "", 0
	if err := models.SaveToken(c.db, t); err != nil {
		c.internalError(w, r, err)
		return
	}
	c.history(principalFrom(r), models.HistToken, "unbound", t.Name, "binding to the agent instance removed (was "+prev+")")
	writeJSON(w, http.StatusOK, c.tokenOut(r, t))
}

func (c *core) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	t := c.loadToken(w, r)
	if t == nil {
		return
	}
	if t.RevokedAt == 0 {
		t.RevokedAt = time.Now().Unix()
		t.PendingHash, t.PendingExpiresAt = "", 0
		if err := models.SaveToken(c.db, t); err != nil {
			c.internalError(w, r, err)
			return
		}
		c.history(principalFrom(r), models.HistToken, "revoked", t.Name, "")
	}
	writeJSON(w, http.StatusOK, c.tokenOut(r, t))
}

// tokenValueFor returns a stored token value as the caller may see it: in
// full for the management CLI, shortened for the browser screens. The
// responses of issuing and reissuing carry the new value in full for both, so
// the administrator can copy it once.
func tokenValueFor(r *http.Request, value string) string {
	if p := principalFrom(r); p != nil && p.ViaCLI {
		return value
	}
	return modules.ShortenSecret(value)
}
