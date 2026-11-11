import { useTranslation } from 'react-i18next';
import { Link, useParams } from 'react-router-dom';
import { getAgent } from '../api/client';
import { Badge } from '../components/Badge';
import { Button } from '../components/Button';
import { Card, DescriptionList, PageHeader } from '../components/PageHeader';
import { Alert, EmptyState, ErrorAlert, Loading } from '../components/StateViews';
import { ApplyStatusBadge, TokenStatusBadge } from '../components/StatusBadges';
import { Table, TBody, Td, Th, THead, Tr } from '../components/Table';
import { useApi } from '../hooks/useApi';
import type { AgentDetail, ApplyResult, AuditdStatus, HostInfo } from '../types';
import { formatDateTime } from '../utils/format';
import { AgentFlags, AgentRuleCell, AuditdCell, ElapsedSinceReport, unackedAlertsLink } from './agentParts';

const linkClass =
    'text-accent hover:text-accent-hover hover:underline focus:outline-none focus-visible:ring-2 focus-visible:ring-accent';

export function AgentDetailPage() {
    const { t } = useTranslation();
    const { id = '' } = useParams();
    const { data: agent, error, loading, reload } = useApi(() => getAgent(id), [id]);

    return (
        <>
            <PageHeader
                breadcrumb={
                    <Link to='/agents' className={linkClass}>
                        {t('agents.title')}
                    </Link>
                }
                title={agent ? agent.display_name : t('agentDetail.title')}
                description={agent?.hostname}
                actions={
                    <Button icon='refresh' onClick={reload} busy={loading && !!agent}>
                        {t('common.reload')}
                    </Button>
                }
            />
            <ErrorAlert error={error} onRetry={reload} />
            {loading && !agent ? <Loading /> : agent && <AgentDetailBody agent={agent} />}
        </>
    );
}

function AgentDetailBody({ agent }: { agent: AgentDetail }) {
    const { t } = useTranslation();
    const host = agent.host_info;
    const auditd = agent.auditd ?? host?.auditd ?? null;

    return (
        <div className='flex flex-col gap-4'>
            <div className='grid grid-cols-1 gap-4 lg:grid-cols-2'>
                <Card title={t('agentDetail.summary')}>
                    <DescriptionList
                        items={[
                            {
                                label: t('agentDetail.kind'),
                                value: t(agent.kind === 'builtin' ? 'agents.builtin' : 'agentDetail.kindToken'),
                            },
                            { label: t('agents.hostname'), value: agent.hostname || '-' },
                            {
                                label: t('agents.token'),
                                value: agent.token ? (
                                    <span className='flex flex-wrap items-center gap-2'>
                                        <Link to='/tokens' className={linkClass}>
                                            {agent.token.name}
                                        </Link>
                                        <TokenStatusBadge status={agent.token.status} />
                                        <span className='text-xs text-muted'>
                                            {t('agents.expires', { date: formatDateTime(agent.token.expires_at) })}
                                        </span>
                                    </span>
                                ) : (
                                    '-'
                                ),
                            },
                            {
                                label: t('agents.lastReport'),
                                value: (
                                    <span className='flex flex-wrap gap-2'>
                                        <ElapsedSinceReport seconds={agent.seconds_since_report} />
                                        <span className='text-muted'>{formatDateTime(agent.last_report_at)}</span>
                                    </span>
                                ),
                            },
                            {
                                label: t('agents.unackedAlerts'),
                                value:
                                    agent.unacked_alerts > 0 ? (
                                        <Link to={unackedAlertsLink(agent.id)} className={`${linkClass} font-semibold`}>
                                            {t('agentDetail.unackedLink', { count: agent.unacked_alerts })}
                                        </Link>
                                    ) : (
                                        '0'
                                    ),
                            },
                            { label: t('agentDetail.droppedTotal'), value: agent.dropped_total },
                            { label: t('agents.state'), value: <AgentFlags agent={agent} /> },
                        ]}
                    />
                    {agent.dropped_total > 0 && (
                        <div className='mt-4'>
                            <Alert tone='warning'>{t('agentDetail.droppedNote')}</Alert>
                        </div>
                    )}
                </Card>
                <Card title={t('agentDetail.ruleAndApply')}>
                    <DescriptionList
                        items={[
                            { label: t('agents.rule'), value: <AgentRuleCell rule={agent.rule} /> },
                            ...(agent.rule
                                ? [
                                      {
                                          label: t('agentDetail.assignedVersion'),
                                          value: `v${agent.rule.assigned_version}`,
                                      },
                                      {
                                          label: t('agentDetail.reportedVersion'),
                                          value: agent.rule.reported_version ? `v${agent.rule.reported_version}` : '-',
                                      },
                                      { label: t('agentDetail.latestVersion'), value: `v${agent.rule.latest_version}` },
                                  ]
                                : []),
                        ]}
                    />
                    <div className='mt-4 border-t border-line pt-4'>
                        <ApplyResultView apply={agent.apply} />
                    </div>
                </Card>
            </div>

            {host ? (
                <HostInfoView host={host} auditd={auditd} />
            ) : (
                <Card title={t('agentDetail.hostInfo')}>
                    <EmptyState icon='hourglass_empty'>{t('agentDetail.noHostInfo')}</EmptyState>
                </Card>
            )}

            <Card title={t('agentDetail.suggestions')}>
                <p className='mb-3 text-xs text-muted'>{t('agentDetail.suggestionsHint')}</p>
                {(agent.suggestions ?? []).length === 0 ? (
                    <EmptyState icon='lightbulb'>{t('agentDetail.noSuggestions')}</EmptyState>
                ) : (
                    <Table>
                        <THead>
                            <Th>{t('agentDetail.suggestionKind')}</Th>
                            <Th>{t('agentDetail.suggestionValue')}</Th>
                            <Th>{t('agentDetail.suggestionReason')}</Th>
                        </THead>
                        <TBody>
                            {(agent.suggestions ?? []).map((s, i) => (
                                <Tr key={i}>
                                    <Td>
                                        <Badge tone='accent'>{t(`suggestionKind.${s.kind}`)}</Badge>
                                    </Td>
                                    <Td className='font-mono text-xs break-all'>{s.value}</Td>
                                    <Td className='text-muted'>{s.reason}</Td>
                                </Tr>
                            ))}
                        </TBody>
                    </Table>
                )}
            </Card>

            <Card title={t('agentDetail.tokenHistory')}>
                {(agent.token_history ?? []).length === 0 ? (
                    <EmptyState icon='history'>{t('common.empty')}</EmptyState>
                ) : (
                    <Table>
                        <THead>
                            <Th>{t('history.at')}</Th>
                            <Th>{t('history.account')}</Th>
                            <Th>{t('history.action')}</Th>
                            <Th>{t('history.detail')}</Th>
                        </THead>
                        <TBody>
                            {(agent.token_history ?? []).map(h => (
                                <Tr key={h.id}>
                                    <Td className='whitespace-nowrap'>{formatDateTime(h.at)}</Td>
                                    <Td>{h.account_name}</Td>
                                    <Td>{h.action}</Td>
                                    <Td className='text-muted'>{h.detail}</Td>
                                </Tr>
                            ))}
                        </TBody>
                    </Table>
                )}
            </Card>
        </div>
    );
}

function ApplyResultView({ apply }: { apply: ApplyResult }) {
    const { t } = useTranslation();
    const warnings = apply.warnings ?? [];
    return (
        <div className='flex flex-col gap-3'>
            <DescriptionList
                items={[
                    { label: t('agents.apply'), value: <ApplyStatusBadge status={apply.status} /> },
                    { label: t('agentDetail.applyVersion'), value: apply.version ? `v${apply.version}` : '-' },
                    { label: t('agentDetail.applyAt'), value: formatDateTime(apply.at) },
                    { label: t('agentDetail.applyMessage'), value: apply.message || '-' },
                ]}
            />
            {warnings.length > 0 && (
                <Alert tone='warning' title={t('agentDetail.applyWarnings')}>
                    <ul className='list-disc pl-5'>
                        {warnings.map((w, i) => (
                            <li key={i}>{w}</li>
                        ))}
                    </ul>
                </Alert>
            )}
        </div>
    );
}

function HostInfoView({ host, auditd }: { host: HostInfo; auditd: AuditdStatus | null }) {
    const { t } = useTranslation();
    const services = host.services ?? [];
    const dirs = host.directories ?? [];
    const cas = host.trusted_cas ?? [];

    return (
        <>
            <div className='grid grid-cols-1 gap-4 lg:grid-cols-2'>
                <Card title={t('agentDetail.hostInfo')}>
                    <DescriptionList
                        items={[
                            { label: t('agentDetail.collectedAt'), value: formatDateTime(host.collected_at) },
                            { label: t('agents.hostname'), value: host.hostname || '-' },
                            { label: t('agentDetail.os'), value: `${host.os_name} ${host.os_version}`.trim() || '-' },
                            { label: t('agentDetail.kernel'), value: host.kernel || '-' },
                            { label: t('agentDetail.agentVersion'), value: host.agent_version || '-' },
                            {
                                label: t('agentDetail.scanBase'),
                                value: host.scan_base ? (
                                    <code className='font-mono text-xs'>{host.scan_base}</code>
                                ) : (
                                    '-'
                                ),
                            },
                            {
                                label: t('agentDetail.hostRule'),
                                value: host.rule ? `#${host.rule.id} v${host.rule.version}` : '-',
                            },
                        ]}
                    />
                </Card>
                {auditd && <AuditdView auditd={auditd} />}
            </div>

            <Card title={t('agentDetail.services')}>
                {services.length === 0 ? (
                    <EmptyState icon='apps'>{t('common.empty')}</EmptyState>
                ) : (
                    <Table>
                        <THead>
                            <Th>{t('agentDetail.software')}</Th>
                            <Th>{t('agentDetail.process')}</Th>
                            <Th>{t('agentDetail.users')}</Th>
                            <Th className='text-right'>{t('agentDetail.processes')}</Th>
                        </THead>
                        <TBody>
                            {services.map((s, i) => (
                                <Tr key={i}>
                                    <Td className='font-medium'>{s.software}</Td>
                                    <Td className='font-mono text-xs'>{s.process}</Td>
                                    <Td>
                                        <div className='flex flex-wrap gap-1'>
                                            {(s.users ?? []).map(u => (
                                                <Badge key={u}>{u}</Badge>
                                            ))}
                                        </div>
                                    </Td>
                                    <Td className='text-right'>{s.processes}</Td>
                                </Tr>
                            ))}
                        </TBody>
                    </Table>
                )}
            </Card>

            <Card title={t('agentDetail.directories')}>
                {host.directories_truncated && (
                    <div className='mb-3'>
                        <Alert tone='info'>{t('agentDetail.directoriesTruncated')}</Alert>
                    </div>
                )}
                {dirs.length === 0 ? (
                    <EmptyState icon='folder'>{t('common.empty')}</EmptyState>
                ) : (
                    <div className='max-h-96 overflow-auto rounded-lg border border-line'>
                        <ul className='min-w-max divide-y divide-line font-mono text-xs'>
                            {dirs.map((d, i) => (
                                <li
                                    key={i}
                                    className='flex items-center gap-4 px-3 py-1 hover:bg-canvas'
                                    style={{ paddingLeft: `${0.75 + d.depth * 1.25}rem` }}
                                >
                                    <span className='flex flex-1 items-center gap-1 text-ink'>
                                        <span className='material-icons-outlined text-sm text-muted' aria-hidden='true'>
                                            folder
                                        </span>
                                        {d.path}
                                    </span>
                                    <span className='text-muted'>{d.mode}</span>
                                    <span className='text-muted'>
                                        {d.owner}:{d.group}
                                    </span>
                                </li>
                            ))}
                        </ul>
                    </div>
                )}
            </Card>

            <Card title={t('agentDetail.trustedCas')}>
                {cas.length === 0 ? (
                    <EmptyState icon='verified_user'>{t('common.empty')}</EmptyState>
                ) : (
                    <Table>
                        <THead>
                            <Th>{t('certificates.fingerprint')}</Th>
                            <Th>{t('certificates.notAfter')}</Th>
                        </THead>
                        <TBody>
                            {cas.map(ca => (
                                <Tr key={ca.fingerprint}>
                                    <Td className='font-mono text-xs break-all'>{ca.fingerprint}</Td>
                                    <Td className='whitespace-nowrap'>{formatDateTime(ca.not_after)}</Td>
                                </Tr>
                            ))}
                        </TBody>
                    </Table>
                )}
            </Card>
        </>
    );
}

function AuditdView({ auditd }: { auditd: AuditdStatus }) {
    const { t } = useTranslation();
    const affected = auditd.affected_processes ?? [];
    const halts = auditd.halt_actions ?? [];
    return (
        <Card title={t('agentDetail.auditd')}>
            <div className='flex flex-col gap-4'>
                <AuditdCell auditd={auditd} />
                <DescriptionList
                    items={[
                        { label: t('auditd.enabled'), value: auditd.enabled },
                        { label: t('auditd.pid'), value: auditd.pid || '-' },
                        { label: t('auditd.lostLabel'), value: auditd.lost },
                        { label: t('auditd.backlog'), value: `${auditd.backlog} / ${auditd.backlog_limit}` },
                        { label: t('auditd.backlogWaitTime'), value: auditd.backlog_wait_time },
                        { label: t('auditd.failure'), value: auditd.failure },
                        {
                            label: t('auditd.neverRuleLabel'),
                            value: auditd.never_rule ? (
                                <Badge tone='danger'>{t('common.yes')}</Badge>
                            ) : (
                                <span className='text-muted'>{t('common.no')}</span>
                            ),
                        },
                    ]}
                />
                {halts.length > 0 && (
                    <Alert tone='danger' title={t('auditd.haltActionsTitle')}>
                        <p>{t('auditd.haltActionsNote')}</p>
                        <ul className='mt-1 list-disc pl-5 font-mono text-xs'>
                            {halts.map(h => (
                                <li key={h}>{h}</li>
                            ))}
                        </ul>
                    </Alert>
                )}
                {affected.length > 0 && (
                    <Alert tone='warning' title={t('auditd.affectedTitle')}>
                        <p>{t('auditd.affectedNote')}</p>
                        <div className='mt-2 overflow-x-auto'>
                            <table className='min-w-full text-xs'>
                                <thead>
                                    <tr className='text-left text-muted'>
                                        <th className='pr-3 font-medium'>PID</th>
                                        <th className='pr-3 font-medium'>{t('auditd.comm')}</th>
                                        <th className='pr-3 font-medium'>{t('auditd.user')}</th>
                                        <th className='font-medium'>{t('auditd.startedAt')}</th>
                                    </tr>
                                </thead>
                                <tbody>
                                    {affected.map(p => (
                                        <tr key={p.pid} className='text-ink'>
                                            <td className='pr-3 font-mono'>{p.pid}</td>
                                            <td className='pr-3 font-mono'>{p.comm}</td>
                                            <td className='pr-3'>{p.user}</td>
                                            <td className='whitespace-nowrap'>{formatDateTime(p.started_at)}</td>
                                        </tr>
                                    ))}
                                </tbody>
                            </table>
                        </div>
                    </Alert>
                )}
            </div>
        </Card>
    );
}
