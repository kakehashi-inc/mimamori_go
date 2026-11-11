import { useTranslation } from 'react-i18next';
import { Link } from 'react-router-dom';
import { listRules } from '../api/client';
import { AppliedAgentsList } from '../components/AppliedAgentsList';
import { Badge } from '../components/Badge';
import { Button, buttonClass } from '../components/Button';
import { PageHeader } from '../components/PageHeader';
import { EmptyState, ErrorAlert, Loading } from '../components/StateViews';
import { Table, TBody, Td, Th, THead, Tr } from '../components/Table';
import { useAuth } from '../context/AuthContext';
import { useApi } from '../hooks/useApi';
import { formatDateTime } from '../utils/format';

export function RulesPage() {
    const { t } = useTranslation();
    const { isAdmin } = useAuth();
    const { data, error, loading, reload } = useApi(listRules, []);

    return (
        <>
            <PageHeader
                title={t('rules.title')}
                description={t('rules.description')}
                actions={
                    <>
                        <Button icon='refresh' onClick={reload} busy={loading && !!data}>
                            {t('common.reload')}
                        </Button>
                        {isAdmin && (
                            <Link to='/rules/new' className={buttonClass('primary')}>
                                <span className='material-icons-outlined text-base' aria-hidden='true'>
                                    add
                                </span>
                                {t('rules.create')}
                            </Link>
                        )}
                    </>
                }
            />
            <div className='flex flex-col gap-4'>
                <ErrorAlert error={error} onRetry={reload} />
                {loading && !data ? (
                    <Loading />
                ) : data && data.length === 0 ? (
                    <EmptyState icon='rule'>{t('rules.empty')}</EmptyState>
                ) : (
                    data && (
                        <Table>
                            <THead>
                                <Th>{t('rules.name')}</Th>
                                <Th>{t('rules.descriptionLabel')}</Th>
                                <Th>{t('rules.latestVersion')}</Th>
                                <Th>{t('rules.appliedAgents')}</Th>
                                <Th>{t('rules.updatedAt')}</Th>
                            </THead>
                            <TBody>
                                {data.map(r => {
                                    const applied = r.applied_agents ?? [];
                                    const outdated = applied.filter(a => a.version !== r.latest_version).length;
                                    return (
                                        <Tr key={r.id}>
                                            <Td>
                                                <Link
                                                    to={`/rules/${r.id}`}
                                                    className='font-medium text-accent hover:text-accent-hover hover:underline focus:outline-none focus-visible:ring-2 focus-visible:ring-accent'
                                                >
                                                    {r.name}
                                                </Link>
                                            </Td>
                                            <Td className='max-w-80 text-muted'>{r.description || '-'}</Td>
                                            <Td>
                                                <span className='flex items-center gap-1'>
                                                    v{r.latest_version}
                                                    {outdated > 0 && (
                                                        <Badge tone='warning' icon='update'>
                                                            {t('rules.outdatedCount', { count: outdated })}
                                                        </Badge>
                                                    )}
                                                </span>
                                            </Td>
                                            <Td>
                                                <AppliedAgentsList agents={applied} latest={r.latest_version} />
                                            </Td>
                                            <Td className='whitespace-nowrap'>{formatDateTime(r.updated_at)}</Td>
                                        </Tr>
                                    );
                                })}
                            </TBody>
                        </Table>
                    )
                )}
            </div>
        </>
    );
}
