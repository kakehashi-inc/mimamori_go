import { useTranslation } from 'react-i18next';
import { Link } from 'react-router-dom';
import { listTokens } from '../api/client';
import { Badge } from '../components/Badge';
import { Button } from '../components/Button';
import { PageHeader } from '../components/PageHeader';
import { Slot } from '../components/Slot';
import { EmptyState, ErrorAlert, Loading } from '../components/StateViews';
import { TokenStatusBadge } from '../components/StatusBadges';
import { Table, TBody, Td, Th, THead, Tr } from '../components/Table';
import { useAuth } from '../context/AuthContext';
import { useApi } from '../hooks/useApi';
import { formatDateTime, formatOperator } from '../utils/format';

export function TokensPage() {
    const { t } = useTranslation();
    const { data, error, loading, reload } = useApi(listTokens, []);
    // Viewers receive no token value (the server leaves the key out).
    const { isAdmin } = useAuth();

    return (
        <>
            <PageHeader
                title={t('tokens.title')}
                description={t('tokens.description')}
                actions={
                    <>
                        <Button icon='refresh' onClick={reload} busy={loading && !!data}>
                            {t('common.reload')}
                        </Button>
                        <Slot name='tokens.headerActions' reload={reload} />
                    </>
                }
            />
            <div className='flex flex-col gap-4'>
                <ErrorAlert error={error} onRetry={reload} />
                {loading && !data ? (
                    <Loading />
                ) : data && data.length === 0 ? (
                    <EmptyState icon='key'>{t('tokens.empty')}</EmptyState>
                ) : (
                    data && (
                        <Table>
                            <THead>
                                <Th>{t('tokens.name')}</Th>
                                {isAdmin && <Th>{t('tokens.value')}</Th>}
                                <Th>{t('tokens.status')}</Th>
                                <Th>{t('tokens.expiresAt')}</Th>
                                <Th>{t('tokens.instanceId')}</Th>
                                <Th>{t('tokens.lastSeen')}</Th>
                                <Th>{t('tokens.created')}</Th>
                                <Slot name='tokens.headEnd' />
                            </THead>
                            <TBody>
                                {data.map(tk => (
                                    <Tr key={tk.id}>
                                        <Td>
                                            <div className='font-medium'>{tk.name}</div>
                                            {tk.agent_id > 0 && (
                                                <Link
                                                    to={`/agents/${tk.agent_id}`}
                                                    className='text-xs text-accent hover:text-accent-hover hover:underline'
                                                >
                                                    {t('tokens.viewAgent')}
                                                </Link>
                                            )}
                                        </Td>
                                        {isAdmin && (
                                            <Td className='font-mono text-xs whitespace-nowrap'>{tk.value || '-'}</Td>
                                        )}
                                        <Td>
                                            <div className='flex flex-col items-start gap-1'>
                                                <TokenStatusBadge status={tk.status} />
                                                {tk.expiring_unconnected && (
                                                    <Badge
                                                        tone='warning'
                                                        icon='warning'
                                                        title={t('tokens.expiringUnconnectedTitle')}
                                                    >
                                                        {t('tokens.expiringUnconnected')}
                                                    </Badge>
                                                )}
                                                {tk.pending_renewal && (
                                                    <Badge
                                                        tone='accent'
                                                        icon='autorenew'
                                                        title={t('tokens.pendingRenewalTitle')}
                                                    >
                                                        {t('tokens.pendingRenewal')}
                                                    </Badge>
                                                )}
                                            </div>
                                        </Td>
                                        <Td className='whitespace-nowrap'>
                                            {formatDateTime(tk.expires_at)}
                                            {tk.renewed_at && (
                                                <div className='text-xs text-muted'>
                                                    {t('tokens.renewedAt', { date: formatDateTime(tk.renewed_at) })}
                                                </div>
                                            )}
                                            {tk.reissued_at && (
                                                <div className='text-xs text-muted'>
                                                    {t('tokens.reissuedAt', { date: formatDateTime(tk.reissued_at) })}
                                                </div>
                                            )}
                                        </Td>
                                        <Td>
                                            {tk.instance_id ? (
                                                <>
                                                    <div
                                                        className='max-w-48 truncate font-mono text-xs'
                                                        title={tk.instance_id}
                                                    >
                                                        {tk.instance_id}
                                                    </div>
                                                    <div className='text-xs whitespace-nowrap text-muted'>
                                                        {t('tokens.boundAt', { date: formatDateTime(tk.bound_at) })}
                                                    </div>
                                                </>
                                            ) : (
                                                <span className='text-muted'>{t('tokens.unbound')}</span>
                                            )}
                                        </Td>
                                        <Td className='whitespace-nowrap'>{formatDateTime(tk.last_seen_at)}</Td>
                                        <Td className='whitespace-nowrap'>
                                            {formatDateTime(tk.created_at)}
                                            <div className='text-xs text-muted'>{formatOperator(tk.created_by)}</div>
                                        </Td>
                                        <Slot name='tokens.rowEnd' token={tk} reload={reload} />
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
