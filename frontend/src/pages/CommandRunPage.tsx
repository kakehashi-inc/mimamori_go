import { useTranslation } from 'react-i18next';
import { Link, useParams } from 'react-router-dom';
import { getCommandRun } from '../api/client';
import { Button } from '../components/Button';
import {
    commandRunsPath,
    CommandStateBadge,
    DisplayValueView,
    formatBytes,
    textLinkClass,
    useReasonText,
} from '../components/CommandResultViews';
import { Card, DescriptionList, PageHeader } from '../components/PageHeader';
import { Slot } from '../components/Slot';
import { Alert, EmptyState, ErrorAlert, Loading } from '../components/StateViews';
import { useApi } from '../hooks/useApi';
import type { CommandRunDetail, CommandRunItem } from '../types';
import { formatDateTime } from '../utils/format';

/** One run of a command set: its summary, values and every command's result and console output. */
export function CommandRunPage() {
    const { t } = useTranslation();
    const { id = '', run = '' } = useParams();
    const { data, error, loading, reload } = useApi(() => getCommandRun(id, run), [id, run]);

    return (
        <>
            <PageHeader
                breadcrumb={
                    <span className='flex flex-wrap items-center gap-1'>
                        <Link to={`/agents/${id}`} className={textLinkClass}>
                            {data?.agent_name ?? t('agentDetail.title')}
                        </Link>
                        {data && (
                            <>
                                <span aria-hidden='true'>/</span>
                                <Link to={commandRunsPath(id, data.set_id)} className={textLinkClass}>
                                    {data.set_name}
                                </Link>
                            </>
                        )}
                    </span>
                }
                title={t('commandRun.title')}
                description={data ? `${data.set_name} - ${formatDateTime(data.started_at)}` : undefined}
                actions={
                    <Button icon='refresh' onClick={reload} busy={loading && !!data}>
                        {t('common.reload')}
                    </Button>
                }
            />
            <ErrorAlert error={error} onRetry={reload} />
            {loading && !data ? <Loading /> : data && <RunBody run={data} />}
        </>
    );
}

function RunBody({ run }: { run: CommandRunDetail }) {
    const { t } = useTranslation();
    const reasonText = useReasonText();
    const commands = run.commands ?? [];
    const finished = run.state !== 'running';
    // When the agent was stopped or ended during the run, the set itself did not stop.
    const interrupted =
        run.reason === 'interrupted' || (commands.length > 0 && commands[commands.length - 1].reason === 'interrupted');

    return (
        <div className='flex flex-col gap-4'>
            <div className='grid grid-cols-1 gap-4 lg:grid-cols-2'>
                <Card title={t('commandRun.summary')}>
                    <DescriptionList
                        items={[
                            { label: t('commandRun.agent'), value: run.agent_name },
                            { label: t('commandRun.set'), value: run.set_name },
                            { node: <Slot name='commandRun.summaryMiddle' run={run} /> },
                            { label: t('commandRuns.started'), value: formatDateTime(run.started_at) },
                            { label: t('commandRuns.finished'), value: formatDateTime(run.finished_at) },
                            { label: t('commandRuns.result'), value: <CommandStateBadge state={run.state} /> },
                            ...(run.reason ? [{ label: t('commandRun.reason'), value: reasonText(run.reason) }] : []),
                            { node: <Slot name='commandRun.summaryEnd' run={run} /> },
                            { label: t('commandRun.receivedAt'), value: formatDateTime(run.received_at) },
                        ]}
                    />
                </Card>
                {run.display && (
                    <Card title={t('commandRun.display')}>
                        <DisplayValueView display={run.display} />
                    </Card>
                )}
            </div>

            {!finished && <Alert tone='info'>{t('commandRun.stillRunning')}</Alert>}
            {finished && run.not_executed > 0 && (
                <Alert tone='warning'>
                    {t(interrupted ? 'commandRun.notExecutedCount' : 'commandRun.notExecuted', {
                        count: run.not_executed,
                    })}
                </Alert>
            )}

            <section className='flex flex-col gap-3'>
                <h2 className='text-sm font-semibold text-ink'>{t('commandRun.commands')}</h2>
                {commands.length === 0 ? (
                    <EmptyState icon='terminal'>{t('commandRun.noCommands')}</EmptyState>
                ) : (
                    commands.map(c => <CommandResultCard key={c.position} item={c} />)
                )}
            </section>
        </div>
    );
}

function CommandResultCard({ item }: { item: CommandRunItem }) {
    const { t } = useTranslation();
    const reasonText = useReasonText();
    return (
        <Card
            title={
                <span className='flex flex-wrap items-center gap-2'>
                    {t('commandRun.command', { n: item.position + 1 })}
                    <CommandStateBadge state={item.kind} />
                </span>
            }
        >
            <div className='flex flex-col gap-3'>
                <Slot name='commandRun.itemStart' item={item} />
                <DescriptionList
                    items={[
                        {
                            label: t('commandRun.exitCode'),
                            value:
                                item.exit_code === null ? (
                                    <span className='text-muted'>{t('commandRun.noExitCode')}</span>
                                ) : (
                                    <span className='font-mono'>{item.exit_code}</span>
                                ),
                        },
                        ...(item.reason ? [{ label: t('commandRun.reason'), value: reasonText(item.reason) }] : []),
                        { label: t('commandRuns.started'), value: formatDateTime(item.started_at) },
                        { label: t('commandRuns.finished'), value: formatDateTime(item.finished_at) },
                    ]}
                />
                <div className='flex flex-col gap-1'>
                    <div className='flex flex-wrap items-baseline justify-between gap-2'>
                        <h3 className='text-sm font-medium text-ink'>{t('commandRun.output')}</h3>
                        <span className='text-xs text-muted'>{formatBytes(item.output_bytes)}</span>
                    </div>
                    {item.output_truncated && (
                        <Alert tone='warning'>
                            {t('commandRun.outputTruncated', { size: formatBytes(item.output_bytes) })}
                        </Alert>
                    )}
                    {item.output === '' ? (
                        <p className='text-sm text-muted'>{t('commandRun.noOutput')}</p>
                    ) : (
                        <pre className='max-h-[32rem] overflow-auto rounded-lg border border-line bg-canvas p-3 font-mono text-xs leading-5 whitespace-pre text-ink'>
                            {item.output}
                        </pre>
                    )}
                </div>
            </div>
        </Card>
    );
}
