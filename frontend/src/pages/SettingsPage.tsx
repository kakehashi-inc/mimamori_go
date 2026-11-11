import { useState, type FormEvent } from 'react';
import { useTranslation } from 'react-i18next';
import { ApiError, changeOwnPassword } from '../api/client';
import { Button } from '../components/Button';
import { Field, inputClass } from '../components/FormControls';
import { Card, DescriptionList, PageHeader } from '../components/PageHeader';
import { Alert } from '../components/StateViews';
import { RoleBadge } from '../components/StatusBadges';
import { MIN_PASSWORD_LENGTH } from '../constants';
import { useAuth } from '../context/AuthContext';
import { useToast } from '../context/ToastContext';
import { errorMessage } from '../utils/format';

export function SettingsPage() {
    const { t } = useTranslation();
    const { me } = useAuth();
    const { showToast } = useToast();
    const [current, setCurrent] = useState('');
    const [next, setNext] = useState('');
    const [confirm, setConfirm] = useState('');
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState<string | null>(null);
    const [done, setDone] = useState(false);

    async function submit(e: FormEvent) {
        e.preventDefault();
        setDone(false);
        if (next.length < MIN_PASSWORD_LENGTH) {
            setError(t('password.tooShort', { min: MIN_PASSWORD_LENGTH }));
            return;
        }
        if (next !== confirm) {
            setError(t('password.mismatch'));
            return;
        }
        setBusy(true);
        setError(null);
        try {
            await changeOwnPassword(current, next);
            setCurrent('');
            setNext('');
            setConfirm('');
            setDone(true);
            showToast(t('settings.changed'));
        } catch (err) {
            setError(
                err instanceof ApiError && err.status === 400 && err.message.startsWith('HTTP ')
                    ? t('settings.wrongCurrent')
                    : errorMessage(t, err)
            );
        } finally {
            setBusy(false);
        }
    }

    return (
        <>
            <PageHeader title={t('settings.title')} />
            <div className='grid max-w-2xl grid-cols-1 gap-4'>
                {me && (
                    <Card title={t('settings.account')}>
                        <DescriptionList
                            items={[
                                { label: t('accounts.name'), value: me.name },
                                { label: t('accounts.role'), value: <RoleBadge role={me.role} /> },
                            ]}
                        />
                    </Card>
                )}
                <Card title={t('settings.changePassword')}>
                    <form onSubmit={e => void submit(e)} className='flex flex-col gap-4'>
                        <input type='text' autoComplete='username' value={me?.name ?? ''} readOnly hidden />
                        <Field label={t('password.current')} htmlFor='sp-current' required>
                            <input
                                id='sp-current'
                                type='password'
                                className={inputClass}
                                value={current}
                                required
                                autoComplete='current-password'
                                onChange={e => setCurrent(e.target.value)}
                            />
                        </Field>
                        <Field
                            label={t('password.new')}
                            htmlFor='sp-new'
                            hint={t('password.minHint', { min: MIN_PASSWORD_LENGTH })}
                            required
                        >
                            <input
                                id='sp-new'
                                type='password'
                                className={inputClass}
                                value={next}
                                required
                                autoComplete='new-password'
                                onChange={e => setNext(e.target.value)}
                            />
                        </Field>
                        <Field label={t('password.confirm')} htmlFor='sp-confirm' required>
                            <input
                                id='sp-confirm'
                                type='password'
                                className={inputClass}
                                value={confirm}
                                required
                                autoComplete='new-password'
                                onChange={e => setConfirm(e.target.value)}
                            />
                        </Field>
                        <p className='text-xs text-muted'>{t('settings.otherSessionsNote')}</p>
                        {error && <Alert tone='danger'>{error}</Alert>}
                        {done && <Alert tone='success'>{t('settings.changed')}</Alert>}
                        <div className='flex justify-end'>
                            <Button type='submit' variant='primary' icon='save' busy={busy}>
                                {t('settings.changePassword')}
                            </Button>
                        </div>
                    </form>
                </Card>
            </div>
        </>
    );
}
