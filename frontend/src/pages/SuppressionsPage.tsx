import { useTranslation } from 'react-i18next';
import { listSuppressions } from '../api/client';
import { Button } from '../components/Button';
import { PageHeader } from '../components/PageHeader';
import { Slot } from '../components/Slot';
import { EmptyState, ErrorAlert, Loading } from '../components/StateViews';
import { Table, TBody, Td, Th, THead, Tr } from '../components/Table';
import { useApi } from '../hooks/useApi';
import { formatDateTime, formatOperator } from '../utils/format';

export function SuppressionsPage() {
    const { t } = useTranslation();
    const { data, error, loading, reload } = useApi(listSuppressions, []);

    const any = <span className='text-muted'>{t('suppressions.any')}</span>;

    return (
        <>
            <PageHeader
                title={t('suppressions.title')}
                description={t('suppressions.description')}
                actions={
                    <>
                        <Button icon='refresh' onClick={reload} busy={loading && !!data}>
                            {t('common.reload')}
                        </Button>
                        <Slot name='suppressions.headerActions' reload={reload} />
                    </>
                }
            />
            <div className='flex flex-col gap-4'>
                <ErrorAlert error={error} onRetry={reload} />
                {loading && !data ? (
                    <Loading />
                ) : data && data.length === 0 ? (
                    <EmptyState icon='block'>{t('suppressions.empty')}</EmptyState>
                ) : (
                    data && (
                        <Table>
                            <THead>
                                <Th>{t('suppressions.agent')}</Th>
                                <Th>{t('detections.type')}</Th>
                                <Th>{t('detections.osUser')}</Th>
                                <Th>{t('detections.exe')}</Th>
                                <Th>{t('detections.target')}</Th>
                                <Th>{t('suppressions.comment')}</Th>
                                <Th>{t('suppressions.createdAt')}</Th>
                                <Slot name='suppressions.headEnd' />
                            </THead>
                            <TBody>
                                {data.map(s => (
                                    <Tr key={s.id}>
                                        <Td>{s.agent_id === null ? t('suppressions.allAgents') : s.agent_name}</Td>
                                        <Td className='whitespace-nowrap'>
                                            {s.type ? t(`detectionType.${s.type}`) : any}
                                        </Td>
                                        <Td className='font-mono text-xs'>{s.os_user || any}</Td>
                                        <Td className='max-w-64 font-mono text-xs break-all'>{s.exe || any}</Td>
                                        <Td className='max-w-80 font-mono text-xs break-all'>{s.target || any}</Td>
                                        <Td className='text-muted'>{s.comment || '-'}</Td>
                                        <Td className='whitespace-nowrap'>
                                            {formatDateTime(s.created_at)}
                                            <div className='text-xs text-muted'>{formatOperator(s.created_by)}</div>
                                        </Td>
                                        <Slot name='suppressions.rowEnd' suppression={s} reload={reload} />
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
