package modules

// Messages exchanged between an agent and the central (/agent/v1). The
// central can hand an agent only rule values, its own token renewal and CA
// certificates; nothing here can make the agent run a command or load an
// arbitrary auditd rule.

// HeaderMachineID carries the host's machine id on authenticated requests.
const HeaderMachineID = "X-Mimamori-Machine-Id"

// ApplyResult is the outcome of applying a rule on an agent.
type ApplyResult struct {
	Status   string   `json:"status"` // none, ok, warning, error
	Message  string   `json:"message"`
	RuleID   int64    `json:"rule_id"`
	Version  int      `json:"version"`
	At       int64    `json:"at"`
	Warnings []string `json:"warnings"`
}

// SyncRequest is sent at the start of every cycle.
type SyncRequest struct {
	AgentVersion   string       `json:"agent_version"`
	RuleID         int64        `json:"rule_id"`
	RuleVersion    int          `json:"rule_version"`
	CAFingerprints []string     `json:"ca_fingerprints"`
	LastApply      *ApplyResult `json:"last_apply"`
}

// RuleAssignment is the rule version assigned to an agent.
type RuleAssignment struct {
	ID      int64       `json:"id"`
	Name    string      `json:"name"`
	Version int         `json:"version"`
	Content RuleContent `json:"content"`
}

// TokenRenewal carries the automatically renewed token.
type TokenRenewal struct {
	Token     string `json:"token"`
	ExpiresAt int64  `json:"expires_at"`
}

// SyncResponse answers a SyncRequest.
type SyncResponse struct {
	AgentName          string          `json:"agent_name"`
	TokenExpiresAt     int64           `json:"token_expires_at"`
	Rule               *RuleAssignment `json:"rule"`
	DefaultIntervalSec int             `json:"default_interval_sec"`
	TokenRenewal       *TokenRenewal   `json:"token_renewal,omitempty"`
	CAPEMs             []string        `json:"ca_pems,omitempty"`
}

// DetectionReport is one aggregated detection sent by an agent.
type DetectionReport struct {
	Type      string `json:"type"`
	Category  string `json:"category"`
	OSUser    string `json:"os_user"`
	Exe       string `json:"exe"`
	Target    string `json:"target"`
	Count     int    `json:"count"`
	FirstAt   int64  `json:"first_at"`
	LastAt    int64  `json:"last_at"`
	FileCtime int64  `json:"file_ctime,omitempty"`
	FileOwner string `json:"file_owner,omitempty"`
	FileMode  string `json:"file_mode,omitempty"`
}

// ReportRequest is sent at the end of every cycle (or later from the outbox).
type ReportRequest struct {
	CreatedAt  int64             `json:"created_at"`
	Detections []DetectionReport `json:"detections"`
	HostInfo   *HostInfo         `json:"host_info,omitempty"`
	Apply      *ApplyResult      `json:"apply,omitempty"`
	Dropped    int               `json:"dropped"`
}

// CAResponse answers GET /agent/v1/ca.
type CAResponse struct {
	CAPEMs []string `json:"ca_pems"`
}

// Agent transport errors that the agent treats specially.
var (
	// ErrUnauthorized means the token or machine binding was rejected.
	ErrUnauthorized = &HTTPError{Status: 401, Body: `{"error":"unauthorized"}`}
)
