import { useTranslation } from 'react-i18next';
import { Link } from 'react-router-dom';
import { getCertificates } from '../api/client';
import { Badge } from '../components/Badge';
import { Button } from '../components/Button';
import { Card, DescriptionList, PageHeader } from '../components/PageHeader';
import { Alert, EmptyState, ErrorAlert, Loading } from '../components/StateViews';
import { CertStatusBadge } from '../components/StatusBadges';
import { Table, TBody, Td, Th, THead, Tr } from '../components/Table';
import { useApi } from '../hooks/useApi';
import { daysUntil, formatDateTime } from '../utils/format';

// Certificates expiring within this many days are highlighted.
const EXPIRY_WARN_DAYS = 30;

function Expiry({ value }: { value: string }) {
    const { t } = useTranslation();
    const days = daysUntil(value);
    return (
        <span className='flex flex-wrap items-center gap-2 whitespace-nowrap'>
            {formatDateTime(value)}
            {days !== null && days < 0 && <Badge tone='danger'>{t('certificates.expired')}</Badge>}
            {days !== null && days >= 0 && days <= EXPIRY_WARN_DAYS && (
                <Badge tone='warning'>{t('certificates.daysLeft', { count: days })}</Badge>
            )}
        </span>
    );
}

export function CertificatesPage() {
    const { t } = useTranslation();
    const { data, error, loading, reload } = useApi(getCertificates, []);
    const missing = data?.agents_missing_new_ca ?? [];

    return (
        <>
            <PageHeader
                title={t('certificates.title')}
                description={t('certificates.description')}
                actions={
                    <Button icon='refresh' onClick={reload} busy={loading && !!data}>
                        {t('common.reload')}
                    </Button>
                }
            />
            <ErrorAlert error={error} onRetry={reload} />
            {loading && !data ? (
                <Loading />
            ) : (
                data && (
                    <div className='flex flex-col gap-4'>
                        <Card title={t('certificates.rotation')}>
                            <DescriptionList
                                items={[
                                    {
                                        label: t('certificates.rotationState'),
                                        value: (
                                            <Badge tone={data.rotation.state === 'none' ? 'neutral' : 'accent'}>
                                                {t(`rotationState.${data.rotation.state}`)}
                                            </Badge>
                                        ),
                                    },
                                    {
                                        label: t('certificates.nextCaCreatedAt'),
                                        value: formatDateTime(data.rotation.next_ca_created_at),
                                    },
                                    {
                                        label: t('certificates.switchAfter'),
                                        value: formatDateTime(data.rotation.switch_after),
                                    },
                                ]}
                            />
                            {missing.length > 0 && (
                                <div className='mt-4'>
                                    <Alert
                                        tone='warning'
                                        title={t('certificates.missingTitle', { count: missing.length })}
                                    >
                                        <p>{t('certificates.missingNote')}</p>
                                        <ul className='mt-2 flex flex-wrap gap-1.5'>
                                            {missing.map(a => (
                                                <li key={a.agent_id}>
                                                    <Link
                                                        to={`/agents/${a.agent_id}`}
                                                        className='rounded-full border border-line bg-surface px-2 py-0.5 text-xs text-ink hover:border-accent hover:text-accent'
                                                    >
                                                        {a.display_name}
                                                    </Link>
                                                </li>
                                            ))}
                                        </ul>
                                    </Alert>
                                </div>
                            )}
                        </Card>

                        <Card title={t('certificates.cas')}>
                            {(data.cas ?? []).length === 0 ? (
                                <EmptyState icon='verified_user' />
                            ) : (
                                <Table>
                                    <THead>
                                        <Th>{t('certificates.status')}</Th>
                                        <Th>{t('certificates.subject')}</Th>
                                        <Th>{t('certificates.fingerprint')}</Th>
                                        <Th>{t('certificates.notBefore')}</Th>
                                        <Th>{t('certificates.notAfter')}</Th>
                                    </THead>
                                    <TBody>
                                        {(data.cas ?? []).map(ca => (
                                            <Tr key={ca.fingerprint}>
                                                <Td>
                                                    <CertStatusBadge status={ca.status} />
                                                </Td>
                                                <Td>{ca.subject}</Td>
                                                <Td className='max-w-96 font-mono text-xs break-all'>
                                                    {ca.fingerprint}
                                                </Td>
                                                <Td className='whitespace-nowrap'>{formatDateTime(ca.not_before)}</Td>
                                                <Td>
                                                    <Expiry value={ca.not_after} />
                                                </Td>
                                            </Tr>
                                        ))}
                                    </TBody>
                                </Table>
                            )}
                        </Card>

                        <Card title={t('certificates.server')}>
                            {!data.server ? (
                                <EmptyState icon='verified_user' />
                            ) : (
                                <DescriptionList
                                    items={[
                                        { label: t('certificates.subject'), value: data.server.subject },
                                        {
                                            label: t('certificates.names'),
                                            value: (
                                                <span className='flex flex-wrap gap-1'>
                                                    {(data.server.names ?? []).map(n => (
                                                        <code
                                                            key={n}
                                                            className='rounded bg-canvas px-1 font-mono text-xs'
                                                        >
                                                            {n}
                                                        </code>
                                                    ))}
                                                </span>
                                            ),
                                        },
                                        {
                                            label: t('certificates.fingerprint'),
                                            value: (
                                                <span className='font-mono text-xs break-all'>
                                                    {data.server.fingerprint}
                                                </span>
                                            ),
                                        },
                                        {
                                            label: t('certificates.issuer'),
                                            value: (
                                                <span className='font-mono text-xs break-all'>
                                                    {data.server.issuer_fingerprint}
                                                </span>
                                            ),
                                        },
                                        {
                                            label: t('certificates.notBefore'),
                                            value: formatDateTime(data.server.not_before),
                                        },
                                        {
                                            label: t('certificates.notAfter'),
                                            value: <Expiry value={data.server.not_after} />,
                                        },
                                    ]}
                                />
                            )}
                        </Card>
                    </div>
                )
            )}
        </>
    );
}
