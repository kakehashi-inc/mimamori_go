import { useEffect, useMemo, useState, type FormEvent, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { Link, useSearchParams } from 'react-router-dom';
import { ackDetections, listAgents, listDetections } from '../api/client';
import { Badge } from '../components/Badge';
import { Button } from '../components/Button';
import { checkboxClass, inputSmAutoClass, inputSmClass } from '../components/FormControls';
import { PageHeader } from '../components/PageHeader';
import { Pagination } from '../components/Pagination';
import { EmptyState, ErrorAlert, Loading } from '../components/StateViews';
import { CategoryBadge } from '../components/StatusBadges';
import { SuppressionFormModal, type SuppressionPrefill } from '../components/SuppressionFormModal';
import { Table, TBody, Td, Th, THead, Tr } from '../components/Table';
import { useAuth } from '../context/AuthContext';
import { useToast } from '../context/ToastContext';
import { useApi } from '../hooks/useApi';
import type { AgentSummary, Detection } from '../types';
import { formatDateTime, localInputToRfc3339 } from '../utils/format';
import { DETECTION_TYPES } from '../utils/ruleContent';

// Rows per page and the choices offered.
const DEFAULT_LIMIT = 100;
const LIMIT_CHOICES = [50, 100, 200, 500];

// Filter keys kept in the URL query string.
const FILTER_KEYS = ['agent_id', 'category', 'type', 'from', 'to', 'acknowledged', 'suppressed'] as const;
type FilterKey = (typeof FILTER_KEYS)[number];
type Filters = Record<FilterKey, string>;

function readFilters(params: URLSearchParams): Filters {
    const f = {} as Filters;
    for (const k of FILTER_KEYS) f[k] = params.get(k) ?? '';
    return f;
}

function rowKey(d: Detection): string {
    return `${d.agent_id}:${d.id}`;
}

export function DetectionsPage() {
    const { t } = useTranslation();
    const { isAdmin } = useAuth();
    const { showToast } = useToast();
    const [params, setParams] = useSearchParams();
    const applied = useMemo(() => readFilters(params), [params]);
    const limit = Number(params.get('limit')) || DEFAULT_LIMIT;
    const offset = Number(params.get('offset')) || 0;
    const [draft, setDraft] = useState<Filters>(applied);
    const [agents, setAgents] = useState<AgentSummary[]>([]);
    const [selected, setSelected] = useState<Set<string>>(new Set());
    const [acking, setAcking] = useState(false);
    const [ackError, setAckError] = useState<unknown>(null);
    const [prefill, setPrefill] = useState<SuppressionPrefill | null>(null);

    const queryKey = params.toString();
    const { data, error, loading, reload } = useApi(
        () =>
            listDetections({
                ...applied,
                from: localInputToRfc3339(applied.from),
                to: localInputToRfc3339(applied.to),
                limit,
                offset,
            }),
        [queryKey]
    );
    const items = data?.items ?? [];

    useEffect(() => setDraft(applied), [applied]);
    useEffect(() => setSelected(new Set()), [queryKey]);
    useEffect(() => {
        listAgents()
            .then(setAgents)
            .catch(() => setAgents([]));
    }, []);

    function updateParams(next: Partial<Filters & { limit: string; offset: string }>) {
        const p = new URLSearchParams(params);
        for (const [k, v] of Object.entries(next)) {
            if (v) p.set(k, v);
            else p.delete(k);
        }
        setParams(p);
    }

    function submitFilters(e: FormEvent) {
        e.preventDefault();
        updateParams({ ...draft, offset: '' });
    }

    function clearFilters() {
        const empty = readFilters(new URLSearchParams());
        setDraft(empty);
        updateParams({ ...empty, offset: '' });
    }

    const selectable = items.filter(d => !d.acknowledged_at);
    const allSelected = selectable.length > 0 && selectable.every(d => selected.has(rowKey(d)));

    function toggle(d: Detection) {
        setSelected(prev => {
            const next = new Set(prev);
            const k = rowKey(d);
            if (next.has(k)) next.delete(k);
            else next.add(k);
            return next;
        });
    }

    function toggleAll() {
        setSelected(allSelected ? new Set() : new Set(selectable.map(rowKey)));
    }

    async function acknowledge() {
        const targets = items.filter(d => selected.has(rowKey(d))).map(d => ({ agent_id: d.agent_id, id: d.id }));
        if (targets.length === 0) return;
        setAcking(true);
        setAckError(null);
        try {
            const r = await ackDetections(targets);
            showToast(t('detections.acked', { count: r.acknowledged }));
            setSelected(new Set());
            reload();
        } catch (e) {
            setAckError(e);
        } finally {
            setAcking(false);
        }
    }

    const set = (k: FilterKey, v: string) => setDraft(d => ({ ...d, [k]: v }));
    const boolOptions = (yes: string, no: string) => (
        <>
            <option value=''>{t('common.all')}</option>
            <option value='true'>{yes}</option>
            <option value='false'>{no}</option>
        </>
    );

    return (
        <>
            <PageHeader
                title={t('detections.title')}
                description={t('detections.description')}
                actions={
                    <Button icon='refresh' onClick={reload} busy={loading && !!data}>
                        {t('common.reload')}
                    </Button>
                }
            />
            <form
                onSubmit={submitFilters}
                className='mb-4 grid grid-cols-1 gap-3 rounded-lg border border-line bg-surface p-4 sm:grid-cols-2 lg:grid-cols-4'
            >
                <FilterField label={t('detections.agent')} id='f-agent'>
                    <select
                        id='f-agent'
                        className={inputSmClass}
                        value={draft.agent_id}
                        onChange={e => set('agent_id', e.target.value)}
                    >
                        <option value=''>{t('common.all')}</option>
                        {agents.map(a => (
                            <option key={a.id} value={a.id}>
                                {a.display_name} ({a.hostname || '-'})
                            </option>
                        ))}
                    </select>
                </FilterField>
                <FilterField label={t('detections.category')} id='f-category'>
                    <select
                        id='f-category'
                        className={inputSmClass}
                        value={draft.category}
                        onChange={e => set('category', e.target.value)}
                    >
                        <option value=''>{t('common.all')}</option>
                        <option value='alert'>{t('category.alert')}</option>
                        <option value='normal'>{t('category.normal')}</option>
                    </select>
                </FilterField>
                <FilterField label={t('detections.type')} id='f-type'>
                    <select
                        id='f-type'
                        className={inputSmClass}
                        value={draft.type}
                        onChange={e => set('type', e.target.value)}
                    >
                        <option value=''>{t('common.all')}</option>
                        {DETECTION_TYPES.map(dt => (
                            <option key={dt} value={dt}>
                                {t(`detectionType.${dt}`)}
                            </option>
                        ))}
                    </select>
                </FilterField>
                <FilterField label={t('detections.acknowledged')} id='f-ack'>
                    <select
                        id='f-ack'
                        className={inputSmClass}
                        value={draft.acknowledged}
                        onChange={e => set('acknowledged', e.target.value)}
                    >
                        {boolOptions(t('detections.ackYes'), t('detections.ackNo'))}
                    </select>
                </FilterField>
                <FilterField label={t('detections.from')} id='f-from'>
                    <input
                        id='f-from'
                        type='datetime-local'
                        step={1}
                        className={inputSmClass}
                        value={draft.from}
                        onChange={e => set('from', e.target.value)}
                    />
                </FilterField>
                <FilterField label={t('detections.to')} id='f-to'>
                    <input
                        id='f-to'
                        type='datetime-local'
                        step={1}
                        className={inputSmClass}
                        value={draft.to}
                        onChange={e => set('to', e.target.value)}
                    />
                </FilterField>
                <FilterField label={t('detections.suppressed')} id='f-sup'>
                    <select
                        id='f-sup'
                        className={inputSmClass}
                        value={draft.suppressed}
                        onChange={e => set('suppressed', e.target.value)}
                    >
                        {boolOptions(t('detections.suppressedYes'), t('detections.suppressedNo'))}
                    </select>
                </FilterField>
                <div className='flex items-end gap-2'>
                    <Button type='submit' variant='primary' icon='filter_alt'>
                        {t('common.search')}
                    </Button>
                    <Button onClick={clearFilters} icon='filter_alt_off'>
                        {t('common.clear')}
                    </Button>
                </div>
            </form>

            <div className='flex flex-col gap-3'>
                <ErrorAlert error={error} onRetry={reload} />
                {ackError ? <ErrorAlert error={ackError} /> : null}
                <div className='flex flex-wrap items-center justify-between gap-2'>
                    <div className='flex items-center gap-2'>
                        {isAdmin && (
                            <Button
                                variant='primary'
                                icon='done_all'
                                disabled={selected.size === 0}
                                busy={acking}
                                onClick={() => void acknowledge()}
                            >
                                {t('detections.ack')}
                                {selected.size > 0 && ` (${selected.size})`}
                            </Button>
                        )}
                    </div>
                    <label className='flex items-center gap-2 text-sm text-muted'>
                        {t('pagination.perPage')}
                        <select
                            className={`${inputSmAutoClass} shrink-0`}
                            value={limit}
                            onChange={e => updateParams({ limit: e.target.value, offset: '' })}
                        >
                            {LIMIT_CHOICES.map(n => (
                                <option key={n} value={n}>
                                    {n}
                                </option>
                            ))}
                        </select>
                    </label>
                </div>

                {loading && !data ? (
                    <Loading />
                ) : data && items.length === 0 ? (
                    <EmptyState icon='notifications_none'>{t('detections.empty')}</EmptyState>
                ) : (
                    data && (
                        <Table>
                            <THead>
                                {isAdmin && (
                                    <Th className='w-8'>
                                        <input
                                            type='checkbox'
                                            className={checkboxClass}
                                            checked={allSelected}
                                            disabled={selectable.length === 0}
                                            onChange={toggleAll}
                                            aria-label={t('detections.selectAll')}
                                        />
                                    </Th>
                                )}
                                <Th>{t('detections.lastAt')}</Th>
                                <Th>{t('detections.agent')}</Th>
                                <Th>{t('detections.category')}</Th>
                                <Th>{t('detections.type')}</Th>
                                <Th>{t('detections.osUser')}</Th>
                                <Th>{t('detections.exe')}</Th>
                                <Th>{t('detections.target')}</Th>
                                <Th className='text-right'>{t('detections.count')}</Th>
                                <Th>{t('detections.firstAt')}</Th>
                                <Th>{t('detections.file')}</Th>
                                <Th>{t('detections.ackState')}</Th>
                                <Th>{t('detections.suppressed')}</Th>
                                {isAdmin && <Th />}
                            </THead>
                            <TBody>
                                {items.map(d => (
                                    <Tr key={rowKey(d)} selected={selected.has(rowKey(d))}>
                                        {isAdmin && (
                                            <Td>
                                                <input
                                                    type='checkbox'
                                                    className={checkboxClass}
                                                    checked={selected.has(rowKey(d))}
                                                    disabled={!!d.acknowledged_at}
                                                    onChange={() => toggle(d)}
                                                    aria-label={t('detections.select')}
                                                />
                                            </Td>
                                        )}
                                        <Td className='whitespace-nowrap'>{formatDateTime(d.last_at)}</Td>
                                        <Td>
                                            <Link
                                                to={`/agents/${d.agent_id}`}
                                                className='text-accent hover:text-accent-hover hover:underline'
                                            >
                                                {d.agent_name}
                                            </Link>
                                            <div className='font-mono text-xs text-muted'>{d.hostname}</div>
                                        </Td>
                                        <Td>
                                            <CategoryBadge category={d.category} />
                                        </Td>
                                        <Td className='whitespace-nowrap'>{t(`detectionType.${d.type}`)}</Td>
                                        <Td className='font-mono text-xs'>{d.os_user || '-'}</Td>
                                        <Td className='max-w-64 font-mono text-xs break-all'>{d.exe || '-'}</Td>
                                        <Td className='min-w-64 max-w-96 font-mono text-xs break-all'>
                                            {d.target || '-'}
                                        </Td>
                                        <Td className='text-right'>{d.count}</Td>
                                        <Td className='whitespace-nowrap'>{formatDateTime(d.first_at)}</Td>
                                        <Td className='text-xs'>
                                            {d.file ? (
                                                <div className='flex flex-col whitespace-nowrap'>
                                                    <span className='font-mono'>
                                                        {d.file.mode} {d.file.owner}
                                                    </span>
                                                    <span className='text-muted'>
                                                        ctime {formatDateTime(d.file.ctime)}
                                                    </span>
                                                </div>
                                            ) : (
                                                <span className='text-muted'>-</span>
                                            )}
                                        </Td>
                                        <Td>
                                            {d.acknowledged_at ? (
                                                <Badge
                                                    tone='success'
                                                    icon='check'
                                                    title={`${d.acknowledged_by} ${formatDateTime(d.acknowledged_at)}`}
                                                >
                                                    {t('detections.ackYes')}
                                                </Badge>
                                            ) : (
                                                <Badge tone='neutral'>{t('detections.ackNo')}</Badge>
                                            )}
                                        </Td>
                                        <Td>
                                            {d.suppressed ? (
                                                <Badge tone='neutral' icon='block'>
                                                    {t('detections.suppressedYes')}
                                                </Badge>
                                            ) : (
                                                <span className='text-muted'>-</span>
                                            )}
                                        </Td>
                                        {isAdmin && (
                                            <Td>
                                                <Button
                                                    size='sm'
                                                    icon='block'
                                                    onClick={() =>
                                                        setPrefill({
                                                            agent_id: d.agent_id,
                                                            agent_name: d.agent_name,
                                                            type: d.type,
                                                            os_user: d.os_user,
                                                            exe: d.exe,
                                                            target: d.target,
                                                        })
                                                    }
                                                >
                                                    {t('detections.toSuppression')}
                                                </Button>
                                            </Td>
                                        )}
                                    </Tr>
                                ))}
                            </TBody>
                        </Table>
                    )
                )}
                {data && data.total > 0 && (
                    <Pagination
                        total={data.total}
                        limit={limit}
                        offset={offset}
                        onChange={o => updateParams({ offset: o ? String(o) : '' })}
                    />
                )}
            </div>

            {isAdmin && (
                <SuppressionFormModal
                    open={prefill !== null}
                    prefill={prefill}
                    onClose={() => setPrefill(null)}
                    onCreated={() => {
                        showToast(t('suppressions.created'));
                        reload();
                    }}
                />
            )}
        </>
    );
}

function FilterField({ label, id, children }: { label: string; id: string; children: ReactNode }) {
    return (
        <div className='flex flex-col gap-1'>
            <label htmlFor={id} className='text-xs font-medium text-muted'>
                {label}
            </label>
            {children}
        </div>
    );
}
