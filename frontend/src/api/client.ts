import type {
    Account,
    AgentDetail,
    AgentSummary,
    CertStatus,
    Detection,
    DetectionQuery,
    Guide,
    Health,
    HistoryItem,
    Me,
    MergePreview,
    Overview,
    Paged,
    Role,
    RuleContent,
    RuleCreate,
    RuleDetail,
    RuleDiff,
    RuleSummary,
    RuleVersion,
    Suppression,
    SuppressionCreate,
    TemplateCreate,
    TemplateDetail,
    TemplateSummary,
    Token,
    TokenIssueRequest,
    TokenWithValue,
} from '../types';

const BASE = '/api/v1';

/** Raised for any non-2xx response, or with status 0 when the network failed. */
export class ApiError extends Error {
    status: number;
    constructor(status: number, message: string) {
        super(message);
        this.name = 'ApiError';
        this.status = status;
    }
}

// Called when any request other than the login itself answers 401, so the app
// can drop the session and show the login screen.
let unauthorizedHandler: (() => void) | null = null;

export function setUnauthorizedHandler(handler: (() => void) | null): void {
    unauthorizedHandler = handler;
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
    const method = (init?.method ?? 'GET').toUpperCase();
    const headers = new Headers(init?.headers);
    if (method !== 'GET') {
        // Required by the server on every non-GET request (CSRF protection).
        headers.set('X-Mimamori-Request', '1');
    }
    if (init?.body !== undefined && !headers.has('Content-Type')) {
        headers.set('Content-Type', 'application/json');
    }
    let res: Response;
    try {
        res = await fetch(`${BASE}${path}`, { credentials: 'same-origin', ...init, method, headers });
    } catch {
        throw new ApiError(0, 'network');
    }
    if (!res.ok) {
        let message = `HTTP ${res.status}`;
        try {
            const body = await res.json();
            if (body && typeof body.error === 'string') message = body.error;
        } catch {
            // The body is not JSON; keep the status text.
        }
        if (res.status === 401 && path !== '/auth/login' && unauthorizedHandler) {
            unauthorizedHandler();
        }
        throw new ApiError(res.status, message);
    }
    if (res.status === 204) {
        return undefined as T;
    }
    return (await res.json()) as T;
}

function send<T>(method: string, path: string, body?: unknown): Promise<T> {
    return request<T>(path, { method, body: body === undefined ? undefined : JSON.stringify(body) });
}

/** Builds "?a=1&b=2" from the defined, non-empty values. */
function query(params: Record<string, string | number | undefined>): string {
    const q = new URLSearchParams();
    for (const [key, value] of Object.entries(params)) {
        if (value === undefined || value === '') continue;
        q.set(key, String(value));
    }
    const s = q.toString();
    return s ? `?${s}` : '';
}

// --- Health and authentication ---

export function getHealth(): Promise<Health> {
    return request<Health>('/health');
}

export function login(name: string, password: string): Promise<Me> {
    return send<Me>('POST', '/auth/login', { name, password });
}

export function logout(): Promise<unknown> {
    return send('POST', '/auth/logout');
}

export function getMe(): Promise<Me> {
    return request<Me>('/auth/me');
}

export function changeOwnPassword(currentPassword: string, newPassword: string): Promise<unknown> {
    return send('POST', '/auth/password', { current_password: currentPassword, new_password: newPassword });
}

// --- Overview and agents ---

export function getOverview(): Promise<Overview> {
    return request<Overview>('/overview');
}

export function listAgents(): Promise<AgentSummary[]> {
    return request<AgentSummary[] | null>('/agents').then(v => v ?? []);
}

export function getAgent(id: number | string): Promise<AgentDetail> {
    return request<AgentDetail>(`/agents/${encodeURIComponent(id)}`);
}

// --- Tokens ---

export function listTokens(): Promise<Token[]> {
    return request<Token[] | null>('/tokens').then(v => v ?? []);
}

export function issueToken(body: TokenIssueRequest): Promise<TokenWithValue> {
    return send<TokenWithValue>('POST', '/tokens', body);
}

export function reissueToken(id: number, validDays?: number): Promise<TokenWithValue> {
    return send<TokenWithValue>('POST', `/tokens/${id}/reissue`, validDays ? { valid_days: validDays } : {});
}

export function unbindToken(id: number): Promise<Token> {
    return send<Token>('POST', `/tokens/${id}/unbind`);
}

export function revokeToken(id: number): Promise<Token> {
    return send<Token>('POST', `/tokens/${id}/revoke`);
}

// --- Monitoring rules ---

export function listRules(): Promise<RuleSummary[]> {
    return request<RuleSummary[] | null>('/rules').then(v => v ?? []);
}

export function getRule(id: number | string): Promise<RuleDetail> {
    return request<RuleDetail>(`/rules/${encodeURIComponent(id)}`);
}

export function getRuleVersion(id: number | string, version: number): Promise<RuleVersion> {
    return request<RuleVersion>(`/rules/${encodeURIComponent(id)}/versions/${version}`);
}

export function getRuleDiff(id: number | string, from: number, to: number): Promise<RuleDiff> {
    return request<RuleDiff>(`/rules/${encodeURIComponent(id)}/diff${query({ from, to })}`);
}

export function mergePreview(templates: string[]): Promise<MergePreview> {
    return send<MergePreview>('POST', '/rules/merge-preview', { templates });
}

export function createRule(body: RuleCreate): Promise<RuleDetail> {
    return send<RuleDetail>('POST', '/rules', body);
}

export function updateRule(
    id: number | string,
    body: { description: string; content: RuleContent; comment: string }
): Promise<RuleDetail> {
    return send<RuleDetail>('PUT', `/rules/${encodeURIComponent(id)}`, body);
}

export function applyRule(id: number | string, version: number, agentIds: number[]): Promise<{ applied: number }> {
    return send('POST', `/rules/${encodeURIComponent(id)}/apply`, { version, agent_ids: agentIds });
}

export function ruleToTemplate(
    id: number | string,
    body: { version: number; name: string; description: string }
): Promise<TemplateDetail> {
    return send<TemplateDetail>('POST', `/rules/${encodeURIComponent(id)}/to-template`, body);
}

export function deleteRule(id: number | string): Promise<unknown> {
    return send('DELETE', `/rules/${encodeURIComponent(id)}`);
}

export function validateRule(content: RuleContent): Promise<string[]> {
    return send<{ errors: string[] | null }>('POST', '/rules/validate', { content }).then(r => r.errors ?? []);
}

// --- Templates ---

export function listTemplates(): Promise<TemplateSummary[]> {
    return request<TemplateSummary[] | null>('/templates').then(v => v ?? []);
}

export function getTemplate(name: string): Promise<TemplateDetail> {
    return request<TemplateDetail>(`/templates/${encodeURIComponent(name)}`);
}

export function createTemplate(body: TemplateCreate): Promise<TemplateDetail> {
    return send<TemplateDetail>('POST', '/templates', body);
}

export function updateTemplate(
    name: string,
    body: { description: string; content: RuleContent }
): Promise<TemplateDetail> {
    return send<TemplateDetail>('PUT', `/templates/${encodeURIComponent(name)}`, body);
}

export function deleteTemplate(name: string): Promise<unknown> {
    return send('DELETE', `/templates/${encodeURIComponent(name)}`);
}

// --- Detections ---

export function listDetections(q: DetectionQuery): Promise<Paged<Detection>> {
    return request<Paged<Detection>>(`/detections${query({ ...q })}`);
}

export function ackDetections(items: { agent_id: number; id: number }[]): Promise<{ acknowledged: number }> {
    return send('POST', '/detections/ack', { items });
}

// --- Suppressions ---

export function listSuppressions(): Promise<Suppression[]> {
    return request<Suppression[] | null>('/suppressions').then(v => v ?? []);
}

export function createSuppression(body: SuppressionCreate): Promise<Suppression> {
    return send<Suppression>('POST', '/suppressions', body);
}

export function deleteSuppression(id: number): Promise<unknown> {
    return send('DELETE', `/suppressions/${id}`);
}

// --- Certificates, history and guide ---

export function getCertificates(): Promise<CertStatus> {
    return request<CertStatus>('/certificates');
}

export function listHistory(q: { category?: string; limit?: number; offset?: number }): Promise<Paged<HistoryItem>> {
    return request<Paged<HistoryItem>>(`/history${query(q)}`);
}

export function getGuide(): Promise<Guide> {
    return request<Guide>('/guide');
}

// --- Accounts ---

export function listAccounts(): Promise<Account[]> {
    return request<Account[] | null>('/accounts').then(v => v ?? []);
}

export function createAccount(body: { name: string; role: Role; password: string }): Promise<Account> {
    return send<Account>('POST', '/accounts', body);
}

export function setAccountRole(id: number, role: Role): Promise<Account> {
    return send<Account>('PUT', `/accounts/${id}/role`, { role });
}

export function resetAccountPassword(id: number, password: string): Promise<Account> {
    return send<Account>('PUT', `/accounts/${id}/password`, { password });
}

export function setAccountDisabled(id: number, disabled: boolean): Promise<Account> {
    return send<Account>('POST', `/accounts/${id}/${disabled ? 'disable' : 'enable'}`);
}

export function deleteAccount(id: number): Promise<unknown> {
    return send('DELETE', `/accounts/${id}`);
}

export function issueCliCredential(id: number): Promise<{ value: string }> {
    return send<{ value: string }>('POST', `/accounts/${id}/cli-credential`);
}
