import type { LangSetting } from '../constants';
import type {
    AgentCommandResults,
    AgentDetail,
    AgentSummary,
    CertStatus,
    CommandResultsByName,
    CommandRunDetail,
    CommandRunList,
    Detection,
    DetectionQuery,
    EmailChangeStart,
    Guide,
    Health,
    HistoryItem,
    LoginChallenge,
    Me,
    Overview,
    Paged,
    RuleDetail,
    RuleDiff,
    RuleSummary,
    RuleVersion,
    ServerMsg,
    ServerProblem,
    Suppression,
    TemplateDetail,
    TemplateSummary,
    Token,
} from '../types';

const BASE = '/api/v1';

/**
 * Raised for any non-2xx response, or with status 0 when the network failed. message is the English
 * text of the answer (field error); i18n its message (error_i18n) and problems the coded problems of
 * the answer (errors_i18n), when the server sent them.
 */
export class ApiError extends Error {
    status: number;
    i18n?: ServerMsg;
    problems?: ServerProblem[];
    constructor(status: number, message: string, i18n?: ServerMsg, problems?: ServerProblem[]) {
        super(message);
        this.name = 'ApiError';
        this.status = status;
        this.i18n = i18n;
        this.problems = problems;
    }
}

function isServerMsg(v: unknown): v is ServerMsg {
    return typeof v === 'object' && v !== null && typeof (v as { code?: unknown }).code === 'string';
}

// Called when any request other than the login steps answers 401, so the app
// can drop the session and show the login screen. On the login steps a 401
// means a wrong password or code, not a lost session.
const LOGIN_PATHS = new Set(['/auth/login', '/auth/login/verify']);
let unauthorizedHandler: (() => void) | null = null;

export function setUnauthorizedHandler(handler: (() => void) | null): void {
    unauthorizedHandler = handler;
}

export async function request<T>(path: string, init?: RequestInit): Promise<T> {
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
        let i18n: ServerMsg | undefined;
        let problems: ServerProblem[] | undefined;
        try {
            const body = await res.json();
            if (body && typeof body.error === 'string') {
                message = body.error;
                if (isServerMsg(body.error_i18n)) i18n = body.error_i18n;
            }
            if (body && Array.isArray(body.errors_i18n)) problems = body.errors_i18n as ServerProblem[];
        } catch {
            // The body is not JSON; keep the status text.
        }
        if (res.status === 401 && !LOGIN_PATHS.has(path) && unauthorizedHandler) {
            unauthorizedHandler();
        }
        throw new ApiError(res.status, message, i18n, problems);
    }
    if (res.status === 204) {
        return undefined as T;
    }
    return (await res.json()) as T;
}

export function send<T>(method: string, path: string, body?: unknown): Promise<T> {
    return request<T>(path, { method, body: body === undefined ? undefined : JSON.stringify(body) });
}

/** Builds "?a=1&b=2" from the defined, non-empty values. */
export function query(params: Record<string, string | number | undefined>): string {
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

/** Result of the password step: logged in, or a code was mailed. */
export type LoginResult = { kind: 'done'; me: Me } | { kind: 'code'; challenge: string };

export async function login(email: string, password: string, remember: boolean): Promise<LoginResult> {
    const r = await send<Me | LoginChallenge>('POST', '/auth/login', { email, password, remember });
    if ('two_step' in r && r.two_step) return { kind: 'code', challenge: r.challenge };
    return { kind: 'done', me: r as Me };
}

/** Second login step: the mailed code; trust skips the code on this browser for the configured days. */
export function loginVerify(challenge: string, code: string, trust: boolean): Promise<Me> {
    return send<Me>('POST', '/auth/login/verify', { challenge, code, trust });
}

export function logout(): Promise<unknown> {
    return send('POST', '/auth/logout');
}

export function getMe(): Promise<Me> {
    return request<Me>('/auth/me');
}

export function setOwnLanguage(language: LangSetting): Promise<Me> {
    return send<Me>('PUT', '/auth/language', { language });
}

export function setOwnDisplayName(displayName: string): Promise<Me> {
    return send<Me>('PUT', '/auth/display-name', { display_name: displayName });
}

/** Starts the change of the own e-mail address (a code is mailed to the new address unless applied). */
export function requestOwnEmailChange(email: string): Promise<EmailChangeStart> {
    return send<EmailChangeStart>('POST', '/auth/email', { email });
}

/** Confirms the own e-mail address change with the mailed code. */
export function verifyOwnEmailChange(code: string): Promise<Me> {
    return send<Me>('POST', '/auth/email/verify', { code });
}

export function changeOwnPassword(currentPassword: string, newPassword: string): Promise<unknown> {
    return send('POST', '/auth/password', { current_password: currentPassword, new_password: newPassword });
}

// --- Overview and agents ---

/**
 * Overview of the central. `background` marks a request the page sends by
 * itself (the periodic refresh): it does not count as an operation that keeps
 * the login session alive.
 */
export function getOverview(background = false): Promise<Overview> {
    return request<Overview>('/overview', background ? { headers: { 'X-Mimamori-Background': '1' } } : undefined);
}

export function listAgents(): Promise<AgentSummary[]> {
    return request<AgentSummary[] | null>('/agents').then(v => v ?? []);
}

export function getAgent(id: number | string): Promise<AgentDetail> {
    return request<AgentDetail>(`/agents/${encodeURIComponent(id)}`);
}

// --- Command results ---

/** Latest results of the command sets of one agent. */
export function getAgentCommandResults(agentId: number | string): Promise<AgentCommandResults> {
    return request<AgentCommandResults>(`/agents/${encodeURIComponent(agentId)}/command-results`);
}

/** Latest results of the sets with this name on every agent ('' lists only the names). */
export function getCommandResultsByName(name: string): Promise<CommandResultsByName> {
    return request<CommandResultsByName>(`/command-results${query({ name })}`);
}

/** Runs of one set, newest first; `before` continues after the run with that ID. */
export function listCommandRuns(
    agentId: number | string,
    setId: number | string,
    before?: number
): Promise<CommandRunList> {
    return request<CommandRunList>(
        `/agents/${encodeURIComponent(agentId)}/command-sets/${encodeURIComponent(setId)}/runs${query({ before })}`
    );
}

export function getCommandRun(agentId: number | string, runId: number | string): Promise<CommandRunDetail> {
    return request<CommandRunDetail>(
        `/agents/${encodeURIComponent(agentId)}/command-runs/${encodeURIComponent(runId)}`
    );
}

// --- Tokens ---

export function listTokens(): Promise<Token[]> {
    return request<Token[] | null>('/tokens').then(v => v ?? []);
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

// --- Templates ---

export function listTemplates(): Promise<TemplateSummary[]> {
    return request<TemplateSummary[] | null>('/templates').then(v => v ?? []);
}

export function getTemplate(name: string): Promise<TemplateDetail> {
    return request<TemplateDetail>(`/templates/${encodeURIComponent(name)}`);
}

// --- Detections ---

export function listDetections(q: DetectionQuery): Promise<Paged<Detection>> {
    return request<Paged<Detection>>(`/detections${query({ ...q })}`);
}

export function getDetection(agentId: number, id: number): Promise<Detection> {
    return request<Detection>(`/detections/${agentId}/${id}`);
}

// --- Suppressions ---

export function listSuppressions(): Promise<Suppression[]> {
    return request<Suppression[] | null>('/suppressions').then(v => v ?? []);
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

// --- Extension modules ---

/** URLs of the extension modules for the signed-in account (absolute paths). */
export function listUiModules(): Promise<string[]> {
    return request<string[] | null>('/ui/modules').then(v => v ?? []);
}
