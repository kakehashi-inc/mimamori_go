import { useTranslation } from 'react-i18next';
import { Link } from 'react-router-dom';
import { listAgents } from '../api/client';
import { Button } from '../components/Button';
import { PageHeader } from '../components/PageHeader';
import { EmptyState, ErrorAlert, Loading } from '../components/StateViews';
import { ApplyStatusBadge, TokenStatusBadge } from '../components/StatusBadges';
import { Table, TBody, Td, Th, THead, Tr } from '../components/Table';
import { useApi } from '../hooks/useApi';
import { formatDateTime } from '../utils/format';
import { serverText } from '../utils/serverText';
import { AgentFlags, AgentRuleCell, AuditdCell, ElapsedSinceReport, unackedAlertsLink } from './agentParts';

export function AgentsPage() {
    const { t } = useTranslation();
    const { data, error, loading, reload } = useApi(listAgents, []);

    return (
        <>
            <PageHeader
                title={t('agents.title')}
                description={t('agents.description')}
                actions={
                    <Button icon='refresh' onClick={reload} busy={loading && !!data}>
                        {t('common.reload')}
                    </Button>
                }
            />
            <div className='flex flex-col gap-4'>
                <ErrorAlert error={error} onRetry={reload} />
                {loading && !data ? (
                    <Loading />
                ) : data && data.length === 0 ? (
                    <EmptyState icon='dns'>{t('agents.empty')}</EmptyState>
                ) : (
                    data && (
                        <Table>
                            <THead>
                                <Th>{t('agents.name')}</Th>
                                <Th>{t('agents.hostname')}</Th>
                                <Th>{t('agents.token')}</Th>
                                <Th>{t('agents.lastReport')}</Th>
                                <Th>{t('agents.rule')}</Th>
                                <Th>{t('agents.apply')}</Th>
                                <Th className='text-right'>{t('agents.unackedAlerts')}</Th>
                                <Th>{t('agents.auditd')}</Th>
                                <Th>{t('agents.state')}</Th>
                            </THead>
                            <TBody>
                                {data.map(a => (
                                    <Tr key={a.id}>
                                        <Td>
                                            <Link
                                                to={`/agents/${a.id}`}
                                                className='font-medium text-accent hover:text-accent-hover hover:underline focus:outline-none focus-visible:ring-2 focus-visible:ring-accent'
                                            >
                                                {a.display_name}
                                            </Link>
                                        </Td>
                                        <Td className='font-mono text-xs'>{a.hostname || '-'}</Td>
                                        <Td>
                                            {a.token ? (
                                                <div className='flex flex-col gap-1'>
                                                    <TokenStatusBadge status={a.token.status} />
                                                    <span className='text-xs whitespace-nowrap text-muted'>
                                                        {t('agents.expires', {
                                                            date: formatDateTime(a.token.expires_at),
                                                        })}
                                                    </span>
                                                </div>
                                            ) : (
                                                <span className='text-muted'>-</span>
                                            )}
                                        </Td>
                                        <Td>
                                            <ElapsedSinceReport seconds={a.seconds_since_report} />
                                            <div className='text-xs whitespace-nowrap text-muted'>
                                                {formatDateTime(a.last_report_at)}
                                            </div>
                                        </Td>
                                        <Td>
                                            <AgentRuleCell rule={a.rule} />
                                        </Td>
                                        <Td className='max-w-64'>
                                            <ApplyStatusBadge status={a.apply.status} />
                                            {a.apply.message && (
                                                <p
                                                    className='mt-1 line-clamp-2 text-xs break-words text-muted'
                                                    title={serverText(t, a.apply.message_i18n, a.apply.message)}
                                                >
                                                    {serverText(t, a.apply.message_i18n, a.apply.message)}
                                                </p>
                                            )}
                                        </Td>
                                        <Td className='text-right'>
                                            {a.unacked_alerts > 0 ? (
                                                <Link
                                                    to={unackedAlertsLink(a.id)}
                                                    className='inline-flex items-center gap-1 rounded-full bg-danger-soft px-2 py-0.5 text-xs font-semibold text-danger hover:underline focus:outline-none focus-visible:ring-2 focus-visible:ring-accent'
                                                >
                                                    <span
                                                        className='material-icons-outlined text-sm'
                                                        aria-hidden='true'
                                                    >
                                                        notifications_active
                                                    </span>
                                                    {a.unacked_alerts}
                                                </Link>
                                            ) : (
                                                <span className='text-muted'>0</span>
                                            )}
                                        </Td>
                                        <Td>
                                            <AuditdCell auditd={a.auditd} />
                                        </Td>
                                        <Td>
                                            <AgentFlags agent={a} />
                                        </Td>
                                    </Tr>
                                ))}
                            </TBody>
                        </Table>
                    )
                )}
            </div>
        </>
    );
}
