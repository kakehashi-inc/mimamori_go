package modules

// Messages exchanged between an agent and the central (/agent/v1). The
// central hands an agent rule values, its own token renewal, CA
// certificates, and the command sets an administrator defined for that
// monitored target with the pending manual run requests of those sets. The
// agent runs only the command lines of these definitions (as the OS user
// each set names); nothing here can load an arbitrary auditd rule.

// HeaderInstanceID carries the agent's instance ID (created at agent init and
// kept in its data directory) on authenticated requests; the token is bound
// to it.
const HeaderInstanceID = "X-Mimamori-Instance-Id"

// ApplyResult is the outcome of applying a rule on an agent. Message and
// Warnings are the English texts; MessageI18n and WarningsI18n are the same
// texts as messages (in the order of Warnings), for the screens. Both coded
// fields are optional: an agent of an older build sends only the English,
// and a central of an older build ignores them.
type ApplyResult struct {
	Status       string   `json:"status"` // none, ok, warning, error
	Message      string   `json:"message"`
	RuleID       int64    `json:"rule_id"`
	Version      int      `json:"version"`
	At           int64    `json:"at"`
	Warnings     []string `json:"warnings"`
	MessageI18n  *Msg     `json:"message_i18n,omitempty"`
	WarningsI18n []Msg    `json:"warnings_i18n,omitempty"`
}

// setMessage sets the message in English and as a message.
func (r *ApplyResult) setMessage(m Msg) {
	r.Message, r.MessageI18n = m.Text(), &m
}

// setWarnings sets the warnings in English and as messages.
func (r *ApplyResult) setWarnings(ws []Msg) {
	r.Warnings, r.WarningsI18n = nil, nil
	for _, w := range ws {
		r.Warnings = append(r.Warnings, w.Text())
	}
	if len(ws) > 0 {
		r.WarningsI18n = ws
	}
}

// SyncRequest is sent at the start of every cycle.
type SyncRequest struct {
	AgentVersion   string       `json:"agent_version"`
	RuleID         int64        `json:"rule_id"`
	RuleVersion    int          `json:"rule_version"`
	CAFingerprints []string     `json:"ca_fingerprints"`
	LastApply      *ApplyResult `json:"last_apply"`
	// CommandSig is the signature of the command set definitions the agent
	// holds ("" when it holds none).
	CommandSig string `json:"command_sig"`
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
	// CommandSets carries every command set definition of the target when
	// the agent's signature differs from the central's.
	CommandSets *CommandDefinitions `json:"command_sets,omitempty"`
	// CommandRequests are the manual run requests waiting for the agent.
	CommandRequests []CommandRunRequest `json:"command_requests,omitempty"`
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
	// CommandRuns carries started and finished runs of command sets with
	// the results of their finished commands. The central stores them
	// idempotently (run UID, command position).
	CommandRuns []CommandRunReport `json:"command_runs,omitempty"`
	// CommandRequestAcks answers delivered manual run requests.
	CommandRequestAcks []CommandRequestAck `json:"command_request_acks,omitempty"`
	// CommandApply reports the outcome of storing delivered definitions.
	CommandApply *CommandApplyResult `json:"command_apply,omitempty"`
}

// CommandDefinitions are the command set definitions of a target with their
// signature (see CommandSignature).
type CommandDefinitions struct {
	Signature string          `json:"signature"`
	Sets      []CommandSetDef `json:"sets"`
}

// CommandRunRequest is a manual run request of a set.
type CommandRunRequest struct {
	ID          int64 `json:"id"`
	SetID       int64 `json:"set_id"`
	RequestedAt int64 `json:"requested_at"`
	ExpiresAt   int64 `json:"expires_at"`
}

// Triggers of a run.
const (
	TriggerManual   = "manual"
	TriggerSchedule = "schedule"
)

// Statuses of a CommandRequestAck.
const (
	RequestAckStarted    = "started"     // a run was started for it (RunUID)
	RequestAckExpired    = "expired"     // it arrived after its expiry and was not run
	RequestAckUnknownSet = "unknown_set" // the agent holds no valid set with its SetID
)

// CommandRequestAck answers a delivered manual run request.
type CommandRequestAck struct {
	RequestID int64  `json:"request_id"`
	Status    string `json:"status"`
	RunUID    string `json:"run_uid,omitempty"`
	At        int64  `json:"at"`
}

// CommandRunReport is a run of a command set: sent when it starts, after
// each command and when it finishes. The set's values at the start are
// copied so that the result does not depend on later changes.
type CommandRunReport struct {
	RunUID        string `json:"run_uid"`
	SetID         int64  `json:"set_id"`
	SetRevision   int64  `json:"set_revision"`
	SetName       string `json:"set_name"`
	Purpose       string `json:"purpose"`
	Joiner        string `json:"joiner"`
	OSUser        string `json:"os_user"`
	WorkDir       string `json:"work_dir"`
	Trigger       string `json:"trigger"`
	RequestID     int64  `json:"request_id"`
	ScheduledFor  int64  `json:"scheduled_for"`
	CommandsTotal int    `json:"commands_total"`
	StartedAt     int64  `json:"started_at"`
	// Finished is set on the last report of the run, with FinishedAt, State
	// (the worst result kind of the executed commands), Reason (for a run
	// that ended without running its commands, such as one found
	// interrupted) and Display (collect_output only).
	Finished   bool                  `json:"finished"`
	FinishedAt int64                 `json:"finished_at"`
	State      string                `json:"state"`
	Reason     string                `json:"reason"`
	Display    *DisplayValue         `json:"display,omitempty"`
	Results    []CommandResultReport `json:"results"`
}

// CommandResultReport is the result of one executed command. Output is the
// stored console output (up to MaxCommandOutputBytes, base64 in JSON);
// OutputBytes is the total size the command wrote.
type CommandResultReport struct {
	Position    int            `json:"position"`
	CommandLine string         `json:"command_line"`
	Note        string         `json:"note"`
	Kind        string         `json:"kind"`
	Reason      string         `json:"reason"`
	ExitCode    *int           `json:"exit_code"`
	StartedAt   int64          `json:"started_at"`
	FinishedAt  int64          `json:"finished_at"`
	Output      []byte         `json:"output"`
	OutputBytes int64          `json:"output_bytes"`
	Truncated   bool           `json:"truncated"`
	Values      *CommandValues `json:"values,omitempty"`
}

// CommandApplyProblem names a delivered set the agent did not accept.
// Problems are the English texts; ProblemsI18n are the same problems with
// their codes in the same order (optional: an agent of an older build sends
// only the English).
type CommandApplyProblem struct {
	SetID        int64     `json:"set_id"`
	SetName      string    `json:"set_name"`
	Problems     []string  `json:"problems"`
	ProblemsI18n []Problem `json:"problems_i18n,omitempty"`
}

// CommandApplyResult reports the definitions the agent stored (Signature)
// and the sets it refused (they are not run).
type CommandApplyResult struct {
	Signature string                `json:"signature"`
	At        int64                 `json:"at"`
	Problems  []CommandApplyProblem `json:"problems"`
}

// CAResponse answers GET /agent/v1/ca.
type CAResponse struct {
	CAPEMs []string `json:"ca_pems"`
}
