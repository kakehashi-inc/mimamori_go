import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Link, useParams } from 'react-router-dom';
import { getTemplate } from '../api/client';
import { Badge } from '../components/Badge';
import { PageHeader } from '../components/PageHeader';
import { RuleContentView } from '../components/RuleContentView';
import { Slot } from '../components/Slot';
import { ErrorAlert, Loading } from '../components/StateViews';
import { useApi } from '../hooks/useApi';
import { formatDateTime } from '../utils/format';

export function TemplateDetailPage() {
    const { t } = useTranslation();
    const { name = '' } = useParams();
    const { data: tpl, error, loading, reload, setData } = useApi(() => getTemplate(name), [name]);
    const [panelOpen, setPanelOpen] = useState(false);

    return (
        <>
            <PageHeader
                breadcrumb={
                    <Link to='/templates' className='text-accent hover:text-accent-hover hover:underline'>
                        {t('templates.title')}
                    </Link>
                }
                title={
                    <span className='flex flex-wrap items-center gap-2'>
                        {tpl?.name ?? name}
                        {tpl?.builtin && <Badge icon='lock'>{t('templates.builtin')}</Badge>}
                    </span>
                }
                description={
                    !panelOpen && tpl?.description ? (
                        <span className='wrap-anywhere'>{tpl.description}</span>
                    ) : undefined
                }
                actions={
                    tpl && (
                        <Slot
                            name='templateDetail.headerActions'
                            template={tpl}
                            panelOpen={panelOpen}
                            setPanelOpen={setPanelOpen}
                        />
                    )
                }
            />
            <ErrorAlert error={error} onRetry={reload} />
            {loading && !tpl ? (
                <Loading />
            ) : (
                tpl && (
                    <div className='flex flex-col gap-4'>
                        <Slot name='templateDetail.notices' template={tpl} />
                        {!panelOpen && (
                            <>
                                <p className='text-xs text-muted'>
                                    {t('templates.updatedAt')}: {formatDateTime(tpl.updated_at)}
                                </p>
                                <RuleContentView content={tpl.content} />
                            </>
                        )}
                        {panelOpen && (
                            <Slot
                                name='templateDetail.panel'
                                template={tpl}
                                onClose={updated => {
                                    if (updated) setData(updated);
                                    setPanelOpen(false);
                                }}
                            />
                        )}
                    </div>
                )
            )}
        </>
    );
}
