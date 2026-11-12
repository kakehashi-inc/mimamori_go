import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Link } from 'react-router-dom';
import { ApiError, listAgents } from '../../api/client';
import { Badge } from '../../components/Badge';
import { Button, buttonClass } from '../../components/Button';
import { commandRunsPath, LatestRunCell } from '../../components/CommandResultViews';
import { ConfirmDialog } from '../../components/ConfirmDialog';
import { checkboxClass, inputClass, inputSmClass } from '../../components/FormControls';
import { Modal } from '../../components/Modal';
import { Card, DescriptionItem } from '../../components/PageHeader';
import { Alert, EmptyState, ErrorAlert, Loading } from '../../components/StateViews';
import { Table, TBody, Td, Th, THead, Tr } from '../../components/Table';
import { useToast } from '../../context/ToastContext';
import { useApi } from '../../hooks/useApi';
import type { AgentDetail, CommandRun, CommandRunDetail, CommandRunItem, CommandSet } from '../../types';
import { errorMessage, formatDateTime } from '../../utils/format';
import { deleteCommandSet, discardCommandRequest, importCommandSets, listCommandSets, requestCommandRun } from '../api';
import { hostZoneText, scheduleSummary } from '../components/commandSetDraft';
import { StepUpCancelled, useStepUp } from '../components/StepUp';

// Parts of the command set definitions: the section on the agent page
// (slot agentDetail.sections), how a run was started in the run history
// (slots commandRuns.headMiddle and commandRuns.rowMiddle) and in the run
// detail (slot commandRun.summaryMiddle), the number of commands run (slot
// commandRun.summaryEnd) and the command line and note of each command in the
// run detail (slot commandRun.itemStart).

/** Command sets of an agent with create, edit, delete, import, run now and discard request. */
export function CommandSetsSection({ agent, reload: reloadPage }: { agent: AgentDetail; reload: () => void }) {
    const { t } = useTranslation();
    const { showToast } = useToast();
    const stepUp = useStepUp();
    const sets$ = useApi(() => listCommandSets(agent.id), [agent.id]);
    const { data, error, loading } = sets$;
    // Reloads this section and the results shown above it.
    const reload = () => {
        sets$.reload();
        reloadPage();
    };
    const [busy, setBusy] = useState<number | null>(null);
    const [actionError, setActionError] = useState<string | null>(null);
    const [deleting, setDeleting] = useState<CommandSet | null>(null);
    const [importing, setImporting] = useState(false);
    const sets = data?.sets ?? [];
    const zone = data ? hostZoneText(data.time_zone, data.utc_offset_sec, data.time_zone !== '') : '';

    async function act(set: CommandSet, action: () => Promise<unknown>, done: string) {
        setBusy(set.id);
        setActionError(null);
        try {
            await action();
            showToast(done, 'success');
            reload();
        } catch (e) {
            setActionError(
                e instanceof ApiError && e.status === 409 ? t('commandSets.requestConflict') : errorMessage(t, e)
            );
            reload();
        } finally {
            setBusy(null);
        }
    }

    return (
        <Card
            title={t('commandSets.title')}
            actions={
                <>
                    <Button size='sm' icon='refresh' onClick={reload} busy={loading && !!data}>
                        {t('common.reload')}
                    </Button>
                    <Button size='sm' icon='file_download' onClick={() => setImporting(true)} disabled={!data}>
                        {t('commandSets.import')}
                    </Button>
                    <Link to={`/agents/${agent.id}/command-sets/new`} className={buttonClass('primary', 'sm')}>
                        <span className='material-icons-outlined text-sm' aria-hidden='true'>
                            add
                        </span>
                        {t('commandSets.create')}
                    </Link>
                </>
            }
        >
            <div className='flex flex-col gap-3'>
                <ErrorAlert error={error} onRetry={reload} />
                {data && (
                    <p className='text-xs text-muted'>
                        {zone ? t('commandSets.hostZone', { zone }) : t('commandSets.hostZoneUnknown')}
                    </p>
                )}
                {data && sets.length > 0 && !data.definitions_delivered && (
                    <Alert tone='info'>{t('commandSets.notDelivered')}</Alert>
                )}
                {actionError && <Alert tone='danger'>{actionError}</Alert>}
                {loading && !data ? (
                    <Loading />
                ) : (
                    data &&
                    (sets.length === 0 ? (
                        <EmptyState icon='terminal'>{t('commandSets.empty')}</EmptyState>
                    ) : (
                        <Table>
                            <THead>
                                <Th>{t('commandSets.fields.name')}</Th>
                                <Th>{t('commandSets.fields.purpose')}</Th>
                                <Th>{t('commandSets.whenRun')}</Th>
                                <Th>{t('commandSets.fields.osUser')}</Th>
                                <Th>{t('commandSets.lastRun')}</Th>
                                <Th>{t('commandSets.request')}</Th>
                                <Th className='text-right'>{t('common.actions')}</Th>
                            </THead>
                            <TBody>
                                {sets.map(s => (
                                    <Tr key={s.id}>
                                        <Td className='min-w-48'>
                                            <div className='flex flex-col gap-1'>
                                                <span className='font-medium break-words'>{s.name}</span>
                                                {s.description && (
                                                    <span className='text-xs break-words whitespace-pre-wrap text-muted'>
                                                        {s.description}
                                                    </span>
                                                )}
                                                <span className='text-xs text-muted'>
                                                    {t('commandSets.commandCount', { count: s.commands?.length ?? 0 })}
                                                </span>
                                                {(s.agent_problems ?? []).length > 0 && (
                                                    <Alert tone='warning' title={t('commandSets.agentProblems')}>
                                                        <ul className='list-disc pl-5 text-xs'>
                                                            {(s.agent_problems ?? []).map((p, i) => (
                                                                <li key={i}>{p}</li>
                                                            ))}
                                                        </ul>
                                                    </Alert>
                                                )}
                                            </div>
                                        </Td>
                                        <Td>
                                            <Badge tone={s.purpose === 'collect_output' ? 'accent' : 'neutral'}>
                                                {t(`commandSets.purpose.${s.purpose}`)}
                                            </Badge>
                                        </Td>
                                        <Td className='min-w-48 text-xs'>{scheduleSummary(t, s.schedule, zone)}</Td>
                                        <Td className='font-mono text-xs'>{s.os_user}</Td>
                                        <Td>
                                            <LatestRunCell run={s.last_run} running={s.running} />
                                        </Td>
                                        <Td className='min-w-40 text-xs'>
                                            {s.request ? (
                                                <div className='flex flex-col items-start gap-1'>
                                                    <Badge tone='accent' icon='schedule_send'>
                                                        {t(
                                                            `commandSets.requestState.${s.request.state === 'delivered' ? 'delivered' : 'pending'}`
                                                        )}
                                                    </Badge>
                                                    <span className='text-muted'>
                                                        {formatDateTime(s.request.requested_at)}
                                                    </span>
                                                    {s.request.state === 'pending' && (
                                                        <Button
                                                            size='sm'
                                                            variant='ghost'
                                                            icon='cancel_schedule_send'
                                                            disabled={busy === s.id}
                                                            onClick={() =>
                                                                void act(
                                                                    s,
                                                                    () =>
                                                                        discardCommandRequest(agent.id, s.request!.id),
                                                                    t('commandSets.discarded', { name: s.name })
                                                                )
                                                            }
                                                        >
                                                            {t('commandSets.discard')}
                                                        </Button>
                                                    )}
                                                </div>
                                            ) : (
                                                <span className='text-muted'>-</span>
                                            )}
                                        </Td>
                                        <Td>
                                            <div className='flex flex-wrap justify-end gap-1'>
                                                <Button
                                                    size='sm'
                                                    icon='play_arrow'
                                                    busy={busy === s.id}
                                                    disabled={!!s.request}
                                                    title={s.request ? t('commandSets.requestConflict') : undefined}
                                                    onClick={() =>
                                                        void act(
                                                            s,
                                                            () => requestCommandRun(agent.id, s.id),
                                                            t('commandSets.requested', { name: s.name })
                                                        )
                                                    }
                                                >
                                                    {t('commandSets.runNow')}
                                                </Button>
                                                <Link
                                                    to={`/agents/${agent.id}/command-sets/${s.id}/edit`}
                                                    className={buttonClass('secondary', 'sm')}
                                                >
                                                    {t('commandSets.edit')}
                                                </Link>
                                                <Link
                                                    to={commandRunsPath(agent.id, s.id)}
                                                    className={buttonClass('ghost', 'sm')}
                                                >
                                                    {t('commandResults.history')}
                                                </Link>
                                                <Button
                                                    size='sm'
                                                    variant='ghost'
                                                    icon='delete'
                                                    onClick={() => setDeleting(s)}
                                                    aria-label={t('commandSets.delete')}
                                                    title={t('commandSets.delete')}
                                                />
                                            </div>
                                        </Td>
                                    </Tr>
                                ))}
                            </TBody>
                        </Table>
                    ))
                )}
            </div>
            <ConfirmDialog
                open={deleting !== null}
                title={t('commandSets.deleteTitle')}
                message={t('commandSets.deleteConfirm', { name: deleting?.name ?? '' })}
                confirmLabel={t('commandSets.delete')}
                danger
                onConfirm={async () => {
                    if (!deleting) return;
                    await stepUp.run(() => deleteCommandSet(agent.id, deleting.id));
                    showToast(t('commandSets.deleted', { name: deleting.name }), 'success');
                    reload();
                }}
                onClose={() => setDeleting(null)}
            />
            {importing && data && (
                <ImportDialog
                    agentId={agent.id}
                    existing={sets.map(s => s.name)}
                    runChange={stepUp.run}
                    onClose={() => setImporting(false)}
                    onImported={count => {
                        setImporting(false);
                        showToast(t('commandSets.imported', { count }), 'success');
                        reload();
                    }}
                />
            )}
            {stepUp.dialog}
        </Card>
    );
}

interface ImportRow {
    checked: boolean;
    name: string;
}

function ImportDialog({
    agentId,
    existing,
    runChange,
    onClose,
    onImported,
}: {
    agentId: number;
    existing: string[];
    runChange: <T>(change: () => Promise<T>) => Promise<T>;
    onClose: () => void;
    onImported: (count: number) => void;
}) {
    const { t } = useTranslation();
    const agents = useApi(listAgents, []);
    const [source, setSource] = useState('');
    const sourceSets = useApi(() => (source ? listCommandSets(source) : Promise.resolve(null)), [source]);
    const [rows, setRows] = useState<Record<number, ImportRow>>({});
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState<string | null>(null);
    const list = sourceSets.data?.sets ?? [];

    const row = (id: number, name: string): ImportRow => rows[id] ?? { checked: false, name };
    const chosen = list.filter(s => row(s.id, s.name).checked);
    const finalName = (id: number, name: string) => row(id, name).name.trim() || name;

    // Names that clash with a set of this agent or with another set of the import.
    const clashes = (() => {
        const taken = new Set(existing);
        const seen = new Map<string, number>();
        for (const s of chosen) {
            const n = finalName(s.id, s.name);
            seen.set(n, (seen.get(n) ?? 0) + 1);
        }
        return new Set([...seen.entries()].filter(([n, c]) => c > 1 || taken.has(n)).map(([n]) => n));
    })();

    async function submit() {
        setBusy(true);
        setError(null);
        try {
            const items = chosen.map(s => ({ set_id: s.id, name: finalName(s.id, s.name) }));
            const r = await runChange(() => importCommandSets(agentId, Number(source), items));
            onImported(r.sets.length);
        } catch (e) {
            setError(e instanceof StepUpCancelled ? e.message : errorMessage(t, e));
        } finally {
            setBusy(false);
        }
    }

    return (
        <Modal
            open
            title={t('commandSets.importTitle')}
            onClose={onClose}
            size='lg'
            locked={busy}
            footer={
                <>
                    <Button onClick={onClose} disabled={busy}>
                        {t('common.cancel')}
                    </Button>
                    <Button
                        variant='primary'
                        icon='file_download'
                        busy={busy}
                        disabled={chosen.length === 0 || clashes.size > 0}
                        onClick={() => void submit()}
                    >
                        {t('commandSets.importSelected', { count: chosen.length })}
                    </Button>
                </>
            }
        >
            <div className='flex flex-col gap-4'>
                <p className='leading-relaxed text-muted'>{t('commandSets.importHint')}</p>
                <div className='flex flex-col gap-1'>
                    <label htmlFor='cs-import-source' className='text-sm font-medium text-ink'>
                        {t('commandSets.importSource')}
                    </label>
                    <select
                        id='cs-import-source'
                        className={inputClass}
                        value={source}
                        disabled={!agents.data}
                        onChange={e => {
                            setSource(e.target.value);
                            setRows({});
                        }}
                    >
                        <option value=''>{t('commandResults.selectAgent')}</option>
                        {(agents.data ?? []).map(a => (
                            <option key={a.id} value={a.id}>
                                {a.id === agentId
                                    ? t('commandSets.thisAgent', { name: a.display_name })
                                    : a.display_name}
                            </option>
                        ))}
                    </select>
                </div>
                <ErrorAlert error={agents.error ?? sourceSets.error} />
                {source && sourceSets.loading && !sourceSets.data ? (
                    <Loading />
                ) : (
                    source &&
                    sourceSets.data &&
                    (list.length === 0 ? (
                        <EmptyState icon='terminal'>{t('commandSets.importNone')}</EmptyState>
                    ) : (
                        <ul className='flex flex-col divide-y divide-line rounded-lg border border-line'>
                            {list.map(s => {
                                const r = row(s.id, s.name);
                                const clash = r.checked && clashes.has(finalName(s.id, s.name));
                                return (
                                    <li key={s.id} className='flex flex-col gap-2 px-3 py-2 sm:flex-row sm:items-start'>
                                        <label className='flex min-w-0 flex-1 items-start gap-2 text-sm text-ink'>
                                            <input
                                                type='checkbox'
                                                className={`${checkboxClass} mt-0.5`}
                                                checked={r.checked}
                                                onChange={e =>
                                                    setRows({ ...rows, [s.id]: { ...r, checked: e.target.checked } })
                                                }
                                            />
                                            <span className='flex min-w-0 flex-col'>
                                                <span className='font-medium break-words'>{s.name}</span>
                                                <span className='text-xs text-muted'>
                                                    {t(`commandSets.purpose.${s.purpose}`)} /{' '}
                                                    {t('commandSets.commandCount', { count: s.commands?.length ?? 0 })}
                                                </span>
                                            </span>
                                        </label>
                                        <div className='flex flex-col gap-1 sm:w-64'>
                                            <input
                                                className={`${inputSmClass} ${clash ? 'border-danger' : ''}`}
                                                aria-label={t('commandSets.importName', { name: s.name })}
                                                value={r.name}
                                                disabled={!r.checked}
                                                maxLength={100}
                                                onChange={e =>
                                                    setRows({ ...rows, [s.id]: { ...r, name: e.target.value } })
                                                }
                                            />
                                            {clash && (
                                                <span className='text-xs text-danger'>
                                                    {t('commandSets.nameClash')}
                                                </span>
                                            )}
                                        </div>
                                    </li>
                                );
                            })}
                        </ul>
                    ))
                )}
                {error && <Alert tone='danger'>{error}</Alert>}
            </div>
        </Modal>
    );
}

/** Manual or schedule, with the scheduled time of a scheduled run. */
function RunTrigger({ run }: { run: CommandRun }) {
    const { t } = useTranslation();
    if (!run.trigger) return <span className='text-muted'>-</span>;
    return (
        <span className='flex flex-col gap-0.5'>
            <span>{t(`commandSets.run.triggers.${run.trigger}`)}</span>
            {run.scheduled_for && (
                <span className='text-xs whitespace-nowrap text-muted'>
                    {t('commandSets.run.scheduledFor', { at: formatDateTime(run.scheduled_for) })}
                </span>
            )}
        </span>
    );
}

/** Heading of the run history column that says how each run was started. */
export function RunTriggerHead() {
    const { t } = useTranslation();
    return <Th>{t('commandSets.run.trigger')}</Th>;
}

/** Cell of the run history that says how the run was started. */
export function RunTriggerCell({ run }: { run: CommandRun }) {
    return (
        <Td>
            <RunTrigger run={run} />
        </Td>
    );
}

/** Item of the run detail summary that says how the run was started. */
export function RunTriggerItem({ run }: { run: CommandRunDetail }) {
    const { t } = useTranslation();
    return <DescriptionItem label={t('commandSets.run.trigger')} value={<RunTrigger run={run} />} />;
}

/** Item of the run detail summary with the number of commands run of all commands of the set. */
export function RunCommandsItem({ run }: { run: CommandRunDetail }) {
    const { t } = useTranslation();
    if (run.commands_total === undefined) return null;
    return (
        <DescriptionItem
            label={t('commandSets.run.commandsRun')}
            value={t('commandSets.run.commandsCount', { done: (run.commands ?? []).length, total: run.commands_total })}
        />
    );
}

/** Command line and note of one command in the run detail, as they were when it ran. */
export function CommandItemDefinition({ item }: { item: CommandRunItem }) {
    const { t } = useTranslation();
    const problems = item.values?.pattern_problems ?? [];
    if (item.command_line === undefined) return null;
    return (
        <div className='flex flex-col gap-2'>
            <div className='flex flex-col gap-1'>
                <span className='text-xs font-medium text-muted'>{t('commandSets.fields.commandLine')}</span>
                <pre className='max-h-64 overflow-auto rounded-lg border border-line bg-canvas p-3 font-mono text-xs leading-5 whitespace-pre-wrap text-ink'>
                    {item.command_line}
                </pre>
            </div>
            {item.note && (
                <p className='text-sm break-words whitespace-pre-wrap text-ink'>
                    <span className='mr-2 text-xs font-medium text-muted'>{t('commandSets.fields.note')}</span>
                    {item.note}
                </p>
            )}
            {problems.length > 0 && (
                <Alert tone='warning' title={t('commandSets.patternProblems')}>
                    <ul className='list-disc pl-5'>
                        {problems.map((p, i) => (
                            <li key={i}>
                                {t(`commandSets.patternProblem.${p.problem === 'timeout' ? 'timeout' : 'invalid'}`, {
                                    n: p.pattern + 1,
                                })}
                            </li>
                        ))}
                    </ul>
                </Alert>
            )}
        </div>
    );
}

/** Option of the history category filter for the command set history. */
export function CommandHistoryOption() {
    const { t } = useTranslation();
    return <option value='command'>{t('historyCategory.command')}</option>;
}
