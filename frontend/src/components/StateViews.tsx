import type { ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { errorMessage } from '../utils/format';

/** Spinner with a label, shown while the first response is pending. */
export function Loading({ label }: { label?: string }) {
    const { t } = useTranslation();
    return (
        <div className='flex items-center justify-center gap-2 py-10 text-sm text-muted' role='status'>
            <span className='material-icons-outlined animate-spin text-xl' aria-hidden='true'>
                autorenew
            </span>
            {label ?? t('common.loading')}
        </div>
    );
}

/** Placeholder for a list without items. */
export function EmptyState({ icon = 'inbox', children }: { icon?: string; children?: ReactNode }) {
    const { t } = useTranslation();
    return (
        <div className='flex flex-col items-center justify-center gap-2 py-10 text-center text-sm text-muted'>
            <span className='material-icons-outlined text-3xl' aria-hidden='true'>
                {icon}
            </span>
            {children ?? t('common.empty')}
        </div>
    );
}

type AlertTone = 'info' | 'warning' | 'danger' | 'success';

const alertTones: Record<AlertTone, { box: string; icon: string }> = {
    info: { box: 'border-accent/30 bg-accent-soft text-ink', icon: 'info' },
    warning: { box: 'border-warning/40 bg-warning-soft text-ink', icon: 'warning' },
    danger: { box: 'border-danger/40 bg-danger-soft text-ink', icon: 'error' },
    success: { box: 'border-success/40 bg-success-soft text-ink', icon: 'check_circle' },
};

const iconColor: Record<AlertTone, string> = {
    info: 'text-accent',
    warning: 'text-warning',
    danger: 'text-danger',
    success: 'text-success',
};

/** An inline message box. */
export function Alert({
    tone = 'info',
    title,
    children,
    action,
}: {
    tone?: AlertTone;
    title?: ReactNode;
    children?: ReactNode;
    action?: ReactNode;
}) {
    return (
        <div
            className={`flex items-start gap-3 rounded-lg border px-4 py-3 text-sm ${alertTones[tone].box}`}
            role='alert'
        >
            <span className={`material-icons-outlined text-xl ${iconColor[tone]}`} aria-hidden='true'>
                {alertTones[tone].icon}
            </span>
            <div className='min-w-0 flex-1'>
                {title && <p className='font-medium'>{title}</p>}
                {children && <div className={`break-words ${title ? 'mt-1 text-muted' : ''}`}>{children}</div>}
            </div>
            {action}
        </div>
    );
}

/** Error box for a failed request, with an optional retry button. */
export function ErrorAlert({ error, onRetry }: { error: unknown; onRetry?: () => void }) {
    const { t } = useTranslation();
    if (!error) return null;
    return (
        <Alert
            tone='danger'
            action={
                onRetry && (
                    <button
                        type='button'
                        onClick={onRetry}
                        className='rounded text-sm font-medium text-accent hover:text-accent-hover focus:outline-none focus-visible:ring-2 focus-visible:ring-accent'
                    >
                        {t('common.retry')}
                    </button>
                )
            }
        >
            {errorMessage(t, error)}
        </Alert>
    );
}

/**
 * Whole-page error for a session check that failed for another reason than
 * "not signed in" (busy server, network failure), with a retry button.
 */
export function SessionCheckError({ error, onRetry }: { error: unknown; onRetry: () => void }) {
    return (
        <div className='mx-auto flex min-h-screen w-full max-w-xl flex-col justify-center px-4 py-10'>
            <ErrorAlert error={error} onRetry={onRetry} />
        </div>
    );
}
