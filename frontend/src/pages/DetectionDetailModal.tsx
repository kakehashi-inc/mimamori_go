import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Link } from 'react-router-dom';
import { getDetection } from '../api/client';
import { Badge } from '../components/Badge';
import { Button } from '../components/Button';
import { CopyButton } from '../components/CopyButton';
import { Modal } from '../components/Modal';
import { DescriptionList } from '../components/PageHeader';
import { ErrorAlert } from '../components/StateViews';
import { CategoryBadge } from '../components/StatusBadges';
import type { Detection } from '../types';
import { formatDateTime, formatOperator } from '../utils/format';

const linkClass =
    'text-accent hover:text-accent-hover hover:underline focus:outline-none focus-visible:ring-2 focus-visible:ring-accent';

/** A value shown in full (wrapped, scrollable when very long) with a copy button. */
function LongValue({ value }: { value: string }) {
    if (!value) return <span className='text-muted'>-</span>;
    return (
        <div className='flex flex-col items-start gap-1'>
            <pre className='max-h-60 w-full overflow-auto rounded-md border border-line bg-canvas px-2 py-1 font-mono text-xs leading-relaxed break-all whitespace-pre-wrap text-ink'>
                {value}
            </pre>
            <CopyButton text={value} />
        </div>
    );
}

/**
 * Every field of a detection (GET /detections/{agent}/{id}), opened from a
 * row of the detection list. The row's values are shown until the detection
 * is loaded again from the server.
 */
export function DetectionDetailModal({ detection, onClose }: { detection: Detection | null; onClose: () => void }) {
    const { t } = useTranslation();
    const [loaded, setLoaded] = useState<Detection | null>(null);
    const [error, setError] = useState<unknown>(null);

    useEffect(() => {
        setLoaded(null);
        setError(null);
        if (!detection) return;
        let active = true;
        getDetection(detection.agent_id, detection.id)
            .then(d => {
                if (active) setLoaded(d);
            })
            .catch(e => {
                if (active) setError(e);
            });
        return () => {
            active = false;
        };
    }, [detection]);

    const d = loaded ?? detection;
    return (
        <Modal
            open={detection !== null}
            title={t('detections.detail.title')}
            onClose={onClose}
            size='xl'
            footer={
                <Button variant='primary' onClick={onClose}>
                    {t('common.close')}
                </Button>
            }
        >
            {d && (
                <div className='flex flex-col gap-4'>
                    {error ? <ErrorAlert error={error} /> : null}
                    <DescriptionList
                        items={[
                            {
                                label: t('detections.detail.id'),
                                value: <code className='font-mono text-xs'>{`${d.agent_id}:${d.id}`}</code>,
                            },
                            { label: t('detections.lastAt'), value: formatDateTime(d.last_at) },
                            { label: t('detections.firstAt'), value: formatDateTime(d.first_at) },
                            { label: t('detections.count'), value: d.count },
                            {
                                label: t('detections.agent'),
                                value: (
                                    <span className='flex flex-col'>
                                        <Link to={`/agents/${d.agent_id}`} className={linkClass} onClick={onClose}>
                                            {d.agent_name}
                                        </Link>
                                        <span className='font-mono text-xs break-all text-muted'>{d.hostname}</span>
                                    </span>
                                ),
                            },
                            { label: t('detections.category'), value: <CategoryBadge category={d.category} /> },
                            { label: t('detections.type'), value: t(`detectionType.${d.type}`) },
                            {
                                label: t('detections.osUser'),
                                value: d.os_user ? (
                                    <code className='font-mono text-xs break-all'>{d.os_user}</code>
                                ) : (
                                    '-'
                                ),
                            },
                            { label: t('detections.exe'), value: <LongValue value={d.exe} /> },
                            { label: t('detections.target'), value: <LongValue value={d.target} /> },
                            {
                                label: t('detections.file'),
                                value: d.file ? (
                                    <DescriptionList
                                        items={[
                                            {
                                                label: t('detections.detail.fileMode'),
                                                value: <code className='font-mono text-xs'>{d.file.mode || '-'}</code>,
                                            },
                                            {
                                                label: t('detections.detail.fileOwner'),
                                                value: (
                                                    <code className='font-mono text-xs break-all'>
                                                        {d.file.owner || '-'}
                                                    </code>
                                                ),
                                            },
                                            {
                                                label: t('detections.detail.fileCtime'),
                                                value: formatDateTime(d.file.ctime),
                                            },
                                        ]}
                                    />
                                ) : (
                                    '-'
                                ),
                            },
                            {
                                label: t('detections.ackState'),
                                value: d.acknowledged_at ? (
                                    <span className='flex flex-wrap items-center gap-2'>
                                        <Badge tone='success' icon='check'>
                                            {t('detections.ackYes')}
                                        </Badge>
                                        <span>{formatDateTime(d.acknowledged_at)}</span>
                                        <span className='break-all text-muted'>
                                            {formatOperator(d.acknowledged_by)}
                                        </span>
                                    </span>
                                ) : (
                                    <Badge tone='neutral'>{t('detections.ackNo')}</Badge>
                                ),
                            },
                            {
                                label: t('detections.suppressed'),
                                value: d.suppressed ? (
                                    <span className='flex flex-wrap items-center gap-2'>
                                        <Badge tone='neutral' icon='block'>
                                            {t('detections.suppressedYes')}
                                        </Badge>
                                        {d.suppression_id !== null && (
                                            <Link to='/suppressions' className={linkClass} onClick={onClose}>
                                                {t('detections.detail.suppressionId', { id: d.suppression_id })}
                                            </Link>
                                        )}
                                    </span>
                                ) : (
                                    t('detections.suppressedNo')
                                ),
                            },
                            {
                                label: t('detections.detail.notifyState'),
                                value: t(`detections.detail.notify.${d.notify_state}`, {
                                    defaultValue: d.notify_state,
                                }),
                            },
                        ]}
                    />
                </div>
            )}
        </Modal>
    );
}
