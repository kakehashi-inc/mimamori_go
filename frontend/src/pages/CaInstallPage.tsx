import { useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { caFileURL, listInstallableCAs } from '../api/client';
import { Button, buttonClass } from '../components/Button';
import { LanguageSwitch } from '../components/LanguageSwitch';
import { Card, DescriptionList } from '../components/PageHeader';
import { Alert, ErrorAlert, Loading } from '../components/StateViews';
import { CertStatusBadge } from '../components/StatusBadges';
import { useApi } from '../hooks/useApi';
import type { InstallableCA } from '../types';
import { ALL_GUIDES, detectInstallEnv, type InstallGuide } from '../utils/caInstall';
import { formatDateTime } from '../utils/format';

/** Install steps of one OS and browser combination. */
function GuideSteps({ guide }: { guide: InstallGuide }) {
    const { t } = useTranslation();
    const note = t(`caInstall.guides.${guide}.note`);
    return (
        <section className='rounded-md border border-line p-4'>
            <h3 className='text-sm font-semibold text-ink'>{t(`caInstall.guides.${guide}.title`)}</h3>
            <ol className='mt-2 list-decimal space-y-1.5 pl-5 text-sm text-ink'>
                {t(`caInstall.guides.${guide}.steps`)
                    .split('\n')
                    .map(step => (
                        <li key={step}>{step}</li>
                    ))}
            </ol>
            {note && <p className='mt-3 text-xs text-muted'>{note}</p>}
        </section>
    );
}

/** One CA certificate with its download link. */
function CACard({ ca }: { ca: InstallableCA }) {
    const { t } = useTranslation();
    return (
        <div className='flex flex-col gap-3 rounded-md border border-line p-4 sm:flex-row sm:items-start sm:justify-between'>
            <div className='flex min-w-0 flex-1 flex-col gap-3'>
                <div>
                    <CertStatusBadge status={ca.status} />
                </div>
                <DescriptionList
                    items={[
                        { label: t('caInstall.subject'), value: ca.subject },
                        {
                            label: t('caInstall.fingerprint'),
                            value: <span className='font-mono text-xs break-all'>{ca.fingerprint}</span>,
                        },
                        { label: t('caInstall.notAfter'), value: formatDateTime(ca.not_after) },
                    ]}
                />
            </div>
            <a href={caFileURL(ca)} className={`${buttonClass('primary')} shrink-0`}>
                <span className='material-icons-outlined text-base' aria-hidden='true'>
                    download
                </span>
                {t('caInstall.download')}
            </a>
        </div>
    );
}

/**
 * CA distribution screen (/ca): the CA certificates of the central and the
 * steps to install them on the device. It needs no login. The steps shown
 * follow the OS and the browser detected from the User-Agent; every step is
 * shown when they are not known.
 */
export function CaInstallPage() {
    const { t } = useTranslation();
    const { data, error, loading, reload } = useApi(listInstallableCAs, []);
    const env = useMemo(() => detectInstallEnv(navigator.userAgent), []);
    const [showAll, setShowAll] = useState(false);
    const known = env.guides.length > 0;
    const shown = showAll || !known ? ALL_GUIDES : env.guides;
    const cas = data ?? [];

    return (
        <div className='flex min-h-screen flex-col'>
            <div className='flex justify-end p-3'>
                <LanguageSwitch />
            </div>
            <main className='mx-auto flex w-full max-w-3xl flex-1 flex-col gap-4 px-4 pb-16'>
                <div className='text-center'>
                    <span className='material-icons-outlined text-4xl text-accent' aria-hidden='true'>
                        verified_user
                    </span>
                    <h1 className='mt-1 text-2xl font-bold tracking-tight text-ink'>{t('caInstall.title')}</h1>
                    <p className='mx-auto mt-2 max-w-xl text-sm text-muted'>{t('caInstall.intro')}</p>
                </div>
                <Alert tone='info'>
                    <p>{t('caInstall.warningNote')}</p>
                    <p className='mt-1'>{t('caInstall.verifyNote')}</p>
                </Alert>

                <Card title={t('caInstall.certificates')}>
                    <ErrorAlert error={error} onRetry={reload} />
                    {loading && !data ? (
                        <Loading />
                    ) : (
                        <div className='flex flex-col gap-3'>
                            {cas.length > 1 && <Alert tone='warning'>{t('caInstall.installBoth')}</Alert>}
                            {cas.map(ca => (
                                <CACard key={ca.fingerprint} ca={ca} />
                            ))}
                            {cas.length > 0 && <p className='text-xs text-muted'>{t('caInstall.nameNote')}</p>}
                        </div>
                    )}
                </Card>

                {env.iosNotSafari && <Alert tone='warning'>{t('caInstall.openInSafari')}</Alert>}

                <Card title={t('caInstall.steps')}>
                    <div className='flex flex-col gap-3'>
                        <p className='text-sm text-ink'>
                            {known
                                ? t('caInstall.detected', {
                                      env: env.guides.map(g => t(`caInstall.guides.${g}.title`)).join(' / '),
                                  })
                                : t('caInstall.undetected')}
                        </p>
                        {env.guides.length > 1 && <p className='text-sm text-muted'>{t('caInstall.macOrIpad')}</p>}
                        {shown.map(g => (
                            <GuideSteps key={g} guide={g} />
                        ))}
                        {known && (
                            <div>
                                <Button
                                    size='sm'
                                    icon={showAll ? 'unfold_less' : 'unfold_more'}
                                    onClick={() => setShowAll(v => !v)}
                                >
                                    {showAll ? t('caInstall.showDetected') : t('caInstall.showAll')}
                                </Button>
                            </div>
                        )}
                        <p className='text-xs text-muted'>{t('caInstall.menusVary')}</p>
                    </div>
                </Card>
            </main>
        </div>
    );
}
