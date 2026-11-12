import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Link, useParams } from 'react-router-dom';
import { getRule, getRuleDiff, getRuleVersion } from '../api/client';
import { AppliedAgentsList } from '../components/AppliedAgentsList';
import { Button } from '../components/Button';
import { DiffView } from '../components/DiffView';
import { inputSmAutoClass } from '../components/FormControls';
import { Card, PageHeader } from '../components/PageHeader';
import { RuleContentView } from '../components/RuleContentView';
import { Slot } from '../components/Slot';
import { EmptyState, ErrorAlert, Loading } from '../components/StateViews';
import { Table, TBody, Td, Th, THead, Tr } from '../components/Table';
import { Tabs } from '../components/Tabs';
import { useApi } from '../hooks/useApi';
import type { RuleDetail, RuleVersionInfo } from '../types';
import { formatDateTime, formatOperator } from '../utils/format';

type Tab = 'content' | 'versions' | 'diff';

/** Versions of a rule, newest first. */
export function sortedVersions(rule: RuleDetail): RuleVersionInfo[] {
    return [...(rule.versions ?? [])].sort((a, b) => b.version - a.version);
}

export function RuleDetailPage() {
    const { t } = useTranslation();
    const { id = '' } = useParams();
    const { data: rule, error, loading, reload } = useApi(() => getRule(id), [id]);
    const [tab, setTab] = useState<Tab>('content');
    const [viewVersion, setViewVersion] = useState(0);
    const [diffFrom, setDiffFrom] = useState(0);
    const [diffTo, setDiffTo] = useState(0);

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
                actions={rule && <Slot name='ruleDetail.headerActions' rule={rule} reload={reload} />}
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
        </>
    );
}

/** Select box of a rule's versions. */
export function VersionSelect({
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
                        {formatOperator(info.created_by)} / {info.comment || t('rules.noComment')}
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
                        <Td>{formatOperator(v.created_by)}</Td>
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
