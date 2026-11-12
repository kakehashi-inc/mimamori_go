// API functions used only by the administrator extension.
import type { LangSetting } from '../constants';
import { query, request, send } from '../api/client';
import type {
    Account,
    CentralConfig,
    CommandRequest,
    CommandSet,
    CommandSetList,
    CommandSetSpec,
    ConfigUpdate,
    MergePreview,
    Role,
    RuleContent,
    RuleCreate,
    RuleDetail,
    Suppression,
    SuppressionCreate,
    TemplateCreate,
    TemplateDetail,
    Token,
    TokenIssueRequest,
    TokenWithValue,
} from '../types';

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

export function ackDetections(items: { agent_id: number; id: number }[]): Promise<{ acknowledged: number }> {
    return send('POST', '/detections/ack', { items });
}

export function createSuppression(body: SuppressionCreate): Promise<Suppression> {
    return send<Suppression>('POST', '/suppressions', body);
}

export function deleteSuppression(id: number): Promise<unknown> {
    return send('DELETE', `/suppressions/${id}`);
}

export function listAccounts(): Promise<Account[]> {
    return request<Account[] | null>('/accounts').then(v => v ?? []);
}

export function createAccount(body: {
    email: string;
    display_name: string;
    role: Role;
    language: LangSetting;
    password: string;
}): Promise<Account> {
    return send<Account>('POST', '/accounts', body);
}

export function setAccountEmail(id: number, email: string): Promise<Account> {
    return send<Account>('PUT', `/accounts/${id}/email`, { email });
}

export function setAccountDisplayName(id: number, displayName: string): Promise<Account> {
    return send<Account>('PUT', `/accounts/${id}/display-name`, { display_name: displayName });
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

export function getConfig(): Promise<CentralConfig> {
    return request<CentralConfig>('/config');
}

export function updateConfig(body: ConfigUpdate): Promise<CentralConfig> {
    return send<CentralConfig>('PUT', '/config', body);
}

export function setAccountNotify(id: number, notify: boolean): Promise<Account> {
    return send<Account>('PUT', `/accounts/${id}/notify`, { notify });
}

// --- Command sets ---

function setsPath(agentId: number | string): string {
    return `/agents/${encodeURIComponent(agentId)}/command-sets`;
}

export function listCommandSets(agentId: number | string): Promise<CommandSetList> {
    return request<CommandSetList>(setsPath(agentId));
}

export function createCommandSet(agentId: number | string, spec: CommandSetSpec): Promise<CommandSet> {
    return send<CommandSet>('POST', setsPath(agentId), spec);
}

export function updateCommandSet(agentId: number | string, setId: number, spec: CommandSetSpec): Promise<CommandSet> {
    return send<CommandSet>('PUT', `${setsPath(agentId)}/${setId}`, spec);
}

export function deleteCommandSet(agentId: number | string, setId: number): Promise<unknown> {
    return send('DELETE', `${setsPath(agentId)}/${setId}`);
}

/** Checks a definition without saving it; setId excludes that set from the name check. */
export function validateCommandSet(agentId: number | string, spec: CommandSetSpec, setId?: number): Promise<string[]> {
    return send<{ errors: string[] | null }>(
        'POST',
        `${setsPath(agentId)}/validate${query({ set_id: setId })}`,
        spec
    ).then(r => r.errors ?? []);
}

export function importCommandSets(
    agentId: number | string,
    sourceAgentId: number,
    items: { set_id: number; name: string }[]
): Promise<{ sets: CommandSet[] }> {
    return send('POST', `${setsPath(agentId)}/import`, { source_agent_id: sourceAgentId, items });
}

export function requestCommandRun(agentId: number | string, setId: number): Promise<CommandRequest> {
    return send<CommandRequest>('POST', `${setsPath(agentId)}/${setId}/run`);
}

export function discardCommandRequest(agentId: number | string, requestId: number): Promise<CommandRequest> {
    return send<CommandRequest>('DELETE', `/agents/${encodeURIComponent(agentId)}/command-requests/${requestId}`);
}

// --- Re-authentication for changes ---

/** Mails a code to the own address; required is false when no code is needed (development mode). */
export function requestStepUp(): Promise<{ required: boolean }> {
    return send<{ required: boolean }>('POST', '/auth/step-up');
}

export function verifyStepUp(code: string): Promise<{ required: boolean }> {
    return send<{ required: boolean }>('POST', '/auth/step-up/verify', { code });
}
