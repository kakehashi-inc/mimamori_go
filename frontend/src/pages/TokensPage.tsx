import { useEffect, useState, type FormEvent } from 'react';
import { useTranslation } from 'react-i18next';
import { Link } from 'react-router-dom';
import {
    getGuide,
    getRule,
    issueToken,
    listRules,
    listTokens,
    reissueToken,
    revokeToken,
    unbindToken,
} from '../api/client';
import { Badge } from '../components/Badge';
import { Button } from '../components/Button';
import { ConfirmDialog } from '../components/ConfirmDialog';
import { CodeBlock } from '../components/CopyButton';
import { Field, inputClass } from '../components/FormControls';
import { Modal } from '../components/Modal';
import { PageHeader } from '../components/PageHeader';
import { Alert, EmptyState, ErrorAlert, Loading } from '../components/StateViews';
import { TokenStatusBadge } from '../components/StatusBadges';
import { Table, TBody, Td, Th, THead, Tr } from '../components/Table';
import { useAuth } from '../context/AuthContext';
import { useToast } from '../context/ToastContext';
import { useApi } from '../hooks/useApi';
import type { Guide, RuleSummary, RuleVersionInfo, Token, TokenWithValue } from '../types';
import { agentInitCommand } from '../utils/commands';
import { errorMessage, formatDateTime } from '../utils/format';

// Token names accepted by the server.
const TOKEN_NAME_PATTERN = /^[A-Za-z0-9._-]{1,64}$/;

type Confirm = { kind: 'unbind' | 'revoke'; token: Token } | null;

export function TokensPage() {
    const { t } = useTranslation();
    const { isAdmin } = useAuth();
    const { showToast } = useToast();
    const { data, error, loading, reload } = useApi(listTokens, []);
    const [issueOpen, setIssueOpen] = useState(false);
    const [reissueTarget, setReissueTarget] = useState<Token | null>(null);
    const [confirm, setConfirm] = useState<Confirm>(null);
    const [issued, setIssued] = useState<TokenWithValue | null>(null);
    // Whether the dialog shows a reissued token: the agent is then initialized again with --overwrite.
    const [reissued, setReissued] = useState(false);

    function onIssued(r: TokenWithValue, isReissue = false) {
        setIssued(r);
        setReissued(isReissue);
        reload();
    }

    return (
        <>
            <PageHeader
                title={t('tokens.title')}
                description={t('tokens.description')}
                actions={
                    <>
                        <Button icon='refresh' onClick={reload} busy={loading && !!data}>
                            {t('common.reload')}
                        </Button>
                        {isAdmin && (
                            <Button variant='primary' icon='add' onClick={() => setIssueOpen(true)}>
                                {t('tokens.issue')}
                            </Button>
                        )}
                    </>
                }
            />
            <div className='flex flex-col gap-4'>
                <ErrorAlert error={error} onRetry={reload} />
                {loading && !data ? (
                    <Loading />
                ) : data && data.length === 0 ? (
                    <EmptyState icon='key'>{t('tokens.empty')}</EmptyState>
                ) : (
                    data && (
                        <Table>
                            <THead>
                                <Th>{t('tokens.name')}</Th>
                                <Th>{t('tokens.value')}</Th>
                                <Th>{t('tokens.status')}</Th>
                                <Th>{t('tokens.expiresAt')}</Th>
                                <Th>{t('tokens.instanceId')}</Th>
                                <Th>{t('tokens.lastSeen')}</Th>
                                <Th>{t('tokens.created')}</Th>
                                {isAdmin && <Th className='text-right'>{t('common.actions')}</Th>}
                            </THead>
                            <TBody>
                                {data.map(tk => (
                                    <Tr key={tk.id}>
                                        <Td>
                                            <div className='font-medium'>{tk.name}</div>
                                            {tk.agent_id > 0 && (
                                                <Link
                                                    to={`/agents/${tk.agent_id}`}
                                                    className='text-xs text-accent hover:text-accent-hover hover:underline'
                                                >
                                                    {t('tokens.viewAgent')}
                                                </Link>
                                            )}
                                        </Td>
                                        <Td className='font-mono text-xs whitespace-nowrap'>{tk.value || '-'}</Td>
                                        <Td>
                                            <div className='flex flex-col items-start gap-1'>
                                                <TokenStatusBadge status={tk.status} />
                                                {tk.expiring_unconnected && (
                                                    <Badge
                                                        tone='warning'
                                                        icon='warning'
                                                        title={t('tokens.expiringUnconnectedTitle')}
                                                    >
                                                        {t('tokens.expiringUnconnected')}
                                                    </Badge>
                                                )}
                                                {tk.pending_renewal && (
                                                    <Badge
                                                        tone='accent'
                                                        icon='autorenew'
                                                        title={t('tokens.pendingRenewalTitle')}
                                                    >
                                                        {t('tokens.pendingRenewal')}
                                                    </Badge>
                                                )}
                                            </div>
                                        </Td>
                                        <Td className='whitespace-nowrap'>
                                            {formatDateTime(tk.expires_at)}
                                            {tk.renewed_at && (
                                                <div className='text-xs text-muted'>
                                                    {t('tokens.renewedAt', { date: formatDateTime(tk.renewed_at) })}
                                                </div>
                                            )}
                                            {tk.reissued_at && (
                                                <div className='text-xs text-muted'>
                                                    {t('tokens.reissuedAt', { date: formatDateTime(tk.reissued_at) })}
                                                </div>
                                            )}
                                        </Td>
                                        <Td>
                                            {tk.instance_id ? (
                                                <>
                                                    <div
                                                        className='max-w-48 truncate font-mono text-xs'
                                                        title={tk.instance_id}
                                                    >
                                                        {tk.instance_id}
                                                    </div>
                                                    <div className='text-xs whitespace-nowrap text-muted'>
                                                        {t('tokens.boundAt', { date: formatDateTime(tk.bound_at) })}
                                                    </div>
                                                </>
                                            ) : (
                                                <span className='text-muted'>{t('tokens.unbound')}</span>
                                            )}
                                        </Td>
                                        <Td className='whitespace-nowrap'>{formatDateTime(tk.last_seen_at)}</Td>
                                        <Td className='whitespace-nowrap'>
                                            {formatDateTime(tk.created_at)}
                                            <div className='text-xs text-muted'>{tk.created_by}</div>
                                        </Td>
                                        {isAdmin && (
                                            <Td className='text-right'>
                                                <div className='flex justify-end gap-1'>
                                                    <Button
                                                        size='sm'
                                                        icon='autorenew'
                                                        onClick={() => setReissueTarget(tk)}
                                                    >
                                                        {t('tokens.reissue')}
                                                    </Button>
                                                    <Button
                                                        size='sm'
                                                        icon='link_off'
                                                        disabled={!tk.instance_id}
                                                        onClick={() => setConfirm({ kind: 'unbind', token: tk })}
                                                    >
                                                        {t('tokens.unbind')}
                                                    </Button>
                                                    <Button
                                                        size='sm'
                                                        variant='danger'
                                                        icon='block'
                                                        disabled={tk.status === 'revoked'}
                                                        onClick={() => setConfirm({ kind: 'revoke', token: tk })}
                                                    >
                                                        {t('tokens.revoke')}
                                                    </Button>
                                                </div>
                                            </Td>
                                        )}
                                    </Tr>
                                ))}
                            </TBody>
                        </Table>
                    )
                )}
            </div>

            {isAdmin && (
                <>
                    <IssueTokenModal open={issueOpen} onClose={() => setIssueOpen(false)} onIssued={onIssued} />
                    <ReissueTokenModal
                        token={reissueTarget}
                        onClose={() => setReissueTarget(null)}
                        onIssued={r => onIssued(r, true)}
                    />
                    <ConfirmDialog
                        open={confirm !== null}
                        title={confirm?.kind === 'revoke' ? t('tokens.revokeTitle') : t('tokens.unbindTitle')}
                        message={
                            confirm?.kind === 'revoke'
                                ? t('tokens.revokeConfirm', { name: confirm.token.name })
                                : t('tokens.unbindConfirm', { name: confirm?.token.name ?? '' })
                        }
                        confirmLabel={confirm?.kind === 'revoke' ? t('tokens.revoke') : t('tokens.unbind')}
                        danger={confirm?.kind === 'revoke'}
                        onClose={() => setConfirm(null)}
                        onConfirm={async () => {
                            if (!confirm) return;
                            if (confirm.kind === 'revoke') await revokeToken(confirm.token.id);
                            else await unbindToken(confirm.token.id);
                            showToast(t('common.done'));
                            reload();
                        }}
                    />
                    <Modal
                        open={issued !== null}
                        title={t(reissued ? 'tokens.reissuedTitle' : 'tokens.issuedTitle', {
                            name: issued?.token.name ?? '',
                        })}
                        onClose={() => setIssued(null)}
                        size='lg'
                        footer={
                            <Button variant='primary' onClick={() => setIssued(null)}>
                                {t('common.close')}
                            </Button>
                        }
                    >
                        {issued && <IssuedTokenHelp issued={issued} reissued={reissued} />}
                    </Modal>
                </>
            )}
        </>
    );
}

/**
 * The new value (in full until the dialog is closed; the browser shows only a
 * shortened value afterwards) and the next steps.
 */
function IssuedTokenHelp({ issued, reissued }: { issued: TokenWithValue; reissued: boolean }) {
    const { t } = useTranslation();
    const [guide, setGuide] = useState<Guide | null>(null);
    useEffect(() => {
        getGuide()
            .then(setGuide)
            .catch(() => setGuide(null));
    }, []);
    // Only names in the server certificate work for --central-host; without any, show a placeholder.
    const hosts = guide?.central_hosts?.length ? guide.central_hosts : ['<host>'];
    const port = guide?.central_port || '<port>';
    return (
        <div className='flex flex-col gap-4'>
            <Alert tone='warning'>{t('tokens.fullValueNote')}</Alert>
            {guide && !guide.central_hosts?.length && <Alert tone='warning'>{t('guide.noCentralHosts')}</Alert>}
            <div className='flex flex-col gap-2'>
                <p className='text-sm font-medium'>{t('tokens.value')}</p>
                <CodeBlock code={issued.value} />
            </div>
            <div className='flex flex-col gap-2'>
                <p className='text-sm font-medium'>{t('tokens.initCommand')}</p>
                <p className='text-xs text-muted'>{t('tokens.initCommandHint')}</p>
                {hosts.map(host => (
                    <CodeBlock key={host} code={agentInitCommand(host, port, undefined, reissued)} />
                ))}
                <p className='text-xs text-muted'>
                    {t('tokens.initThenStart')} <code className='font-mono'>./mimamori agent start</code>
                </p>
                <p className='text-xs'>
                    <Link to='/guide' className='text-accent hover:text-accent-hover hover:underline'>
                        {t('tokens.seeGuide')}
                    </Link>
                </p>
            </div>
        </div>
    );
}

function parseValidDays(text: string): number | undefined | null {
    if (text.trim() === '') return undefined;
    const n = Number(text);
    return Number.isInteger(n) && n > 0 ? n : null;
}

function IssueTokenModal({
    open,
    onClose,
    onIssued,
}: {
    open: boolean;
    onClose: () => void;
    onIssued: (r: TokenWithValue) => void;
}) {
    const { t } = useTranslation();
    const [name, setName] = useState('');
    const [validDays, setValidDays] = useState('');
    const [rules, setRules] = useState<RuleSummary[]>([]);
    const [ruleId, setRuleId] = useState('');
    const [versions, setVersions] = useState<RuleVersionInfo[]>([]);
    const [version, setVersion] = useState('');
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState<string | null>(null);

    useEffect(() => {
        if (!open) return;
        setName('');
        setValidDays('');
        setRuleId('');
        setVersion('');
        setError(null);
        listRules()
            .then(setRules)
            .catch(() => setRules([]));
    }, [open]);

    useEffect(() => {
        setVersions([]);
        setVersion('');
        if (!ruleId) return;
        let active = true;
        getRule(ruleId)
            .then(r => {
                if (!active) return;
                const list = [...(r.versions ?? [])].sort((a, b) => b.version - a.version);
                setVersions(list);
                setVersion(String(r.latest_version));
            })
            .catch(() => {
                if (active) setVersions([]);
            });
        return () => {
            active = false;
        };
    }, [ruleId]);

    async function submit(e: FormEvent) {
        e.preventDefault();
        const days = parseValidDays(validDays);
        if (!TOKEN_NAME_PATTERN.test(name)) {
            setError(t('tokens.nameInvalid'));
            return;
        }
        if (days === null) {
            setError(t('tokens.validDaysInvalid'));
            return;
        }
        setBusy(true);
        setError(null);
        try {
            const r = await issueToken({
                name,
                valid_days: days,
                rule_id: ruleId ? Number(ruleId) : undefined,
                rule_version: ruleId && version ? Number(version) : undefined,
            });
            onClose();
            onIssued(r);
        } catch (err) {
            setError(errorMessage(t, err));
        } finally {
            setBusy(false);
        }
    }

    return (
        <Modal
            open={open}
            title={t('tokens.issueTitle')}
            onClose={onClose}
            locked={busy}
            footer={
                <>
                    <Button onClick={onClose} disabled={busy}>
                        {t('common.cancel')}
                    </Button>
                    <Button variant='primary' type='submit' form='issue-token-form' busy={busy} icon='key'>
                        {t('tokens.issue')}
                    </Button>
                </>
            }
        >
            <form id='issue-token-form' onSubmit={e => void submit(e)} className='flex flex-col gap-4'>
                <Field label={t('tokens.name')} htmlFor='tk-name' hint={t('tokens.nameHint')} required>
                    <input
                        id='tk-name'
                        className={inputClass}
                        value={name}
                        required
                        maxLength={64}
                        onChange={e => setName(e.target.value)}
                    />
                </Field>
                <Field label={t('tokens.validDays')} htmlFor='tk-days' hint={t('tokens.validDaysHint')}>
                    <input
                        id='tk-days'
                        type='number'
                        min={1}
                        className={inputClass}
                        value={validDays}
                        onChange={e => setValidDays(e.target.value)}
                    />
                </Field>
                <Field label={t('tokens.rule')} htmlFor='tk-rule' hint={t('tokens.ruleHint')}>
                    <select
                        id='tk-rule'
                        className={inputClass}
                        value={ruleId}
                        onChange={e => setRuleId(e.target.value)}
                    >
                        <option value=''>{t('tokens.noRule')}</option>
                        {rules.map(r => (
                            <option key={r.id} value={r.id}>
                                {r.name}
                            </option>
                        ))}
                    </select>
                </Field>
                {ruleId && (
                    <Field label={t('rules.version')} htmlFor='tk-version'>
                        <select
                            id='tk-version'
                            className={inputClass}
                            value={version}
                            onChange={e => setVersion(e.target.value)}
                        >
                            {versions.map(v => (
                                <option key={v.version} value={v.version}>
                                    v{v.version}
                                    {v.comment ? ` - ${v.comment}` : ''}
                                </option>
                            ))}
                        </select>
                    </Field>
                )}
                {error && <Alert tone='danger'>{error}</Alert>}
            </form>
        </Modal>
    );
}

function ReissueTokenModal({
    token,
    onClose,
    onIssued,
}: {
    token: Token | null;
    onClose: () => void;
    onIssued: (r: TokenWithValue) => void;
}) {
    const { t } = useTranslation();
    const [validDays, setValidDays] = useState('');
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState<string | null>(null);

    useEffect(() => {
        if (token) {
            setValidDays('');
            setError(null);
        }
    }, [token]);

    async function submit(e: FormEvent) {
        e.preventDefault();
        if (!token) return;
        const days = parseValidDays(validDays);
        if (days === null) {
            setError(t('tokens.validDaysInvalid'));
            return;
        }
        setBusy(true);
        setError(null);
        try {
            const r = await reissueToken(token.id, days);
            onClose();
            onIssued(r);
        } catch (err) {
            setError(errorMessage(t, err));
        } finally {
            setBusy(false);
        }
    }

    return (
        <Modal
            open={token !== null}
            title={t('tokens.reissueTitle', { name: token?.name ?? '' })}
            onClose={onClose}
            locked={busy}
            footer={
                <>
                    <Button onClick={onClose} disabled={busy}>
                        {t('common.cancel')}
                    </Button>
                    <Button variant='primary' type='submit' form='reissue-token-form' busy={busy} icon='autorenew'>
                        {t('tokens.reissue')}
                    </Button>
                </>
            }
        >
            <form id='reissue-token-form' onSubmit={e => void submit(e)} className='flex flex-col gap-4'>
                <Alert tone='warning'>{t('tokens.reissueNote')}</Alert>
                <Field label={t('tokens.validDays')} htmlFor='rtk-days' hint={t('tokens.validDaysHint')}>
                    <input
                        id='rtk-days'
                        type='number'
                        min={1}
                        className={inputClass}
                        value={validDays}
                        onChange={e => setValidDays(e.target.value)}
                    />
                </Field>
                {error && <Alert tone='danger'>{error}</Alert>}
            </form>
        </Modal>
    );
}
