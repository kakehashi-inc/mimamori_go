import { useTranslation } from 'react-i18next';
import { Badge } from '../../components/Badge';
import { checkboxClass } from '../../components/FormControls';
import { ErrorAlert, Loading } from '../../components/StateViews';
import { RoleBadge } from '../../components/StatusBadges';
import type { Account } from '../../types';

/**
 * The accounts that receive the notification mails, chosen one by one with a
 * check box each. `value` holds the checked state of the accounts whose box
 * was changed and not saved yet.
 */
export function NotifyRecipients({
    accounts,
    error,
    onRetry,
    value,
    onChange,
}: {
    accounts: Account[] | null;
    error: unknown;
    onRetry: () => void;
    value: Record<number, boolean>;
    onChange: (v: Record<number, boolean>) => void;
}) {
    const { t } = useTranslation();
    const checked = (a: Account) => value[a.id] ?? a.notify;

    function toggle(a: Account, on: boolean) {
        const next = { ...value };
        if (on === a.notify) delete next[a.id];
        else next[a.id] = on;
        onChange(next);
    }

    const count = (accounts ?? []).filter(a => checked(a) && !a.disabled).length;

    return (
        <div className='grid grid-cols-1 gap-2 border-b border-line pb-4'>
            <div>
                <p className='text-sm font-medium text-ink'>{t('config.recipients.title')}</p>
                <p className='text-xs text-muted'>{t('config.recipients.hint')}</p>
            </div>
            <ErrorAlert error={error} onRetry={onRetry} />
            {!accounts ? (
                !error && <Loading />
            ) : (
                <>
                    <ul className='flex flex-col divide-y divide-line rounded-lg border border-line'>
                        {accounts.map(a => (
                            <li key={a.id}>
                                <label className='flex cursor-pointer flex-wrap items-center gap-x-3 gap-y-1 px-3 py-2 text-sm hover:bg-canvas'>
                                    <input
                                        type='checkbox'
                                        className={checkboxClass}
                                        checked={checked(a)}
                                        onChange={e => toggle(a, e.target.checked)}
                                    />
                                    <span className='font-medium text-ink'>{a.display_name}</span>
                                    <span className='min-w-0 break-all text-muted'>{a.email}</span>
                                    <RoleBadge role={a.role} />
                                    {a.disabled && (
                                        <Badge tone='danger' title={t('config.recipients.disabledTitle')}>
                                            {t('accounts.disabled')}
                                        </Badge>
                                    )}
                                    {value[a.id] !== undefined && (
                                        <Badge tone='warning'>{t('config.recipients.unsaved')}</Badge>
                                    )}
                                </label>
                            </li>
                        ))}
                    </ul>
                    <p className='text-xs text-muted'>
                        {count === 0 ? t('config.recipients.none') : t('config.recipients.count', { count })}
                    </p>
                </>
            )}
        </div>
    );
}
