import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Link, useParams } from 'react-router-dom';
import { listCommandRuns } from '../api/client';
import { Badge } from '../components/Badge';
import { Button } from '../components/Button';
import {
    commandRunPath,
    CommandStateBadge,
    DisplayValueView,
    textLinkClass,
    useReasonText,
} from '../components/CommandResultViews';
import { PageHeader } from '../components/PageHeader';
import { Alert, EmptyState, ErrorAlert, Loading } from '../components/StateViews';
import { Table, TBody, Td, Th, THead, Tr } from '../components/Table';
import { useApi } from '../hooks/useApi';
import { formatDateTime } from '../utils/format';

/** Run history of one command set, newest first, a page at a time. */
export function CommandRunsPage() {
    const { id = '', set = '' } = useParams();
    // A new set starts again at its newest page.
    return <CommandRuns key={`${id}/${set}`} id={id} set={set} />;
}

function CommandRuns({ id, set }: { id: string; set: string }) {
    const { t } = useTranslation();
    const reasonText = useReasonText();
    // The "before" values of the pages shown so far; the last one is the current page.
    const [pages, setPages] = useState<(number | undefined)[]>([undefined]);
    const before = pages[pages.length - 1];
    const { data, error, loading, reload } = useApi(() => listCommandRuns(id, set, before), [id, set, before]);
    const runs = data?.runs ?? [];
    const showValues = runs.some(r => r.display);

    return (
        <>
            <PageHeader
                breadcrumb={
                    <span className='flex flex-wrap items-center gap-1'>
                        <Link to='/agents' className={textLinkClass}>
                            {t('agents.title')}
                        </Link>
                        <span aria-hidden='true'>/</span>
                        <Link to={`/agents/${id}`} className={textLinkClass}>
                            {data?.agent_name ?? t('agentDetail.title')}
                        </Link>
                    </span>
                }
                title={
                    <span className='flex flex-wrap items-center gap-2'>
                        {data?.set_name ?? t('commandRuns.title')}
                        {data?.deleted && <Badge tone='neutral'>{t('commandRuns.deleted')}</Badge>}
                    </span>
                }
                description={t('commandRuns.title')}
                actions={
                    <Button icon='refresh' onClick={reload} busy={loading && !!data}>
                        {t('common.reload')}
                    </Button>
                }
            />
            <div className='flex flex-col gap-3'>
                {data?.deleted && <Alert tone='info'>{t('commandRuns.deletedNote')}</Alert>}
                <ErrorAlert error={error} onRetry={reload} />
                {loading && !data ? (
                    <Loading />
                ) : data && runs.length === 0 ? (
                    <EmptyState icon='history'>{t('commandRuns.empty')}</EmptyState>
                ) : (
                    data && (
                        <Table>
                            <THead>
                                <Th>{t('commandRuns.started')}</Th>
                                <Th>{t('commandRuns.finished')}</Th>
                                <Th>{t('commandRuns.trigger')}</Th>
                                <Th>{t('commandRuns.result')}</Th>
                                {showValues && <Th className='w-full'>{t('commandRuns.value')}</Th>}
                                <Th>
                                    <span className='sr-only'>{t('commandRuns.detail')}</span>
                                </Th>
                            </THead>
                            <TBody>
                                {runs.map(run => (
                                    <Tr key={run.id}>
                                        <Td className='whitespace-nowrap'>{formatDateTime(run.started_at)}</Td>
                                        <Td className='whitespace-nowrap'>{formatDateTime(run.finished_at)}</Td>
                                        <Td>
                                            <div className='flex flex-col gap-0.5'>
                                                <span>{t(`commandRuns.triggers.${run.trigger}`)}</span>
                                                {run.scheduled_for && (
                                                    <span className='text-xs whitespace-nowrap text-muted'>
                                                        {t('commandRuns.scheduledFor', {
                                                            at: formatDateTime(run.scheduled_for),
                                                        })}
                                                    </span>
                                                )}
                                            </div>
                                        </Td>
                                        <Td>
                                            <div className='flex flex-col gap-1'>
                                                <CommandStateBadge state={run.state} />
                                                {run.reason && (
                                                    <span className='text-xs text-muted'>{reasonText(run.reason)}</span>
                                                )}
                                            </div>
                                        </Td>
                                        {showValues && (
                                            <Td className='min-w-64'>
                                                {run.display ? (
                                                    <DisplayValueView display={run.display} compact />
                                                ) : (
                                                    <span className='text-muted'>-</span>
                                                )}
                                            </Td>
                                        )}
                                        <Td className='whitespace-nowrap'>
                                            <Link
                                                to={commandRunPath(id, run.id)}
                                                className={`${textLinkClass} text-sm`}
                                            >
                                                {t('commandRuns.detail')}
                                            </Link>
                                        </Td>
                                    </Tr>
                                ))}
                            </TBody>
                        </Table>
                    )
                )}
                {data && (pages.length > 1 || data.next_before !== null) && (
                    <div className='flex justify-end gap-2'>
                        <Button
                            size='sm'
                            icon='chevron_left'
                            disabled={pages.length <= 1 || loading}
                            onClick={() => setPages(p => p.slice(0, -1))}
                        >
                            {t('commandRuns.newer')}
                        </Button>
                        <Button
                            size='sm'
                            icon='chevron_right'
                            disabled={data.next_before === null || loading}
                            onClick={() => {
                                const next = data.next_before;
                                if (next !== null) setPages(p => [...p, next]);
                            }}
                        >
                            {t('commandRuns.older')}
                        </Button>
                    </div>
                )}
            </div>
        </>
    );
}
