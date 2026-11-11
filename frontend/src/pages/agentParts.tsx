import { useTranslation } from 'react-i18next';
import { Link } from 'react-router-dom';
import { Badge } from '../components/Badge';
import type { AgentRuleRef, AgentSummary, AuditdStatus } from '../types';
import { formatElapsed } from '../utils/format';

// Reports older than this are highlighted as stale.
export const STALE_REPORT_SEC = 300;

/** Link to the detections list narrowed to an agent's unacknowledged alerts. */
export function unackedAlertsLink(agentId: number): string {
    return `/detections?agent_id=${agentId}&category=alert&acknowledged=false`;
}

/** Elapsed time since the last report, highlighted when stale. */
export function ElapsedSinceReport({ seconds }: { seconds: number | null }) {
    const { t } = useTranslation();
    const stale = seconds === null || seconds > STALE_REPORT_SEC;
    return (
        <span className={`whitespace-nowrap ${stale ? 'text-warning' : 'text-ink'}`}>{formatElapsed(t, seconds)}</span>
    );
}

/** Rule name, assigned version, and outdated / unreported badges. */
export function AgentRuleCell({ rule }: { rule: AgentRuleRef | null }) {
    const { t } = useTranslation();
    if (!rule) return <span className='text-muted'>{t('agents.noRule')}</span>;
    const mismatch = rule.reported_version !== rule.assigned_version;
    return (
        <div className='flex flex-col gap-1'>
            <Link
                to={`/rules/${rule.id}`}
                className='font-medium text-accent hover:text-accent-hover hover:underline focus:outline-none focus-visible:ring-2 focus-visible:ring-accent'
            >
                {rule.name}
            </Link>
            <div className='flex flex-wrap items-center gap-1'>
                <span className='text-xs text-muted'>v{rule.assigned_version}</span>
                {rule.outdated && (
                    <Badge
                        tone='warning'
                        icon='update'
                        title={t('agents.outdatedTitle', { latest: rule.latest_version })}
                    >
                        {t('agents.outdated')}
                    </Badge>
                )}
                {mismatch && (
                    <Badge
                        tone='accent'
                        icon='sync_problem'
                        title={t('agents.reportedMismatchTitle', {
                            reported: rule.reported_version ?? '-',
                            assigned: rule.assigned_version,
                        })}
                    >
                        {t('agents.reportedVersion', { version: rule.reported_version ?? '-' })}
                    </Badge>
                )}
            </div>
        </div>
    );
}

/** Compact auditd state for the agent list. */
export function AuditdCell({ auditd }: { auditd: AuditdStatus | null }) {
    const { t } = useTranslation();
    if (!auditd) return <span className='text-muted'>-</span>;
    return (
        <div className='flex flex-wrap gap-1'>
            {auditd.running ? (
                <Badge tone='success'>{t('auditd.running')}</Badge>
            ) : (
                <Badge tone='danger' icon='error'>
                    {t('auditd.stopped')}
                </Badge>
            )}
            {auditd.lost > 0 && (
                <Badge tone='warning' title={t('auditd.lostTitle')}>
                    {t('auditd.lost', { count: auditd.lost })}
                </Badge>
            )}
            {auditd.never_rule && (
                <Badge tone='danger' title={t('auditd.neverRuleTitle')}>
                    {t('auditd.neverRule')}
                </Badge>
            )}
            {(auditd.halt_actions?.length ?? 0) > 0 && (
                <Badge tone='danger' icon='power_settings_new' title={(auditd.halt_actions ?? []).join(', ')}>
                    {t('auditd.haltActions')}
                </Badge>
            )}
        </div>
    );
}

/** Small badges for builtin agent state and CA delivery. */
export function AgentFlags({ agent }: { agent: AgentSummary }) {
    const { t } = useTranslation();
    return (
        <div className='flex flex-wrap gap-1'>
            {agent.kind === 'builtin' && <Badge tone='accent'>{t('agents.builtin')}</Badge>}
            {agent.kind === 'builtin' && agent.builtin_running === false && (
                <Badge tone='danger' icon='error'>
                    {t('agents.builtinStopped')}
                </Badge>
            )}
            {!agent.ca_up_to_date && (
                <Badge tone='warning' icon='verified_user' title={t('agents.caOutdatedTitle')}>
                    {t('agents.caOutdated')}
                </Badge>
            )}
        </div>
    );
}
