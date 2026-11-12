import { useTranslation } from 'react-i18next';
import { Link } from 'react-router-dom';
import { listTemplates } from '../api/client';
import { Badge } from '../components/Badge';
import { Button } from '../components/Button';
import { PageHeader } from '../components/PageHeader';
import { Slot } from '../components/Slot';
import { EmptyState, ErrorAlert, Loading } from '../components/StateViews';
import { Table, TBody, Td, Th, THead, Tr } from '../components/Table';
import { useApi } from '../hooks/useApi';
import { formatDateTime } from '../utils/format';

export function TemplatesPage() {
    const { t } = useTranslation();
    const { data, error, loading, reload } = useApi(listTemplates, []);

    return (
        <>
            <PageHeader
                title={t('templates.title')}
                description={t('templates.description')}
                actions={
                    <>
                        <Button icon='refresh' onClick={reload} busy={loading && !!data}>
                            {t('common.reload')}
                        </Button>
                        <Slot name='templates.headerActions' templates={data ?? []} />
                    </>
                }
            />
            <div className='flex flex-col gap-4'>
                <ErrorAlert error={error} onRetry={reload} />
                {loading && !data ? (
                    <Loading />
                ) : data && data.length === 0 ? (
                    <EmptyState icon='library_books'>{t('templates.empty')}</EmptyState>
                ) : (
                    data && (
                        <Table>
                            <THead>
                                <Th>{t('templates.name')}</Th>
                                <Th>{t('templates.kind')}</Th>
                                <Th>{t('templates.descriptionLabel')}</Th>
                                <Th>{t('templates.updatedAt')}</Th>
                            </THead>
                            <TBody>
                                {data.map(tpl => (
                                    <Tr key={tpl.name}>
                                        <Td>
                                            <Link
                                                to={`/templates/${encodeURIComponent(tpl.name)}`}
                                                className='font-medium text-accent hover:text-accent-hover hover:underline focus:outline-none focus-visible:ring-2 focus-visible:ring-accent'
                                            >
                                                {tpl.name}
                                            </Link>
                                        </Td>
                                        <Td>
                                            {tpl.builtin ? (
                                                <Badge icon='lock'>{t('templates.builtin')}</Badge>
                                            ) : (
                                                <Badge tone='accent'>{t('templates.custom')}</Badge>
                                            )}
                                        </Td>
                                        <Td className='text-muted'>{tpl.description || '-'}</Td>
                                        <Td className='whitespace-nowrap'>{formatDateTime(tpl.updated_at)}</Td>
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
