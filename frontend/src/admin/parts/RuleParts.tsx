import { useEffect, useState, type FormEvent } from 'react';
import { useTranslation } from 'react-i18next';
import { Link, useNavigate } from 'react-router-dom';
import { ApiError, listAgents } from '../../api/client';
import { Button, buttonClass } from '../../components/Button';
import { ConfirmDialog } from '../../components/ConfirmDialog';
import { checkboxClass, Field, inputClass } from '../../components/FormControls';
import { Modal } from '../../components/Modal';
import { Alert, EmptyState, Loading } from '../../components/StateViews';
import { useToast } from '../../context/ToastContext';
import { sortedVersions, VersionSelect } from '../../pages/RuleDetailPage';
import type { AgentDetail, AgentSummary, CentralConfig, RuleDetail } from '../../types';
import { errorMessage } from '../../utils/format';
import { applyRule, deleteRule, getConfig, ruleToTemplate, unassignRule } from '../api';

// Parts placed in the rule list, the rule page and the agent page (slots
// rules.*, ruleDetail.* and agentDetail.ruleActions).

/**
 * Whether the built-in agent is disabled in the configuration the central
 * runs with: a restart item that is pending runs with the other value.
 * Without the configuration, a built-in agent that does not run (or is not
 * reported because it is disabled) is taken as disabled.
 */
function builtinDisabled(config: CentralConfig | null, agent: AgentSummary): boolean {
    if (agent.kind !== 'builtin') return false;
    const item = config?.items.find(i => i.name === 'builtin_agent');
    if (!item) return agent.builtin_running !== true;
    const enabled = item.value === 'true';
    return item.pending ? enabled : !enabled;
}

/** Link to the rule creation page. */
export function CreateRuleAction() {
    const { t } = useTranslation();
    return (
        <Link to='/rules/new' className={buttonClass('primary')}>
            <span className='material-icons-outlined text-base' aria-hidden='true'>
                add
            </span>
            {t('rules.create')}
        </Link>
    );
}

/** Edit, apply, turn into a template and delete, with their dialogs. */
export function RuleActions({ rule, reload }: { rule: RuleDetail; reload: () => void }) {
    const { t } = useTranslation();
    const navigate = useNavigate();
    const { showToast } = useToast();
    const [applyOpen, setApplyOpen] = useState(false);
    const [templateOpen, setTemplateOpen] = useState(false);
    const [deleteOpen, setDeleteOpen] = useState(false);
    return (
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
    // The built-in agent is not offered while it is disabled (it would never apply the rule).
    const [hiddenBuiltin, setHiddenBuiltin] = useState(false);
    const [selected, setSelected] = useState<Set<number>>(new Set());
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState<string | null>(null);

    useEffect(() => {
        if (!open) return;
        let active = true;
        setVersion(rule.latest_version);
        setError(null);
        setAgents(null);
        setHiddenBuiltin(false);
        setSelected(new Set());
        Promise.all([listAgents(), getConfig().catch(() => null)])
            .then(([list, config]) => {
                if (!active) return;
                const offered = list.filter(a => !builtinDisabled(config, a));
                const ids = new Set(offered.map(a => a.id));
                setAgents(offered);
                setHiddenBuiltin(offered.length < list.length);
                // Agents already using this rule are preselected so they can be upgraded in one step.
                setSelected(new Set((rule.applied_agents ?? []).map(a => a.agent_id).filter(id => ids.has(id))));
            })
            .catch(e => {
                if (!active) return;
                setAgents([]);
                setError(errorMessage(t, e));
            });
        return () => {
            active = false;
        };
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
                    {hiddenBuiltin && <p className='text-xs text-muted'>{t('rules.applyBuiltinDisabled')}</p>}
                </div>
                {error && <Alert tone='danger'>{error}</Alert>}
            </form>
        </Modal>
    );
}

/** Button on the agent page that takes the rule away from the agent, with its confirmation. */
export function RemoveRuleAction({ agent, reload }: { agent: AgentDetail; reload: () => void }) {
    const { t } = useTranslation();
    const { showToast } = useToast();
    const [open, setOpen] = useState(false);
    if (!agent.rule) return null;
    const ruleName = agent.rule.name;
    return (
        <>
            <Button icon='link_off' variant='danger' onClick={() => setOpen(true)}>
                {t('rules.unassign')}
            </Button>
            <ConfirmDialog
                open={open}
                title={t('rules.unassignTitle')}
                message={t('rules.unassignConfirm', { rule: ruleName, agent: agent.display_name })}
                confirmLabel={t('rules.unassign')}
                danger
                onClose={() => setOpen(false)}
                onConfirm={async () => {
                    await unassignRule([agent.id]);
                    showToast(t('rules.unassigned'));
                    reload();
                }}
            />
        </>
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
