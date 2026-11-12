import { useTranslation } from 'react-i18next';
import { getAgentCommandResults } from '../api/client';
import { Button } from '../components/Button';
import { CommandStateBadge, DisplayValueView, LatestRunCell, RunHistoryLink } from '../components/CommandResultViews';
import { Card } from '../components/PageHeader';
import { EmptyState, ErrorAlert, Loading } from '../components/StateViews';
import { Table, TBody, Td, Th, THead, Tr } from '../components/Table';
import { useApi } from '../hooks/useApi';
import type { CommandLatest } from '../types';

/** Latest values of the command sets of one agent and links to the run history of every set. */
export function AgentCommandResults({
    agentId,
    title,
    refresh = 0,
}: {
    agentId: number;
    title?: string;
    /** Loads the results again when it changes. */
    refresh?: number;
}) {
    const { t } = useTranslation();
    const { data, error, loading, reload } = useApi(() => getAgentCommandResults(agentId), [agentId, refresh]);
    const results = data?.results ?? [];
    const sets = data?.sets ?? [];

    return (
        <Card
            title={title ?? t('commandResults.section')}
            actions={
                <Button size='sm' icon='refresh' onClick={reload} busy={loading && !!data}>
                    {t('common.reload')}
                </Button>
            }
        >
            <ErrorAlert error={error} onRetry={reload} />
            {loading && !data ? (
                <Loading />
            ) : (
                data &&
                (sets.length === 0 ? (
                    <EmptyState icon='terminal'>{t('commandResults.noSets')}</EmptyState>
                ) : (
                    <div className='flex flex-col gap-5'>
                        <section className='flex flex-col gap-2'>
                            <h3 className='text-sm font-medium text-ink'>{t('commandResults.latest')}</h3>
                            {results.length === 0 ? (
                                <p className='text-sm text-muted'>{t('commandResults.noLatest')}</p>
                            ) : (
                                <LatestTable items={results} agentId={agentId} />
                            )}
                        </section>
                        <section className='flex flex-col gap-2'>
                            <h3 className='text-sm font-medium text-ink'>{t('commandResults.allSets')}</h3>
                            <ul className='flex flex-col divide-y divide-line rounded-lg border border-line text-sm'>
                                {sets.map(s => (
                                    <li
                                        key={s.set_id}
                                        className='flex flex-wrap items-center justify-between gap-2 px-3 py-2 hover:bg-canvas'
                                    >
                                        <span className='min-w-0 font-medium break-words text-ink'>{s.set_name}</span>
                                        <span className='flex flex-wrap items-center gap-3'>
                                            {s.running && <CommandStateBadge state='running' />}
                                            {s.last_run ? (
                                                <CommandStateBadge state={s.last_run.state} />
                                            ) : (
                                                <span className='text-xs text-muted'>{t('commandResults.notRun')}</span>
                                            )}
                                            <RunHistoryLink agentId={agentId} setId={s.set_id} />
                                        </span>
                                    </li>
                                ))}
                            </ul>
                        </section>
                    </div>
                ))
            )}
        </Card>
    );
}

/** Table of the latest values; the first column is the set name, or the agent name when byAgent is set. */
export function LatestTable({
    items,
    agentId,
    byAgent,
}: {
    items: CommandLatest[];
    agentId?: number;
    byAgent?: boolean;
}) {
    const { t } = useTranslation();
    return (
        <Table>
            <THead>
                <Th>{byAgent ? t('commandResults.agent') : t('commandResults.set')}</Th>
                <Th className='w-full'>{t('commandResults.value')}</Th>
                <Th>{t('commandResults.result')}</Th>
                <Th>
                    <span className='sr-only'>{t('commandResults.history')}</span>
                </Th>
            </THead>
            <TBody>
                {items.map(item => (
                    <Tr key={`${item.agent_id}-${item.set_id}`}>
                        <Td className='font-medium break-words'>{byAgent ? item.agent_name : item.set_name}</Td>
                        <Td className='min-w-64'>
                            {item.last_run ? (
                                <DisplayValueView display={item.last_run.display} compact />
                            ) : (
                                <span className='text-sm text-muted'>-</span>
                            )}
                        </Td>
                        <Td>
                            <LatestRunCell run={item.last_run} running={item.running} />
                        </Td>
                        <Td className='whitespace-nowrap'>
                            <RunHistoryLink agentId={agentId ?? item.agent_id} setId={item.set_id} />
                        </Td>
                    </Tr>
                ))}
            </TBody>
        </Table>
    );
}
