import { useEffect, useState, type FormEvent } from 'react';
import { useTranslation } from 'react-i18next';
import {
    ApiError,
    changeOwnPassword,
    requestOwnEmailChange,
    setOwnDisplayName,
    verifyOwnEmailChange,
} from '../api/client';
import { Button } from '../components/Button';
import { CODE_LENGTH, CodeInput } from '../components/CodeInput';
import { emailInputProps, Field, inputClass } from '../components/FormControls';
import { Card, DescriptionList, PageHeader } from '../components/PageHeader';
import { Alert } from '../components/StateViews';
import { RoleBadge } from '../components/StatusBadges';
import { MAX_DISPLAY_NAME_LENGTH, MIN_PASSWORD_LENGTH } from '../constants';
import { useAuth } from '../context/AuthContext';
import { useToast } from '../context/ToastContext';
import type { Me } from '../types';
import { accountErrorMessage } from '../utils/accountErrors';

/** The signed-in account: its fields and the changes the user makes alone. */
export function ProfilePage() {
    const { t } = useTranslation();
    const { me } = useAuth();

    return (
        <>
            <PageHeader title={t('profile.title')} description={t('profile.description')} />
            {me && (
                <div className='grid max-w-2xl grid-cols-1 gap-4'>
                    <Card title={t('profile.account')}>
                        <DescriptionList
                            items={[
                                { label: t('accounts.email'), value: me.email },
                                { label: t('accounts.displayName'), value: me.display_name },
                                { label: t('accounts.role'), value: <RoleBadge role={me.role} /> },
                            ]}
                        />
                    </Card>
                    <DisplayNameCard me={me} />
                    <EmailCard me={me} />
                    <PasswordCard me={me} />
                </div>
            )}
        </>
    );
}

function DisplayNameCard({ me }: { me: Me }) {
    const { t } = useTranslation();
    const { updateMe } = useAuth();
    const { showToast } = useToast();
    const [name, setName] = useState(me.display_name);
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState<string | null>(null);

    useEffect(() => setName(me.display_name), [me.display_name]);

    async function submit(e: FormEvent) {
        e.preventDefault();
        setBusy(true);
        setError(null);
        try {
            updateMe(await setOwnDisplayName(name.trim()));
            showToast(t('profile.displayNameChanged'));
        } catch (err) {
            setError(accountErrorMessage(t, err));
        } finally {
            setBusy(false);
        }
    }

    return (
        <Card title={t('profile.changeDisplayName')}>
            <form onSubmit={e => void submit(e)} className='flex flex-col gap-4'>
                <Field
                    label={t('accounts.displayName')}
                    htmlFor='pf-display-name'
                    hint={t('accounts.displayNameHint', { max: MAX_DISPLAY_NAME_LENGTH })}
                    required
                >
                    <input
                        id='pf-display-name'
                        className={inputClass}
                        value={name}
                        required
                        autoComplete='name'
                        onChange={e => setName(e.target.value)}
                    />
                </Field>
                {error && <Alert tone='danger'>{error}</Alert>}
                <div className='flex justify-end'>
                    <Button
                        type='submit'
                        variant='primary'
                        icon='save'
                        busy={busy}
                        disabled={name.trim() === me.display_name}
                    >
                        {t('profile.changeDisplayName')}
                    </Button>
                </div>
            </form>
        </Card>
    );
}

function EmailCard({ me }: { me: Me }) {
    const { t } = useTranslation();
    const { updateMe, refreshMe } = useAuth();
    const { showToast } = useToast();
    const [email, setEmail] = useState('');
    // The new address while its code is awaited.
    const [pending, setPending] = useState<string | null>(null);
    const [code, setCode] = useState('');
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState<string | null>(null);

    function changed() {
        setEmail('');
        setPending(null);
        setCode('');
        showToast(t('profile.emailChanged'));
    }

    async function submitAddress(e: FormEvent) {
        e.preventDefault();
        const next = email.trim();
        setBusy(true);
        setError(null);
        try {
            const r = await requestOwnEmailChange(next);
            if (r.applied) {
                await refreshMe();
                changed();
            } else {
                setPending(next);
                setCode('');
            }
        } catch (err) {
            setError(accountErrorMessage(t, err));
        } finally {
            setBusy(false);
        }
    }

    async function submitCode(e: FormEvent) {
        e.preventDefault();
        setBusy(true);
        setError(null);
        try {
            updateMe(await verifyOwnEmailChange(code));
            changed();
        } catch (err) {
            if (err instanceof ApiError && err.status === 400) {
                setCode('');
                setError(t('code.invalid'));
            } else if (err instanceof ApiError && err.status === 410) {
                setPending(null);
                setCode('');
                setError(t('profile.emailCodeExpired'));
            } else {
                setError(accountErrorMessage(t, err));
            }
        } finally {
            setBusy(false);
        }
    }

    return (
        <Card title={t('profile.changeEmail')}>
            {pending === null ? (
                <form onSubmit={e => void submitAddress(e)} className='flex flex-col gap-4'>
                    <Field label={t('profile.newEmail')} htmlFor='pf-email' hint={t('profile.newEmailHint')} required>
                        <input
                            id='pf-email'
                            {...emailInputProps}
                            className={inputClass}
                            value={email}
                            required
                            autoComplete='email'
                            onChange={e => setEmail(e.target.value)}
                        />
                    </Field>
                    <p className='text-xs text-muted'>{t('profile.emailNote')}</p>
                    {error && <Alert tone='danger'>{error}</Alert>}
                    <div className='flex justify-end'>
                        <Button
                            type='submit'
                            variant='primary'
                            icon='mail'
                            busy={busy}
                            disabled={email.trim() === '' || email.trim() === me.email}
                        >
                            {t('profile.changeEmail')}
                        </Button>
                    </div>
                </form>
            ) : (
                <form onSubmit={e => void submitCode(e)} className='flex flex-col gap-4'>
                    <Alert tone='info'>{t('profile.emailCodeSent', { email: pending })}</Alert>
                    <Field
                        label={t('code.label')}
                        htmlFor='pf-email-code'
                        hint={t('code.hint', { digits: CODE_LENGTH })}
                        required
                    >
                        <CodeInput id='pf-email-code' value={code} onChange={setCode} autoFocus />
                    </Field>
                    {error && <Alert tone='danger'>{error}</Alert>}
                    <div className='flex flex-wrap justify-end gap-2'>
                        <Button
                            disabled={busy}
                            onClick={() => {
                                setPending(null);
                                setCode('');
                                setError(null);
                            }}
                        >
                            {t('common.cancel')}
                        </Button>
                        <Button type='submit' variant='primary' icon='verified' busy={busy}>
                            {t('profile.confirmEmail')}
                        </Button>
                    </div>
                </form>
            )}
        </Card>
    );
}

function PasswordCard({ me }: { me: Me }) {
    const { t } = useTranslation();
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
            showToast(t('profile.passwordChanged'));
        } catch (err) {
            setError(accountErrorMessage(t, err));
        } finally {
            setBusy(false);
        }
    }

    return (
        <Card title={t('profile.changePassword')}>
            <form onSubmit={e => void submit(e)} className='flex flex-col gap-4'>
                <input type='text' autoComplete='username' value={me.email} readOnly hidden />
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
                <p className='text-xs text-muted'>{t('profile.passwordNote')}</p>
                {error && <Alert tone='danger'>{error}</Alert>}
                {done && <Alert tone='success'>{t('profile.passwordChanged')}</Alert>}
                <div className='flex justify-end'>
                    <Button type='submit' variant='primary' icon='save' busy={busy}>
                        {t('profile.changePassword')}
                    </Button>
                </div>
            </form>
        </Card>
    );
}
