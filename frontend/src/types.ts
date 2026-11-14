import type { LangSetting } from './constants';
// Shapes of the JSON exchanged with the server (see the API specification in Documents/).
// Dates are RFC3339 strings in UTC; unset values are null.

/** GET /api/v1/health */
export interface Health {
    status: string;
    version: string;
    /** Days a "keep me logged in" session lasts. */
    remember_days: number;
    /** Days a browser trusted at the login code step needs no code. */
    trust_days: number;
}

// --- Texts for display ---

/**
 * A text of the server for display: a stable code and its parameters, sent next to the English text in a
 * field named "<field>_i18n". Code '' is a text that is data only (params.text, shown as it is).
 * Parameter values are strings, numbers, booleans, nested messages or lists of these.
 */
export interface ServerMsg {
    code: string;
    params?: Record<string, unknown> | null;
}

/** A message with its location (such as "command 2: timeout_sec"), the parts in order. */
export interface ServerProblem extends ServerMsg {
    where?: ServerMsg[] | null;
}

// --- Authentication ---

export type Role = 'admin' | 'viewer';

export interface Me {
    id: number;
    /** Login ID. */
    email: string;
    display_name: string;
    role: Role;
    language: LangSetting;
}

/** POST /auth/login answers 202 with this when a code was mailed. */
export interface LoginChallenge {
    two_step: true;
    /** Opaque value passed to POST /auth/login/verify. */
    challenge: string;
}

/** POST /auth/email: applied is true when the address changed at once. */
export interface EmailChangeStart {
    applied: boolean;
}

/**
 * Who did an operation (history account_name, created_by, acknowledged_by).
 * Administrators get the recorded e-mail address; viewers get the account's
 * current display name, or '' when no account has that address any more.
 * Values without '@' (system, cli, local:<user>) are sent as recorded.
 */
export type Operator = string;

// --- Overview ---

export interface Overview {
    version: string;
    central_hostname: string;
    // enabled is false when the configuration disables the built-in agent.
    builtin_agent: { enabled: boolean; running: boolean; message: string; message_i18n?: ServerMsg | null };
    agents: number;
    unacked_alerts: number;
}

// --- Agents ---

export type ApplyStatus = 'none' | 'pending' | 'ok' | 'warning' | 'error';
export type TokenStatus = 'unused' | 'active' | 'expired' | 'revoked';

export interface TokenRef {
    id: number;
    name: string;
    status: TokenStatus;
    expires_at: string;
}

export interface AuditdStatus {
    running: boolean;
    enabled: number;
    pid: number;
    lost: number;
    backlog: number;
    backlog_limit: number;
    backlog_wait_time: number;
    failure: number;
    never_rule: boolean;
    halt_actions: string[] | null;
}

export interface ApplyResult {
    status: ApplyStatus;
    message: string;
    version: number | null;
    at: string | null;
    warnings: string[] | null;
    /** Absent for agents of older builds: the English is shown. */
    message_i18n?: ServerMsg | null;
    /** Same order as warnings. */
    warnings_i18n?: ServerMsg[] | null;
}

export interface AgentRuleRef {
    id: number;
    name: string;
    assigned_version: number;
    reported_version: number | null;
    latest_version: number;
    outdated: boolean;
}

export interface AgentSummary {
    id: number;
    kind: 'builtin' | 'token';
    display_name: string;
    hostname: string;
    token: TokenRef | null;
    last_report_at: string | null;
    seconds_since_report: number | null;
    rule: AgentRuleRef | null;
    apply: ApplyResult;
    unacked_alerts: number;
    auditd: AuditdStatus | null;
    ca_up_to_date: boolean;
    builtin_running: boolean | null;
}

export interface HostDirectory {
    path: string;
    owner: string;
    group: string;
    mode: string;
    depth: number;
}

export interface HostInfo {
    collected_at: string;
    hostname: string;
    os_name: string;
    os_version: string;
    kernel: string;
    agent_version: string;
    auditd: AuditdStatus;
    scan_base: string;
    directories: HostDirectory[] | null;
    directories_truncated: boolean;
    rule: { id: number; version: number } | null;
    last_apply: ApplyResult;
    trusted_cas: { fingerprint: string; not_after: string }[] | null;
    /** File scans whose pass is split over several checks and not complete yet. */
    incomplete_scans?: IncompleteScan[] | null;
    /** Worker count the agent reported in use (0 when it has not reported it). */
    workers?: number;
}

/** A file scan that reached its entry limit; the rest is scanned in the following checks. */
export interface IncompleteScan {
    kind: 'writable' | 'exec_files';
    /** Path where the next check continues. */
    resume_at: string;
    /** Entries scanned per check. */
    limit: number;
    /** Start of the split pass. */
    started_at: string | null;
}

export interface AgentDetail extends AgentSummary {
    host_info: HostInfo | null;
    token_history: HistoryItem[] | null;
    dropped_total: number;
}

// --- Tokens ---

export interface Token {
    id: number;
    name: string;
    /**
     * Shortened value ("mmr_AbCd...WxYz"); the full value is only in the
     * issue/reissue response. Absent for viewers.
     */
    value?: string;
    status: TokenStatus;
    expires_at: string;
    created_at: string;
    created_by: Operator;
    last_seen_at: string | null;
    instance_id: string;
    bound_at: string | null;
    reissued_at: string | null;
    renewed_at: string | null;
    pending_renewal: boolean;
    expiring_unconnected: boolean;
    agent_id: number;
}

export interface TokenIssueRequest {
    name: string;
    valid_days?: number;
    rule_id?: number;
    rule_version?: number;
}

export interface TokenWithValue {
    token: Token;
    /** The new value in full; the browser receives it only in this response. */
    value: string;
}

// --- Monitoring rules ---

/** '' means "not specified". */
export type Category = 'alert' | 'normal' | '';
export type IfMissing = 'ignore' | 'warn';

export interface RuleElement {
    value: string;
    if_missing: IfMissing;
}

export interface LegitLocation {
    path: string;
    extensions: string[];
    category: Category;
}

export interface RuleContent {
    users: RuleElement[];
    exclude_interactive: boolean | null;
    check_interval_sec: number;
    scan_interval_sec: number;
    exec: { category: Category };
    denied: { category: Category };
    read: { dirs: RuleElement[]; category: Category };
    network: { executables: RuleElement[]; category: Category };
    writable: { base: string; allowed_paths: string[]; category: Category };
    exec_files: { extensions: string[]; legit: LegitLocation[]; other_category: Category };
}

export interface AppliedAgent {
    agent_id: number;
    display_name: string;
    version: number;
}

export interface RuleSummary {
    id: number;
    name: string;
    description: string;
    latest_version: number;
    created_at: string;
    updated_at: string;
    applied_agents: AppliedAgent[] | null;
}

export interface RuleVersionInfo {
    version: number;
    created_at: string;
    created_by: Operator;
    comment: string;
    /** Only for a comment the program wrote (version 1). */
    comment_i18n?: ServerMsg | null;
}

export interface RuleDetail extends RuleSummary {
    content: RuleContent;
    versions: RuleVersionInfo[] | null;
}

export interface RuleVersion {
    rule_id: number;
    version: number;
    content: RuleContent;
    created_at: string;
    created_by: Operator;
    comment: string;
    comment_i18n?: ServerMsg | null;
}

export interface DiffLine {
    op: ' ' | '+' | '-';
    text: string;
}

export interface RuleDiff {
    from: number;
    to: number;
    lines: DiffLine[] | null;
}

export interface MergeConflict {
    field: string;
    options: { template: string; value: unknown }[];
}

export interface MergePreview {
    content: RuleContent;
    conflicts: MergeConflict[] | null;
}

export interface RuleCreate {
    name: string;
    description: string;
    templates?: string[];
    choices?: Record<string, string>;
    content?: RuleContent;
}

// --- Templates ---

export interface TemplateSummary {
    name: string;
    builtin: boolean;
    description: string;
    /** Only for built-in templates. */
    description_i18n?: ServerMsg | null;
    updated_at: string | null;
}

export interface TemplateDetail extends TemplateSummary {
    content: RuleContent;
}

export interface TemplateCreate {
    name: string;
    description: string;
    content?: RuleContent;
    from_builtin?: string;
    from_rule?: { rule_id: number; version: number };
}

// --- Detections ---

export type DetectionType = 'exec' | 'denied' | 'read' | 'network' | 'writable' | 'exec_file';
export type DetectionCategory = 'alert' | 'normal';

export interface Detection {
    id: number;
    agent_id: number;
    agent_name: string;
    hostname: string;
    type: DetectionType;
    category: DetectionCategory;
    os_user: string;
    exe: string;
    target: string;
    count: number;
    first_at: string;
    last_at: string;
    /** writable and exec_file only; ctime is null when unknown. */
    file: { ctime: string | null; owner: string; mode: string } | null;
    suppressed: boolean;
    suppression_id: number | null;
    acknowledged_at: string | null;
    acknowledged_by: Operator;
    notify_state: 'pending' | 'sent' | 'skipped';
}

export interface DetectionQuery {
    agent_id?: string;
    category?: string;
    type?: string;
    from?: string;
    to?: string;
    acknowledged?: string;
    suppressed?: string;
    limit?: number;
    offset?: number;
}

export interface Paged<T> {
    items: T[] | null;
    total: number;
}

// --- Suppressions ---

export interface Suppression {
    id: number;
    agent_id: number | null;
    agent_name: string;
    type: DetectionType | '';
    os_user: string;
    exe: string;
    target: string;
    comment: string;
    created_at: string;
    created_by: Operator;
}

export interface SuppressionCreate {
    agent_id?: number;
    type: DetectionType | '';
    os_user: string;
    exe: string;
    target: string;
    comment: string;
}

// --- Certificates ---

export type CertState = 'active' | 'next' | 'retired';

export interface CertInfo {
    fingerprint: string;
    subject: string;
    not_before: string;
    not_after: string;
    status: CertState;
}

export interface CertStatus {
    cas: CertInfo[] | null;
    /** Absent while there is no active server certificate. */
    server?: CertInfo & { names: string[] | null; issuer_fingerprint: string };
    rotation: {
        state: 'none' | 'distributing' | 'switched';
        next_ca_created_at: string | null;
        switch_after: string | null;
    };
    agents_missing_new_ca: { agent_id: number; display_name: string }[] | null;
}

// --- History ---

export type HistoryCategory =
    | 'account'
    | 'rule'
    | 'token'
    | 'certificate'
    | 'rejected'
    | 'template'
    | 'suppression'
    | 'detection'
    | 'config'
    | 'command';

export interface HistoryItem {
    id: number;
    at: string;
    account_name: Operator;
    category: HistoryCategory;
    action: string;
    target: string;
    detail: string;
    /** Present only when the recorded text has a code (target: mostly values, which have none). */
    target_i18n?: ServerMsg | null;
    detail_i18n?: ServerMsg | null;
    remote_addr: string;
    count: number;
}

// --- Accounts ---

export interface Account {
    id: number;
    /** Login ID. */
    email: string;
    display_name: string;
    role: Role;
    language: LangSetting;
    disabled: boolean;
    /** Receives the notification mails (kept while disabled, but nothing is sent then). */
    notify: boolean;
    locked_until: string | null;
    created_at: string;
}

// --- Guide ---

export interface Guide {
    version: string;
    central_hostname: string;
    /** Host names and IP addresses the agents can use for the central. */
    central_hosts: string[] | null;
    /** Port of the central for the agents (agent_listen in effect). */
    central_port: number;
    ca_fingerprint: string;
    default_valid_days: number;
}

// --- Central configuration ---

export type ConfigKind = 'string' | 'int' | 'bool' | 'list' | 'secret';

export interface ConfigItem {
    name: string;
    kind: ConfigKind;
    /** Current stored or default value ("" for the secret item). */
    value: string;
    /** The secret item has a value. */
    is_set: boolean;
    default: string;
    /** false when the default applies. */
    stored: boolean;
    /** Takes effect when the central restarts. */
    restart: boolean;
    /** A restart item whose stored value is not in effect yet. */
    pending: boolean;
}

export interface CentralConfig {
    items: ConfigItem[];
}

export interface ConfigUpdate {
    set: Record<string, string>;
    unset: string[];
}

// --- Command sets ---
// Result types are returned to every account; the definition types are used
// only by the administrator extension (types are not part of the built files).

export type CommandKind = 'success' | 'failure' | 'error';
export type CommandRunState = 'running' | CommandKind;

/** Display value of a run: one row per group name ('' is output used without patterns). */
export interface DisplayValue {
    rows: { name: string; values: string[]; value: string }[] | null;
    truncated: boolean;
}

export interface CommandRun {
    id: number;
    set_id: number;
    set_name: string;
    started_at: string | null;
    finished_at: string | null;
    state: CommandRunState;
    reason: string;
    display: DisplayValue | null;
    received_at: string | null;
    // Present for administrators only.
    trigger?: 'manual' | 'schedule';
    scheduled_for?: string | null;
    request_id?: number;
    commands_total?: number;
    set_revision?: number;
    purpose?: string;
    os_user?: string;
    work_dir?: string;
}

export interface CommandLatest {
    agent_id: number;
    agent_name: string;
    set_id: number;
    set_name: string;
    running: boolean;
    last_run: CommandRun | null;
    purpose?: string;
}

export interface AgentCommandResults {
    agent_id: number;
    agent_name: string;
    results: CommandLatest[] | null;
    sets: CommandLatest[] | null;
}

export interface CommandResultsByName {
    names: string[] | null;
    name: string;
    results: CommandLatest[] | null;
}

export interface CommandRunList {
    agent_id: number;
    agent_name: string;
    set_id: number;
    set_name: string;
    deleted: boolean;
    runs: CommandRun[] | null;
    next_before: number | null;
    purpose?: string;
}

export interface CommandValues {
    values: { name: string; value: string }[] | null;
    truncated: boolean;
    pattern_problems?: { pattern: number; problem: 'timeout' | 'invalid' }[] | null;
}

/** One executed command of a run. */
export interface CommandRunItem {
    position: number;
    kind: CommandKind;
    reason: string;
    exit_code: number | null;
    started_at: string | null;
    finished_at: string | null;
    output: string;
    output_bytes: number;
    output_truncated: boolean;
    // Present for administrators only.
    command_line?: string;
    note?: string;
    values?: CommandValues | null;
}

export interface CommandRunDetail extends CommandRun {
    agent_id: number;
    agent_name: string;
    not_executed: number;
    commands: CommandRunItem[] | null;
}

export type CodeSpecKind = 'single' | 'range' | 'min' | 'max';
export interface CodeSpec {
    kind: CodeSpecKind;
    a: number;
    b?: number;
}

export type Weekday = 'sun' | 'mon' | 'tue' | 'wed' | 'thu' | 'fri' | 'sat';
export type ScheduleKind = 'once' | 'weekly' | 'daily' | 'every_hours' | 'every_minutes' | 'hourly_at';

export interface CommandSchedule {
    kind: ScheduleKind;
    at?: string;
    weekdays?: Weekday[];
    time?: string;
    every?: number;
    minute?: number;
}

export type OnProblems = 'stop' | 'continue_on_failure' | 'continue_always';
export type CommandPurpose = 'collect_output' | 'run_only';
export type Joiner = 'comma' | 'newline';

export interface CommandSpec {
    command_line: string;
    note: string;
    success: CodeSpec;
    failure: CodeSpec;
    on_problems: OnProblems;
    timeout_sec: number;
    line_limit: number;
    patterns: string[] | null;
}

export interface CommandSetSpec {
    name: string;
    description: string;
    purpose: CommandPurpose;
    os_user: string;
    work_dir: string;
    run_mode: 'manual' | 'schedule';
    schedule: CommandSchedule | null;
    keep_runs: number;
    joiner: Joiner;
    commands: CommandSpec[] | null;
}

export interface CommandRequest {
    id: number;
    set_id: number;
    state: 'queued' | 'pending' | 'delivered' | 'running' | 'done' | 'expired' | 'discarded';
    requested_at: string | null;
    requested_by: string;
    expires_at: string | null;
    delivered_at: string | null;
    done_at: string | null;
    run_uid: string;
    discarded_at: string | null;
    discarded_by: string;
}

export interface CommandSet extends CommandSetSpec {
    id: number;
    revision: number;
    schedule_summary: string;
    imported_from: string;
    created_at: string | null;
    created_by: string;
    updated_at: string | null;
    updated_by: string;
    request: CommandRequest | null;
    last_run: CommandRun | null;
    running: boolean;
    agent_problems: string[] | null;
    /** Same order as agent_problems. */
    agent_problems_i18n?: ServerProblem[] | null;
}

export interface CommandSetList {
    agent_id: number;
    agent_name: string;
    default_os_user: string;
    time_zone: string;
    utc_offset_sec: number;
    definitions_delivered: boolean;
    /** Whether the agent synced within three check intervals and 600 seconds; while false its run requests wait without a worker of the central. */
    agent_responding: boolean;
    sets: CommandSet[] | null;
}
