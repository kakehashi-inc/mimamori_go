import type { ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { Link } from 'react-router-dom';
import { getGuide } from '../api/client';
import { CodeBlock, CopyButton } from '../components/CopyButton';
import { Card, DescriptionList, PageHeader } from '../components/PageHeader';
import { Alert, ErrorAlert, Loading } from '../components/StateViews';
import { useApi } from '../hooks/useApi';
import { agentInitCommand } from '../utils/commands';
import { formatVersion } from '../utils/format';

// Shell commands shown in the guide. They are not translated.
const CMD_AUDITD_CHECK = ['sudo systemctl is-enabled auditd', 'sudo systemctl status auditd', 'sudo auditctl -s'].join(
    '\n'
);
const CMD_AUDITD_ENABLE = 'sudo systemctl enable --now auditd';
const CMD_ACTIONS_CHECK =
    "sudo grep -E '^\\s*(space_left_action|admin_space_left_action|disk_full_action|disk_error_action)\\s*=' /etc/audit/auditd.conf";
const CONF_ACTIONS_EXAMPLE = [
    'space_left_action = SYSLOG',
    'admin_space_left_action = SUSPEND',
    'disk_full_action = ROTATE',
    'disk_error_action = SYSLOG',
].join('\n');
const CMD_AUDITD_RESTART = 'sudo service auditd restart';
const CMD_ROTATE_CHECK = "sudo grep -E '^\\s*(max_log_file|max_log_file_action|num_logs)\\s*=' /etc/audit/auditd.conf";
const CONF_ROTATE_EXAMPLE = ['max_log_file = 8', 'max_log_file_action = ROTATE', 'num_logs = 5'].join('\n');
const CMD_AGENT_START = 'sudo mimamori agent start';
const CMD_NEVER_CHECK = "sudo auditctl -l | grep -- '-a never,task'";
const RULES_FILE = '/etc/audit/rules.d/99-mimamori.rules';

function Step({ n, title, children }: { n: number; title: ReactNode; children: ReactNode }) {
    return (
        <section className='rounded-lg border border-line bg-surface'>
            <div className='flex items-center gap-3 border-b border-line px-4 py-3'>
                <span className='flex h-7 w-7 shrink-0 items-center justify-center rounded-full bg-accent text-sm font-semibold text-accent-contrast'>
                    {n}
                </span>
                <h2 className='text-sm font-semibold text-ink'>{title}</h2>
            </div>
            <div className='flex flex-col gap-3 p-4 text-sm leading-relaxed text-ink'>{children}</div>
        </section>
    );
}

function Bullets({ items }: { items: ReactNode[] }) {
    return (
        <ul className='list-disc space-y-1 pl-5'>
            {items.map((item, i) => (
                <li key={i}>{item}</li>
            ))}
        </ul>
    );
}

export function GuidePage() {
    const { t } = useTranslation();
    const { data: guide, error, loading, reload } = useApi(getGuide, []);

    const urls = guide?.agent_urls?.length ? guide.agent_urls : [`https://${guide?.central_hostname || 'HOST'}:8443`];
    const fp = guide?.ca_fingerprint || '<fingerprint>';
    const url = urls[0];

    return (
        <>
            <PageHeader title={t('guide.title')} description={t('guide.description')} />
            <ErrorAlert error={error} onRetry={reload} />
            {loading && !guide ? (
                <Loading />
            ) : (
                <div className='flex flex-col gap-4'>
                    {guide && (
                        <Card title={t('guide.values')}>
                            <DescriptionList
                                items={[
                                    { label: t('guide.version'), value: formatVersion(guide.version) },
                                    { label: t('guide.centralHostname'), value: guide.central_hostname || '-' },
                                    {
                                        label: t('guide.agentUrls'),
                                        value: (
                                            <ul className='flex flex-col gap-1'>
                                                {urls.map(u => (
                                                    <li key={u}>
                                                        <code className='font-mono text-xs'>{u}</code>
                                                    </li>
                                                ))}
                                            </ul>
                                        ),
                                    },
                                    {
                                        label: t('guide.caFingerprint'),
                                        value: (
                                            <span className='flex flex-wrap items-center gap-2'>
                                                <code className='font-mono text-xs break-all'>
                                                    {guide.ca_fingerprint}
                                                </code>
                                                <CopyButton text={guide.ca_fingerprint} />
                                            </span>
                                        ),
                                    },
                                    {
                                        label: t('guide.defaultValidDays'),
                                        value: t('guide.days', { count: guide.default_valid_days }),
                                    },
                                ]}
                            />
                        </Card>
                    )}

                    <Step n={1} title={t('guide.auditd.title')}>
                        <p>{t('guide.auditd.body')}</p>
                        <CodeBlock code={CMD_AUDITD_CHECK} />
                        <Bullets
                            items={[
                                t('guide.auditd.expectEnabled'),
                                t('guide.auditd.expectRunning'),
                                t('guide.auditd.expectAuditctl'),
                            ]}
                        />
                        <p>{t('guide.auditd.fix')}</p>
                        <CodeBlock code={CMD_AUDITD_ENABLE} />
                    </Step>

                    <Step n={2} title={t('guide.actions.title')}>
                        <p>{t('guide.actions.body')}</p>
                        <CodeBlock code={CMD_ACTIONS_CHECK} />
                        <Alert tone='danger'>{t('guide.actions.warning')}</Alert>
                        <p>{t('guide.actions.change')}</p>
                        <CodeBlock code={CONF_ACTIONS_EXAMPLE} />
                        <p>{t('guide.actions.restart')}</p>
                        <CodeBlock code={CMD_AUDITD_RESTART} />
                        <p className='text-xs text-muted'>{t('guide.actions.restartNote')}</p>
                    </Step>

                    <Step n={3} title={t('guide.rotate.title')}>
                        <p>{t('guide.rotate.body')}</p>
                        <CodeBlock code={CMD_ROTATE_CHECK} />
                        <p>{t('guide.rotate.expect')}</p>
                        <CodeBlock code={CONF_ROTATE_EXAMPLE} />
                        <p className='text-xs text-muted'>{t('guide.rotate.restart')}</p>
                    </Step>

                    <Step n={4} title={t('guide.token.title')}>
                        <p>
                            {t('guide.token.body')}{' '}
                            <Link to='/tokens' className='text-accent hover:text-accent-hover hover:underline'>
                                {t('guide.token.link')}
                            </Link>
                        </p>
                        <p className='text-xs text-muted'>{t('guide.token.note')}</p>
                    </Step>

                    <Step n={5} title={t('guide.init.title')}>
                        <p>{t('guide.init.body')}</p>
                        <CodeBlock code={agentInitCommand('<token>', url, fp)} />
                        <Bullets
                            items={[t('guide.init.stdin'), t('guide.init.fingerprint'), t('guide.init.overwrite')]}
                        />
                        <p>{t('guide.init.start')}</p>
                        <CodeBlock code={CMD_AGENT_START} />
                        {urls.length > 1 && <p className='text-xs text-muted'>{t('guide.init.otherUrls')}</p>}
                    </Step>

                    <Step n={6} title={t('guide.auto.title')}>
                        <p>{t('guide.auto.body')}</p>
                        <Bullets
                            items={[
                                <>
                                    {t('guide.auto.globals')} <code className='font-mono text-xs'>{RULES_FILE}</code>
                                </>,
                                t('guide.auto.osDefaults'),
                                t('guide.auto.never'),
                            ]}
                        />
                        <CodeBlock code={CMD_NEVER_CHECK} />
                        <Alert tone='warning' title={t('guide.auto.restartTitle')}>
                            {t('guide.auto.restartBody')}
                        </Alert>
                    </Step>

                    <Step n={7} title={t('guide.reinit.title')}>
                        <p>{t('guide.reinit.body')}</p>
                        <Bullets
                            items={[
                                t('guide.reinit.caseExpired'),
                                t('guide.reinit.caseReissued'),
                                t('guide.reinit.caseCa'),
                            ]}
                        />
                        <CodeBlock code={agentInitCommand('<token>', url, fp, true)} />
                        <p>{t('guide.reinit.after')}</p>
                        <CodeBlock code={CMD_AGENT_START} />
                    </Step>

                    <Step n={8} title={t('guide.central.title')}>
                        <p>{t('guide.central.body')}</p>
                    </Step>
                </div>
            )}
        </>
    );
}
