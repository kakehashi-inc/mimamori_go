package models

import "database/sql"

// Agent kinds.
const (
	AgentBuiltin = "builtin"
	AgentToken   = "token"
)

// Agent is a row of the agents table: one monitored target (an agent
// identified by its token, or the built-in agent). Detections and host
// reports are kept in the target's own database file (DBFile).
type Agent struct {
	ID                  int64
	Kind                string
	TokenID             sql.NullInt64
	DBFile              string
	Hostname            string
	OSName              string
	OSVersion           string
	AgentVersion        string
	AssignedRuleID      sql.NullInt64
	AssignedRuleVersion int
	AssignedAt          int64
	ReportedRuleID      int64
	ReportedRuleVersion int
	ApplyStatus         string
	ApplyMessage        string
	ApplyWarnings       string
	ApplyVersion        int
	ApplyAt             int64
	CAFingerprints      string
	AuditdJSON          string
	LastSyncAt          int64
	LastReportAt        int64
	DroppedTotal        int64
	// HostOSUser is the OS user the agent runs as; TimeZone and
	// UTCOffsetSec describe the host's local time (all from its host
	// information). CommandSig is the signature of the command set
	// definitions the agent reported holding; CommandProblems is the JSON
	// of the last outcome the agent reported for storing delivered
	// definitions (modules.CommandApplyResult, "" when none).
	HostOSUser      string
	TimeZone        string
	UTCOffsetSec    int
	CommandSig      string
	CommandProblems string
	CreatedAt       int64
}

const agentCols = `id, kind, token_id, db_file, hostname, os_name, os_version, agent_version, assigned_rule_id, assigned_rule_version,
	assigned_at, reported_rule_id, reported_rule_version, apply_status, apply_message, apply_warnings, apply_version, apply_at,
	ca_fingerprints, auditd_json, last_sync_at, last_report_at, dropped_total, host_os_user, time_zone, utc_offset_sec, command_sig, command_problems, created_at`

func scanAgent(s rowScanner) (*Agent, error) {
	a := &Agent{}
	if err := s.Scan(&a.ID, &a.Kind, &a.TokenID, &a.DBFile, &a.Hostname, &a.OSName, &a.OSVersion, &a.AgentVersion,
		&a.AssignedRuleID, &a.AssignedRuleVersion, &a.AssignedAt, &a.ReportedRuleID, &a.ReportedRuleVersion, &a.ApplyStatus,
		&a.ApplyMessage, &a.ApplyWarnings, &a.ApplyVersion, &a.ApplyAt, &a.CAFingerprints, &a.AuditdJSON, &a.LastSyncAt,
		&a.LastReportAt, &a.DroppedTotal, &a.HostOSUser, &a.TimeZone, &a.UTCOffsetSec, &a.CommandSig, &a.CommandProblems, &a.CreatedAt); err != nil {
		return nil, err
	}
	return a, nil
}

// InsertAgent creates an agent row. DBFile is derived from the id after insert.
func InsertAgent(db Execer, a *Agent) error {
	a.CreatedAt = Now()
	a.ApplyStatus = "none"
	if a.AssignedRuleID.Valid {
		a.ApplyStatus = "pending"
	}
	res, err := db.Exec(`INSERT INTO agents (kind, token_id, db_file, assigned_rule_id, assigned_rule_version, assigned_at, apply_status, created_at)
		VALUES (?, ?, '', ?, ?, ?, ?, ?)`, a.Kind, a.TokenID, a.AssignedRuleID, a.AssignedRuleVersion, a.AssignedAt, a.ApplyStatus, a.CreatedAt)
	if err != nil {
		return err
	}
	a.ID, _ = res.LastInsertId()
	return nil
}

// SetAgentDBFile records the per-target database file name.
func SetAgentDBFile(db Execer, id int64, file string) error {
	return execOne(db, `UPDATE agents SET db_file = ? WHERE id = ?`, file, id)
}

// GetAgent returns an agent by id.
func GetAgent(db Execer, id int64) (*Agent, error) {
	return scanAgent(db.QueryRow(`SELECT `+agentCols+` FROM agents WHERE id = ?`, id))
}

// GetAgentByToken returns the agent of a token.
func GetAgentByToken(db Execer, tokenID int64) (*Agent, error) {
	return scanAgent(db.QueryRow(`SELECT `+agentCols+` FROM agents WHERE token_id = ?`, tokenID))
}

// GetBuiltinAgent returns the built-in agent row.
func GetBuiltinAgent(db Execer) (*Agent, error) {
	return scanAgent(db.QueryRow(`SELECT ` + agentCols + ` FROM agents WHERE kind = 'builtin' ORDER BY id LIMIT 1`))
}

// ListAgents returns all agents, the built-in agent first.
func ListAgents(db Execer) ([]*Agent, error) {
	rows, err := db.Query(`SELECT ` + agentCols + ` FROM agents ORDER BY CASE kind WHEN 'builtin' THEN 0 ELSE 1 END, id`)
	return collect(rows, err, scanAgent)
}

// AssignRule sets the rule version an agent should apply.
func AssignRule(db Execer, id, ruleID int64, version int) error {
	return execOne(db, `UPDATE agents SET assigned_rule_id = ?, assigned_rule_version = ?, assigned_at = ?,
		apply_status = CASE WHEN reported_rule_id = ? AND reported_rule_version = ? THEN apply_status ELSE 'pending' END WHERE id = ?`,
		ruleID, version, Now(), ruleID, version, id)
}

// UnassignRule takes the assigned rule away from an agent. The application
// status becomes pending until the agent reports that nothing is applied,
// or none when the agent reported no applied rule and no failure.
func UnassignRule(db Execer, id int64) error {
	return execOne(db, `UPDATE agents SET assigned_rule_id = NULL, assigned_rule_version = 0, assigned_at = ?,
		apply_status = CASE WHEN reported_rule_id = 0 AND apply_status IN ('none', 'pending') THEN 'none' ELSE 'pending' END WHERE id = ?`,
		Now(), id)
}

// SetAgentApplyNone records that nothing is applied on the agent (the
// central removed the audit rules of the disabled built-in agent).
func SetAgentApplyNone(db Execer, id int64, message string, at int64) error {
	return execOne(db, `UPDATE agents SET apply_status = 'none', apply_message = ?, apply_warnings = '[]', apply_version = 0, apply_at = ?,
		reported_rule_id = 0, reported_rule_version = 0 WHERE id = ?`, message, at, id)
}

// CountAgentsUsingRule counts agents a rule is assigned to.
func CountAgentsUsingRule(db Execer, ruleID int64) (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM agents WHERE assigned_rule_id = ?`, ruleID).Scan(&n)
	return n, err
}

// RecordAgentSync stores the state reported in a sync request.
func RecordAgentSync(db Execer, id int64, agentVersion string, ruleID int64, ruleVersion int, caFPs string, now int64) error {
	_, err := db.Exec(`UPDATE agents SET agent_version = ?, reported_rule_id = ?, reported_rule_version = ?, ca_fingerprints = ?, last_sync_at = ? WHERE id = ?`,
		agentVersion, ruleID, ruleVersion, caFPs, now, id)
	return err
}

// AgentReportFields are the values a report updates on the agent row.
type AgentReportFields struct {
	Hostname, OSName, OSVersion, AgentVersion, AuditdJSON string
	HostOSUser, TimeZone                                  string
	UTCOffsetSec                                          int
	ApplyStatus, ApplyMessage, ApplyWarnings              string
	ApplyVersion                                          int
	ApplyAt                                               int64
	ReportedRuleID                                        int64
	ReportedRuleVersion                                   int
	HasHost, HasApply                                     bool
	Dropped                                               int64
	Now                                                   int64
}

// RecordAgentReport stores the summary of a report on the agent row.
func RecordAgentReport(db Execer, id int64, f AgentReportFields) error {
	if f.HasHost {
		if _, err := db.Exec(`UPDATE agents SET hostname = ?, os_name = ?, os_version = ?, agent_version = ?, auditd_json = ?,
			host_os_user = ?, time_zone = ?, utc_offset_sec = ? WHERE id = ?`,
			f.Hostname, f.OSName, f.OSVersion, f.AgentVersion, f.AuditdJSON, f.HostOSUser, f.TimeZone, f.UTCOffsetSec, id); err != nil {
			return err
		}
	}
	if f.HasApply {
		if _, err := db.Exec(`UPDATE agents SET apply_status = ?, apply_message = ?, apply_warnings = ?, apply_version = ?, apply_at = ?,
			reported_rule_id = ?, reported_rule_version = ? WHERE id = ?`,
			f.ApplyStatus, f.ApplyMessage, f.ApplyWarnings, f.ApplyVersion, f.ApplyAt, f.ReportedRuleID, f.ReportedRuleVersion, id); err != nil {
			return err
		}
	}
	_, err := db.Exec(`UPDATE agents SET last_report_at = ?, dropped_total = dropped_total + ? WHERE id = ?`, f.Now, f.Dropped, id)
	return err
}

// RecordAgentCommandSig stores the signature of the command set definitions
// an agent reported holding.
func RecordAgentCommandSig(db Execer, id int64, sig string) error {
	_, err := db.Exec(`UPDATE agents SET command_sig = ? WHERE id = ?`, sig, id)
	return err
}

// RecordAgentCommandApply stores the outcome an agent reported for storing
// delivered command set definitions (JSON of modules.CommandApplyResult).
func RecordAgentCommandApply(db Execer, id int64, applyJSON string) error {
	_, err := db.Exec(`UPDATE agents SET command_problems = ? WHERE id = ?`, applyJSON, id)
	return err
}
