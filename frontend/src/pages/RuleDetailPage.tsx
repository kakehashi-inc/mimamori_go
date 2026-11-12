import { useEffect, useState, type FormEvent } from 'react';
import { useTranslation } from 'react-i18next';
import { Link, useNavigate, useParams } from 'react-router-dom';
import {
    ApiError,
    applyRule,
    deleteRule,
    getRule,
    getRuleDiff,
    getRuleVersion,
    listAgents,
    ruleToTemplate,
} from '../api/client';
import { AppliedAgentsList } from '../components/AppliedAgentsList';
import { Button, buttonClass } from '../components/Button';
import { ConfirmDialog } from '../components/ConfirmDialog';
import { DiffView } from '../components/DiffView';
import { checkboxClass, Field, inputClass, inputSmAutoClass } from '../components/FormControls';
import { Modal } from '../components/Modal';
import { Card, PageHeader } from '../components/PageHeader';
import { RuleContentView } from '../components/RuleContentView';
import { Alert, EmptyState, ErrorAlert, Loading } from '../components/StateViews';
import { Table, TBody, Td, Th, THead, Tr } from '../components/Table';
import { Tabs } from '../components/Tabs';
import { useAuth } from '../context/AuthContext';
import { useToast } from '../context/ToastContext';
import { useApi } from '../hooks/useApi';
import type { AgentSummary, RuleDetail, RuleVersionInfo } from '../types';
import { errorMessage, formatDateTime } from '../utils/format';

type Tab = 'content' | 'versions' | 'diff';

function sortedVersions(rule: RuleDetail): RuleVersionInfo[] {
    return [...(rule.versions ?? [])].sort((a, b) => b.version - a.version);
}

export function RuleDetailPage() {
    const { t } = useTranslation();
    const { id = '' } = useParams();
    const navigate = useNavigate();
    const { isAdmin } = useAuth();
    const { showToast } = useToast();
    const { data: rule, error, loading, reload } = useApi(() => getRule(id), [id]);
    const [tab, setTab] = useState<Tab>('content');
    const [viewVersion, setViewVersion] = useState(0);
    const [diffFrom, setDiffFrom] = useState(0);
    const [diffTo, setDiffTo] = useState(0);
    const [applyOpen, setApplyOpen] = useState(false);
    const [templateOpen, setTemplateOpen] = useState(false);
    const [deleteOpen, setDeleteOpen] = useState(false);

    useEffect(() => {
        if (!rule) return;
        setViewVersion(rule.latest_version);
        setDiffTo(rule.latest_version);
        setDiffFrom(Math.max(1, rule.latest_version - 1));
    }, [rule]);

    const linkClass = 'text-accent hover:text-accent-hover hover:underline';

    return (
        <>
            <PageHeader
                breadcrumb={
                    <Link to='/rules' className={linkClass}>
                        {t('rules.title')}
                    </Link>
                }
                title={rule?.name ?? t('rules.detailTitle')}
                description={rule?.description}
                actions={
                    rule &&
                    isAdmin && (
                        <>
                            <Link to={`/rules/${rule.id}/edit`} className={buttonClass('secondary')}>
                                <span className='material-icons-outlined text-base' aria-hidden='true'>
                                    edit
                                </span>
                                {t('common.edit')}
                            </Link>
                            <Button icon='send' variant='primary' onClick={() => setApplyOpen(true)}>
                                {t('rules.apply')}
                            </Button>
                            <Button icon='library_add' onClick={() => setTemplateOpen(true)}>
                                {t('rules.toTemplate')}
                            </Button>
                            <Button icon='delete' variant='danger' onClick={() => setDeleteOpen(true)}>
                                {t('common.delete')}
                            </Button>
                        </>
                    )
                }
            />
            <ErrorAlert error={error} onRetry={reload} />
            {loading && !rule ? (
                <Loading />
            ) : (
                rule && (
                    <div className='flex flex-col gap-4'>
                        <Card title={t('rules.appliedAgents')}>
                            <AppliedAgentsList agents={rule.applied_agents ?? []} latest={rule.latest_version} />
                        </Card>
                        <Tabs
                            value={tab}
                            onChange={setTab}
                            items={[
                                { value: 'content', label: t('rules.tabContent'), icon: 'article' },
                                { value: 'versions', label: t('rules.tabVersions'), icon: 'history' },
                                { value: 'diff', label: t('rules.tabDiff'), icon: 'difference' },
                            ]}
                        />
                        {tab === 'content' && (
                            <VersionContent rule={rule} version={viewVersion} onVersionChange={setViewVersion} />
                        )}
                        {tab === 'versions' && (
                            <VersionsTable
                                rule={rule}
                                onView={v => {
                                    setViewVersion(v);
                                    setTab('content');
                                }}
                                onDiff={v => {
                                    setDiffFrom(Math.max(1, v - 1));
                                    setDiffTo(v);
                                    setTab('diff');
                                }}
                            />
                        )}
                        {tab === 'diff' && (
                            <RuleDiffPanel
                                rule={rule}
                                from={diffFrom}
                                to={diffTo}
                                onFrom={setDiffFrom}
                                onTo={setDiffTo}
                            />
                        )}
                    </div>
                )
            )}
            {rule && isAdmin && (
                <>
                    <ApplyRuleModal
                        open={applyOpen}
                        rule={rule}
                        onClose={() => setApplyOpen(false)}
                        onApplied={n => {
                            showToast(t('rules.applied', { count: n }));
                            reload();
                        }}
                    />
                    <ToTemplateModal
                        open={templateOpen}
                        rule={rule}
                        onClose={() => setTemplateOpen(false)}
                        onCreated={name => {
                            showToast(t('templates.created'));
                            navigate(`/templates/${encodeURIComponent(name)}`);
                        }}
                    />
                    <ConfirmDialog
                        open={deleteOpen}
                        title={t('rules.deleteTitle')}
                        message={t('rules.deleteConfirm', { name: rule.name })}
                        confirmLabel={t('common.delete')}
                        danger
                        onClose={() => setDeleteOpen(false)}
                        mapError={e => (e instanceof ApiError && e.status === 409 ? t('rules.deleteInUse') : null)}
                        onConfirm={async () => {
                            await deleteRule(rule.id);
                            showToast(t('common.deleted'));
                            navigate('/rules', { replace: true });
                        }}
                    />
                </>
            )}
        </>
    );
}

function VersionSelect({
    id,
    versions,
    value,
    onChange,
}: {
    id: string;
    versions: RuleVersionInfo[];
    value: number;
    onChange: (v: number) => void;
}) {
    return (
        <select
            id={id}
            className={`${inputSmAutoClass} shrink-0`}
            value={value}
            onChange={e => onChange(Number(e.target.value))}
        >
            {versions.map(v => (
                <option key={v.version} value={v.version}>
                    v{v.version} ({formatDateTime(v.created_at)})
                </option>
            ))}
        </select>
    );
}

function VersionContent({
    rule,
    version,
    onVersionChange,
}: {
    rule: RuleDetail;
    version: number;
    onVersionChange: (v: number) => void;
}) {
    const { t } = useTranslation();
    const isLatest = version === rule.latest_version || version === 0;
    const { data, error, loading } = useApi(
        () => (isLatest ? Promise.resolve(null) : getRuleVersion(rule.id, version)),
        [rule.id, version, isLatest]
    );
    const versions = sortedVersions(rule);
    const info = versions.find(v => v.version === (version || rule.latest_version));
    const content = isLatest ? rule.content : data?.content;

    return (
        <div className='flex flex-col gap-4'>
            <div className='flex flex-wrap items-center gap-3 rounded-lg border border-line bg-surface px-4 py-3 text-sm'>
                <label htmlFor='rv-version' className='text-muted'>
                    {t('rules.version')}
                </label>
                <VersionSelect id='rv-version' versions={versions} value={version} onChange={onVersionChange} />
                {isLatest && <span className='text-xs text-success'>{t('rules.latest')}</span>}
                {info && (
                    <span className='min-w-0 text-xs wrap-anywhere text-muted'>
                        {info.created_by} / {info.comment || t('rules.noComment')}
                    </span>
                )}
            </div>
            <ErrorAlert error={error} />
            {!isLatest && loading ? <Loading /> : content && <RuleContentView content={content} />}
        </div>
    );
}

function VersionsTable({
    rule,
    onView,
    onDiff,
}: {
    rule: RuleDetail;
    onView: (v: number) => void;
    onDiff: (v: number) => void;
}) {
    const { t } = useTranslation();
    const versions = sortedVersions(rule);
    if (versions.length === 0) return <EmptyState />;
    return (
        <Table>
            <THead>
                <Th>{t('rules.version')}</Th>
                <Th>{t('rules.createdAt')}</Th>
                <Th>{t('rules.createdBy')}</Th>
                <Th>{t('rules.comment')}</Th>
                <Th className='text-right'>{t('common.actions')}</Th>
            </THead>
            <TBody>
                {versions.map(v => (
                    <Tr key={v.version}>
                        <Td className='font-medium'>
                            v{v.version}
                            {v.version === rule.latest_version && (
                                <span className='ml-2 text-xs text-success'>{t('rules.latest')}</span>
                            )}
                        </Td>
                        <Td className='whitespace-nowrap'>{formatDateTime(v.created_at)}</Td>
                        <Td>{v.created_by}</Td>
                        <Td className='text-muted'>{v.comment || '-'}</Td>
                        <Td className='text-right'>
                            <div className='flex justify-end gap-1'>
                                <Button size='sm' icon='visibility' onClick={() => onView(v.version)}>
                                    {t('rules.view')}
                                </Button>
                                <Button
                                    size='sm'
                                    icon='difference'
                                    disabled={v.version <= 1}
                                    onClick={() => onDiff(v.version)}
                                >
                                    {t('rules.diffPrev')}
                                </Button>
                            </div>
                        </Td>
                    </Tr>
                ))}
            </TBody>
        </Table>
    );
}

function RuleDiffPanel({
    rule,
    from,
    to,
    onFrom,
    onTo,
}: {
    rule: RuleDetail;
    from: number;
    to: number;
    onFrom: (v: number) => void;
    onTo: (v: number) => void;
}) {
    const { t } = useTranslation();
    const versions = sortedVersions(rule);
    const ready = from > 0 && to > 0;
    const { data, error, loading } = useApi(
        () => (ready ? getRuleDiff(rule.id, from, to) : Promise.resolve(null)),
        [rule.id, from, to, ready]
    );
    return (
        <div className='flex flex-col gap-4'>
            <div className='flex flex-wrap items-center gap-3 rounded-lg border border-line bg-surface px-4 py-3 text-sm'>
                <label htmlFor='diff-from' className='text-muted'>
                    {t('rules.diffFrom')}
                </label>
                <VersionSelect id='diff-from' versions={versions} value={from} onChange={onFrom} />
                <span className='material-icons-outlined text-muted' aria-hidden='true'>
                    arrow_forward
                </span>
                <label htmlFor='diff-to' className='text-muted'>
                    {t('rules.diffTo')}
                </label>
                <VersionSelect id='diff-to' versions={versions} value={to} onChange={onTo} />
            </div>
            <ErrorAlert error={error} />
            {loading ? <Loading /> : data && <DiffView lines={data.lines ?? []} />}
        </div>
    );
}

function ApplyRuleModal({
    open,
    rule,
    onClose,
    onApplied,
}: {
    open: boolean;
    rule: RuleDetail;
    onClose: () => void;
    onApplied: (n: number) => void;
}) {
    const { t } = useTranslation();
    const [version, setVersion] = useState(rule.latest_version);
    const [agents, setAgents] = useState<AgentSummary[] | null>(null);
    const [selected, setSelected] = useState<Set<number>>(new Set());
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState<string | null>(null);

    useEffect(() => {
        if (!open) return;
        setVersion(rule.latest_version);
        setError(null);
        setAgents(null);
        // Agents already using this rule are preselected so they can be upgraded in one step.
        setSelected(new Set((rule.applied_agents ?? []).map(a => a.agent_id)));
        listAgents()
            .then(setAgents)
            .catch(e => {
                setAgents([]);
                setError(errorMessage(t, e));
            });
    }, [open, rule, t]);

    function toggle(id: number) {
        setSelected(prev => {
            const next = new Set(prev);
            if (next.has(id)) next.delete(id);
            else next.add(id);
            return next;
        });
    }

    async function submit(e: FormEvent) {
        e.preventDefault();
        if (selected.size === 0) {
            setError(t('rules.applyNoAgents'));
            return;
        }
        setBusy(true);
        setError(null);
        try {
            const r = await applyRule(rule.id, version, [...selected]);
            onClose();
            onApplied(r.applied);
        } catch (err) {
            setError(errorMessage(t, err));
        } finally {
            setBusy(false);
        }
    }

    return (
        <Modal
            open={open}
            title={t('rules.applyTitle', { name: rule.name })}
            onClose={onClose}
            size='lg'
            locked={busy}
            footer={
                <>
                    <Button onClick={onClose} disabled={busy}>
                        {t('common.cancel')}
                    </Button>
                    <Button variant='primary' type='submit' form='apply-rule-form' busy={busy} icon='send'>
                        {t('rules.apply')}
                    </Button>
                </>
            }
        >
            <form id='apply-rule-form' onSubmit={e => void submit(e)} className='flex flex-col gap-4'>
                <Field label={t('rules.version')} htmlFor='apply-version'>
                    <VersionSelect
                        id='apply-version'
                        versions={sortedVersions(rule)}
                        value={version}
                        onChange={setVersion}
                    />
                </Field>
                <div className='flex flex-col gap-2'>
                    <span className='text-sm font-medium'>{t('rules.applyAgents')}</span>
                    {agents === null ? (
                        <Loading />
                    ) : agents.length === 0 ? (
                        <EmptyState icon='dns'>{t('agents.empty')}</EmptyState>
                    ) : (
                        <ul className='max-h-72 divide-y divide-line overflow-y-auto rounded-lg border border-line'>
                            {agents.map(a => (
                                <li key={a.id}>
                                    <label className='flex cursor-pointer items-center gap-3 px-3 py-2 hover:bg-canvas'>
                                        <input
                                            type='checkbox'
                                            className={checkboxClass}
                                            checked={selected.has(a.id)}
                                            onChange={() => toggle(a.id)}
                                        />
                                        <span className='flex-1'>
                                            <span className='font-medium'>{a.display_name}</span>
                                            <span className='ml-2 font-mono text-xs text-muted'>{a.hostname}</span>
                                        </span>
                                        <span className='text-xs text-muted'>
                                            {a.rule ? `${a.rule.name} v${a.rule.assigned_version}` : t('agents.noRule')}
                                        </span>
                                    </label>
                                </li>
                            ))}
                        </ul>
                    )}
                </div>
                {error && <Alert tone='danger'>{error}</Alert>}
            </form>
        </Modal>
    );
}

function ToTemplateModal({
    open,
    rule,
    onClose,
    onCreated,
}: {
    open: boolean;
    rule: RuleDetail;
    onClose: () => void;
    onCreated: (name: string) => void;
}) {
    const { t } = useTranslation();
    const [version, setVersion] = useState(rule.latest_version);
    const [name, setName] = useState('');
    const [description, setDescription] = useState('');
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState<string | null>(null);

    useEffect(() => {
        if (!open) return;
        setVersion(rule.latest_version);
        setName('');
        setDescription(rule.description);
        setError(null);
    }, [open, rule]);

    async function submit(e: FormEvent) {
        e.preventDefault();
        setBusy(true);
        setError(null);
        try {
            const tpl = await ruleToTemplate(rule.id, { version, name: name.trim(), description: description.trim() });
            onClose();
            onCreated(tpl.name);
        } catch (err) {
            setError(errorMessage(t, err));
        } finally {
            setBusy(false);
        }
    }

    return (
        <Modal
            open={open}
            title={t('rules.toTemplateTitle')}
            onClose={onClose}
            locked={busy}
            footer={
                <>
                    <Button onClick={onClose} disabled={busy}>
                        {t('common.cancel')}
                    </Button>
                    <Button variant='primary' type='submit' form='to-template-form' busy={busy} icon='library_add'>
                        {t('rules.toTemplate')}
                    </Button>
                </>
            }
        >
            <form id='to-template-form' onSubmit={e => void submit(e)} className='flex flex-col gap-4'>
                <Field label={t('rules.version')} htmlFor='tt-version'>
                    <VersionSelect
                        id='tt-version'
                        versions={sortedVersions(rule)}
                        value={version}
                        onChange={setVersion}
                    />
                </Field>
                <Field label={t('templates.name')} htmlFor='tt-name' required>
                    <input
                        id='tt-name'
                        className={inputClass}
                        value={name}
                        required
                        onChange={e => setName(e.target.value)}
                    />
                </Field>
                <Field label={t('templates.descriptionLabel')} htmlFor='tt-desc'>
                    <textarea
                        id='tt-desc'
                        className={inputClass}
                        rows={2}
                        value={description}
                        onChange={e => setDescription(e.target.value)}
                    />
                </Field>
                {error && <Alert tone='danger'>{error}</Alert>}
            </form>
        </Modal>
    );
}
