import { useTranslation } from 'react-i18next';
import { Link } from 'react-router-dom';
import type { CommandKind, CommandRun, CommandRunState, DisplayValue } from '../types';
import { formatDateTime } from '../utils/format';
import { Badge, type BadgeTone } from './Badge';

// Parts shared by the views of command set results (agent page, results
// page, run history and run detail).

const stateTone: Record<CommandRunState, BadgeTone> = {
    running: 'accent',
    success: 'success',
    failure: 'warning',
    error: 'danger',
};

const stateIcon: Record<CommandRunState, string> = {
    running: 'autorenew',
    success: 'check_circle',
    failure: 'cancel',
    error: 'error',
};

/** Result of a run or of one command: running, success, failure or error. */
export function CommandStateBadge({ state }: { state: CommandRunState | CommandKind }) {
    const { t } = useTranslation();
    return (
        <Badge tone={stateTone[state] ?? 'neutral'} icon={stateIcon[state]}>
            {t(`commandState.${state}`)}
        </Badge>
    );
}

const REASONS = ['unexpected_code', 'start_failed', 'workdir_failed', 'timeout', 'signaled', 'interrupted'] as const;
type Reason = (typeof REASONS)[number];

/** Text of an error reason; unknown reasons are shown as they are. */
export function useReasonText(): (reason: string) => string {
    const { t } = useTranslation();
    return reason =>
        (REASONS as readonly string[]).includes(reason) ? t(`commandReason.${reason as Reason}`) : reason;
}

/** Link path of the run history of a set. */
export function commandRunsPath(agentId: number | string, setId: number | string): string {
    return `/agents/${agentId}/command-sets/${setId}/runs`;
}

/** Link path of one run. */
export function commandRunPath(agentId: number | string, runId: number | string): string {
    return `/agents/${agentId}/command-runs/${runId}`;
}

export const textLinkClass =
    'text-accent hover:text-accent-hover hover:underline focus:outline-none focus-visible:ring-2 focus-visible:ring-accent rounded';

/** The display value of a run as rows of group name and values. */
export function DisplayValueView({ display, compact }: { display: DisplayValue | null; compact?: boolean }) {
    const { t } = useTranslation();
    const rows = display?.rows ?? [];
    if (rows.length === 0) return <span className='text-sm text-muted'>{t('commandResults.noValue')}</span>;
    return (
        <div className='flex flex-col gap-1'>
            <dl
                className={`grid grid-cols-[max-content_minmax(0,1fr)] gap-x-3 gap-y-1 text-sm ${compact ? 'max-h-48 overflow-y-auto' : ''}`}
            >
                {rows.map((row, i) => (
                    <div key={i} className='contents'>
                        <dt className={`font-medium ${row.name ? 'text-ink' : 'text-muted'}`}>
                            {row.name || t('commandResults.output')}
                        </dt>
                        <dd className='font-mono text-xs leading-5 break-words whitespace-pre-wrap text-ink'>
                            {row.value}
                        </dd>
                    </div>
                ))}
            </dl>
            {display?.truncated && <p className='text-xs text-muted'>{t('commandResults.truncated')}</p>}
        </div>
    );
}

/** Result and end time of the latest run of a set, with a mark while another run is going on. */
export function LatestRunCell({ run, running }: { run: CommandRun | null; running: boolean }) {
    const { t } = useTranslation();
    return (
        <div className='flex flex-col gap-1'>
            <div className='flex flex-wrap items-center gap-1'>
                {run ? (
                    <CommandStateBadge state={run.state} />
                ) : (
                    <span className='text-xs whitespace-nowrap text-muted'>{t('commandResults.notRun')}</span>
                )}
                {running && run?.state !== 'running' && <CommandStateBadge state='running' />}
            </div>
            {run?.finished_at && (
                <span className='text-xs whitespace-nowrap text-muted'>{formatDateTime(run.finished_at)}</span>
            )}
        </div>
    );
}

/** Link to the run history of a set. */
export function RunHistoryLink({ agentId, setId, label }: { agentId: number; setId: number; label?: string }) {
    const { t } = useTranslation();
    return (
        <Link
            to={commandRunsPath(agentId, setId)}
            className={`${textLinkClass} inline-flex items-center gap-1 text-sm`}
        >
            <span className='material-icons-outlined text-base' aria-hidden='true'>
                history
            </span>
            {label ?? t('commandResults.history')}
        </Link>
    );
}

/** Size in bytes as B, KiB or MiB. */
export function formatBytes(bytes: number): string {
    if (bytes < 1024) return `${bytes} B`;
    if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KiB`;
    return `${(bytes / 1024 / 1024).toFixed(1)} MiB`;
}
