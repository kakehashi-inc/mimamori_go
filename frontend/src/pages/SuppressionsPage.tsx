import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { deleteSuppression, listSuppressions } from '../api/client';
import { Button } from '../components/Button';
import { ConfirmDialog } from '../components/ConfirmDialog';
import { PageHeader } from '../components/PageHeader';
import { EmptyState, ErrorAlert, Loading } from '../components/StateViews';
import { SuppressionFormModal } from '../components/SuppressionFormModal';
import { Table, TBody, Td, Th, THead, Tr } from '../components/Table';
import { useAuth } from '../context/AuthContext';
import { useToast } from '../context/ToastContext';
import { useApi } from '../hooks/useApi';
import type { Suppression } from '../types';
import { formatDateTime } from '../utils/format';

export function SuppressionsPage() {
    const { t } = useTranslation();
    const { isAdmin } = useAuth();
    const { showToast } = useToast();
    const { data, error, loading, reload } = useApi(listSuppressions, []);
    const [createOpen, setCreateOpen] = useState(false);
    const [target, setTarget] = useState<Suppression | null>(null);

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
                        {isAdmin && (
                            <Button variant='primary' icon='add' onClick={() => setCreateOpen(true)}>
                                {t('suppressions.create')}
                            </Button>
                        )}
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
                                {isAdmin && <Th />}
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
                                            <div className='text-xs text-muted'>{s.created_by}</div>
                                        </Td>
                                        {isAdmin && (
                                            <Td className='text-right'>
                                                <Button
                                                    size='sm'
                                                    variant='danger'
                                                    icon='delete'
                                                    onClick={() => setTarget(s)}
                                                >
                                                    {t('common.delete')}
                                                </Button>
                                            </Td>
                                        )}
                                    </Tr>
                                ))}
                            </TBody>
                        </Table>
                    )
                )}
            </div>
            {isAdmin && (
                <>
                    <SuppressionFormModal
                        open={createOpen}
                        onClose={() => setCreateOpen(false)}
                        onCreated={() => {
                            showToast(t('suppressions.created'));
                            reload();
                        }}
                    />
                    <ConfirmDialog
                        open={target !== null}
                        title={t('suppressions.deleteTitle')}
                        message={t('suppressions.deleteConfirm')}
                        confirmLabel={t('common.delete')}
                        danger
                        onClose={() => setTarget(null)}
                        onConfirm={async () => {
                            if (!target) return;
                            await deleteSuppression(target.id);
                            showToast(t('common.deleted'));
                            reload();
                        }}
                    />
                </>
            )}
        </>
    );
}
