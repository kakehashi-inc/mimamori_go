import { useTranslation } from 'react-i18next';
import { Link } from 'react-router-dom';
import type { AppliedAgent } from '../types';

/** Applied agents with their version, marking those behind the latest. */
export function AppliedAgentsList({ agents, latest }: { agents: AppliedAgent[]; latest: number }) {
    const { t } = useTranslation();
    if (agents.length === 0) return <span className='text-muted'>{t('rules.notApplied')}</span>;
    return (
        <ul className='flex flex-wrap gap-1.5'>
            {agents.map(a => (
                <li key={a.agent_id}>
                    <Link
                        to={`/agents/${a.agent_id}`}
                        className='inline-flex items-center gap-1 rounded-full border border-line px-2 py-0.5 text-xs text-ink hover:border-accent hover:text-accent focus:outline-none focus-visible:ring-2 focus-visible:ring-accent'
                        title={a.version !== latest ? t('agents.outdatedTitle', { latest }) : undefined}
                    >
                        {a.display_name}
                        <span className='text-muted'>v{a.version}</span>
                        {a.version !== latest && (
                            <span
                                className='material-icons-outlined text-sm text-warning'
                                aria-label={t('agents.outdated')}
                            >
                                update
                            </span>
                        )}
                    </Link>
                </li>
            ))}
        </ul>
    );
}
