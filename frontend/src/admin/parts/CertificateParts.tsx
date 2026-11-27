import { useTranslation } from 'react-i18next';
import { buttonClass } from '../../components/Button';
import { CopyButton } from '../../components/CopyButton';
import { Card } from '../../components/PageHeader';

// Parts placed in the certificates screen (slot certificates.sections).

/** URL of the CA distribution page, to copy and tell the users of the screens. */
export function CaPageSection() {
    const { t } = useTranslation();
    const url = `${window.location.origin}/ca`;
    return (
        <Card title={t('certificates.caPage')}>
            <p className='text-sm text-muted'>{t('certificates.caPageNote')}</p>
            <div className='mt-3 flex flex-wrap items-center gap-2'>
                <code className='rounded bg-canvas px-2 py-1 font-mono text-sm break-all text-ink'>{url}</code>
                <CopyButton text={url} />
                <a href='/ca' target='_blank' rel='noopener noreferrer' className={buttonClass('secondary', 'sm')}>
                    <span className='material-icons-outlined text-sm' aria-hidden='true'>
                        open_in_new
                    </span>
                    {t('certificates.open')}
                </a>
            </div>
        </Card>
    );
}
