import type { LangSetting } from './constants';
// Shapes of the JSON exchanged with the server (see the API specification in Documents/).
// Dates are RFC3339 strings in UTC; unset values are null.

/** GET /api/v1/health */
export interface Health {
    status: string;
    version: string;
    /** Days a "keep me logged in" session lasts. */
    remember_days: number;
}

// --- Authentication ---

export type Role = 'admin' | 'viewer';

export interface Me {
    id: number;
    name: string;
    role: Role;
    language: LangSetting;
}

// --- Overview ---

export interface Overview {
    version: string;
    central_hostname: string;
    builtin_agent: { running: boolean; message: string };
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
    status: TokenStatus;
    expires_at: string;
    created_at: string;
    created_by: string;
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
    created_by: string;
    comment: string;
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
    created_by: string;
    comment: string;
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
    file: { ctime: string; owner: string; mode: string } | null;
    suppressed: boolean;
    suppression_id: number | null;
    acknowledged_at: string | null;
    acknowledged_by: string;
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
    created_by: string;
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
    server: CertInfo & { names: string[] | null; issuer_fingerprint: string };
    rotation: {
        state: 'none' | 'distributing' | 'switched';
        next_ca_created_at: string | null;
        switch_after: string | null;
    };
    agents_missing_new_ca: { agent_id: number; display_name: string }[] | null;
}

// --- History ---

export type HistoryCategory =
    'account' | 'rule' | 'token' | 'certificate' | 'rejected' | 'template' | 'suppression' | 'detection' | 'config';

export interface HistoryItem {
    id: number;
    at: string;
    account_name: string;
    category: HistoryCategory;
    action: string;
    target: string;
    detail: string;
    remote_addr: string;
    count: number;
}

// --- Accounts ---

export interface Account {
    id: number;
    name: string;
    role: Role;
    language: LangSetting;
    disabled: boolean;
    locked_until: string | null;
    created_at: string;
}

// --- Guide ---

export interface Guide {
    version: string;
    central_hostname: string;
    agent_urls: string[] | null;
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
