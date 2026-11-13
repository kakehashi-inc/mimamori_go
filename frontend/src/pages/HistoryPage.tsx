import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { listHistory } from '../api/client';
import { Badge } from '../components/Badge';
import { Button } from '../components/Button';
import { inputSmAutoClass } from '../components/FormControls';
import { PageHeader } from '../components/PageHeader';
import { Pagination } from '../components/Pagination';
import { Slot } from '../components/Slot';
import { EmptyState, ErrorAlert, Loading } from '../components/StateViews';
import { Table, TBody, Td, Th, THead, Tr } from '../components/Table';
import { useApi } from '../hooks/useApi';
import type { HistoryCategory } from '../types';
import { formatDateTime, formatOperator } from '../utils/format';
import { historyActionText, historyDetailText, historyTargetText } from '../utils/historyText';

// Categories every account can read; extensions add their own options.
const CATEGORIES: HistoryCategory[] = [
    'rule',
    'token',
    'certificate',
    'rejected',
    'template',
    'suppression',
    'detection',
];

// Rows per page.
const LIMIT = 100;

export function HistoryPage() {
    const { t } = useTranslation();
    const [category, setCategory] = useState('');
    const [offset, setOffset] = useState(0);
    const { data, error, loading, reload } = useApi(
        () => listHistory({ category, limit: LIMIT, offset }),
        [category, offset]
    );
    const items = data?.items ?? [];

    return (
        <>
            <PageHeader
                title={t('history.title')}
                description={t('history.description')}
                actions={
                    <Button icon='refresh' onClick={reload} busy={loading && !!data}>
                        {t('common.reload')}
                    </Button>
                }
            />
            <div className='flex flex-col gap-3'>
                <div className='flex items-center gap-2'>
                    <label htmlFor='h-category' className='text-sm text-muted'>
                        {t('history.category')}
                    </label>
                    <select
                        id='h-category'
                        className={`${inputSmAutoClass} shrink-0`}
                        value={category}
                        onChange={e => {
                            setCategory(e.target.value);
                            setOffset(0);
                        }}
                    >
                        <option value=''>{t('common.all')}</option>
                        <Slot name='history.categoryOptionsStart' />
                        {CATEGORIES.map(c => (
                            <option key={c} value={c}>
                                {t(`historyCategory.${c}`)}
                            </option>
                        ))}
                        <Slot name='history.categoryOptionsEnd' />
                    </select>
                </div>
                <ErrorAlert error={error} onRetry={reload} />
                {loading && !data ? (
                    <Loading />
                ) : data && items.length === 0 ? (
                    <EmptyState icon='history'>{t('history.empty')}</EmptyState>
                ) : (
                    data && (
                        <Table>
                            <THead>
                                <Th>{t('history.at')}</Th>
                                <Th>{t('history.account')}</Th>
                                <Th>{t('history.category')}</Th>
                                <Th>{t('history.action')}</Th>
                                <Th>{t('history.target')}</Th>
                                <Th>{t('history.detail')}</Th>
                                <Th>{t('history.remoteAddr')}</Th>
                                <Th className='text-right'>{t('history.count')}</Th>
                            </THead>
                            <TBody>
                                {items.map(h => (
                                    <Tr key={h.id}>
                                        <Td className='whitespace-nowrap'>{formatDateTime(h.at)}</Td>
                                        <Td>{formatOperator(h.account_name)}</Td>
                                        <Td>
                                            <Badge tone={h.category === 'rejected' ? 'danger' : 'neutral'}>
                                                {t(`historyCategory.${h.category}`)}
                                            </Badge>
                                        </Td>
                                        <Td>{historyActionText(t, h)}</Td>
                                        <Td className='max-w-64 break-all'>
                                            {h.target ? historyTargetText(t, h) : '-'}
                                        </Td>
                                        <Td className='max-w-96 break-words text-muted'>
                                            {h.detail ? historyDetailText(t, h) : '-'}
                                        </Td>
                                        <Td className='font-mono text-xs'>{h.remote_addr || '-'}</Td>
                                        <Td className='text-right'>{h.count}</Td>
                                    </Tr>
                                ))}
                            </TBody>
                        </Table>
                    )
                )}
                {data && data.total > 0 && (
                    <Pagination total={data.total} limit={LIMIT} offset={offset} onChange={setOffset} />
                )}
            </div>
        </>
    );
}
