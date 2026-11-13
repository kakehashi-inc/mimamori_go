import { useState, type FormEvent } from 'react';
import { useTranslation } from 'react-i18next';
import { Navigate, useLocation, useNavigate } from 'react-router-dom';
import { ApiError } from '../api/client';
import { Button } from '../components/Button';
import { CODE_LENGTH, CodeInput } from '../components/CodeInput';
import { checkboxClass, emailInputProps, Field, inputClass } from '../components/FormControls';
import { LanguageSwitch } from '../components/LanguageSwitch';
import { Alert, Loading, SessionCheckError } from '../components/StateViews';
import { useAuth } from '../context/AuthContext';
import { useHealth } from '../hooks/useHealth';
import { isCodeMailFailed } from '../utils/accountErrors';
import { errorMessage, formatVersion } from '../utils/format';

// Shown until /health answers.
const DEFAULT_REMEMBER_DAYS = 30;
const DEFAULT_TRUST_DAYS = 30;

/**
 * Login in two steps: e-mail address and password, then (unless the server
 * logs in at once) the code mailed to the account's address.
 */
export function LoginPage() {
    const { t } = useTranslation();
    const { status, checkError, retryCheck, login, verify } = useAuth();
    const { health } = useHealth();
    const navigate = useNavigate();
    const location = useLocation();
    const from = (location.state as { from?: string } | null)?.from ?? '/';
    const [email, setEmail] = useState('');
    const [password, setPassword] = useState('');
    const [remember, setRemember] = useState(false);
    // Set while the code step is shown.
    const [challenge, setChallenge] = useState<string | null>(null);
    const [code, setCode] = useState('');
    const [trust, setTrust] = useState(false);
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState<string | null>(null);
    const [notice, setNotice] = useState<string | null>(null);

    if (status === 'loading') return <Loading />;
    if (status === 'error') return <SessionCheckError error={checkError} onRetry={retryCheck} />;
    if (status === 'authenticated' && !busy) return <Navigate to={from} replace />;

    /** Back to the password step, keeping the e-mail address. */
    function startOver(message: string | null) {
        setChallenge(null);
        setCode('');
        setTrust(false);
        setPassword('');
        setError(message);
        setNotice(null);
    }

    function loginError(err: unknown): string {
        if (err instanceof ApiError && err.status === 423) return t('login.locked');
        if (isCodeMailFailed(err)) return t('code.mailFailed');
        return errorMessage(t, err);
    }

    async function submitPassword(e: FormEvent) {
        e.preventDefault();
        setBusy(true);
        setError(null);
        setNotice(null);
        try {
            const step = await login(email.trim(), password, remember);
            if (step.kind === 'done') {
                navigate(from, { replace: true });
                return;
            }
            setChallenge(step.challenge);
            setCode('');
            setNotice(t('login.codeSent'));
        } catch (err) {
            setError(err instanceof ApiError && err.status === 401 ? t('login.invalid') : loginError(err));
        } finally {
            setBusy(false);
        }
    }

    async function submitCode(e: FormEvent) {
        e.preventDefault();
        if (!challenge) return;
        setBusy(true);
        setError(null);
        try {
            await verify(challenge, code, trust);
            navigate(from, { replace: true });
        } catch (err) {
            if (err instanceof ApiError && err.status === 401) {
                setCode('');
                setError(t('code.invalid'));
            } else if (err instanceof ApiError && err.status === 410) {
                startOver(t('login.codeExpired'));
            } else if (err instanceof ApiError && err.status === 423) {
                startOver(t('login.locked'));
            } else {
                setError(loginError(err));
            }
        } finally {
            setBusy(false);
        }
    }

    return (
        <div className='flex min-h-screen flex-col'>
            <div className='flex justify-end p-3'>
                <LanguageSwitch />
            </div>
            <section className='flex flex-1 items-start justify-center px-4 pt-8 pb-16 sm:items-center sm:pt-0'>
                <div className='w-full max-w-sm rounded-2xl border border-line bg-surface p-8 shadow-sm'>
                    <div className='mb-6 text-center'>
                        <span className='material-icons-outlined text-4xl text-accent' aria-hidden='true'>
                            visibility
                        </span>
                        <h1 className='mt-1 text-2xl font-bold tracking-tight text-ink'>{t('app.title')}</h1>
                        <p className='mt-1 text-sm text-muted'>
                            {challenge ? t('login.codeSubtitle') : t('login.subtitle')}
                        </p>
                    </div>
                    {challenge === null ? (
                        <form onSubmit={e => void submitPassword(e)} className='flex flex-col gap-4'>
                            <Field label={t('login.email')} htmlFor='login-email'>
                                <input
                                    id='login-email'
                                    {...emailInputProps}
                                    className={inputClass}
                                    autoComplete='username'
                                    autoFocus
                                    required
                                    value={email}
                                    onChange={e => setEmail(e.target.value)}
                                />
                            </Field>
                            <Field label={t('login.password')} htmlFor='login-password'>
                                <input
                                    id='login-password'
                                    type='password'
                                    className={inputClass}
                                    autoComplete='current-password'
                                    required
                                    value={password}
                                    onChange={e => setPassword(e.target.value)}
                                />
                            </Field>
                            <label className='flex cursor-pointer items-start gap-2 text-sm text-ink'>
                                <input
                                    type='checkbox'
                                    className={`${checkboxClass} mt-0.5`}
                                    checked={remember}
                                    onChange={e => setRemember(e.target.checked)}
                                />
                                <span>
                                    {t('login.remember')}
                                    <span className='block text-xs text-muted'>
                                        {t('login.rememberHint', {
                                            days: health?.remember_days ?? DEFAULT_REMEMBER_DAYS,
                                        })}
                                    </span>
                                </span>
                            </label>
                            {error && <Alert tone='danger'>{error}</Alert>}
                            <Button type='submit' variant='primary' busy={busy} icon='login' className='w-full'>
                                {t('login.submit')}
                            </Button>
                        </form>
                    ) : (
                        <form onSubmit={e => void submitCode(e)} className='flex flex-col gap-4'>
                            {notice && <Alert tone='info'>{notice}</Alert>}
                            <Field
                                label={t('code.label')}
                                htmlFor='login-code'
                                hint={t('code.hint', { digits: CODE_LENGTH })}
                            >
                                <CodeInput id='login-code' value={code} onChange={setCode} autoFocus />
                            </Field>
                            <label className='flex cursor-pointer items-start gap-2 text-sm text-ink'>
                                <input
                                    type='checkbox'
                                    className={`${checkboxClass} mt-0.5`}
                                    checked={trust}
                                    onChange={e => setTrust(e.target.checked)}
                                />
                                <span>
                                    {t('login.trust', { days: health?.trust_days ?? DEFAULT_TRUST_DAYS })}
                                    <span className='block text-xs text-muted'>{t('login.trustHint')}</span>
                                </span>
                            </label>
                            {error && <Alert tone='danger'>{error}</Alert>}
                            <Button type='submit' variant='primary' busy={busy} icon='verified' className='w-full'>
                                {t('login.verify')}
                            </Button>
                            <Button
                                icon='restart_alt'
                                className='w-full'
                                disabled={busy}
                                onClick={() => startOver(null)}
                            >
                                {t('login.startOver')}
                            </Button>
                            <p className='text-xs text-muted'>{t('login.noCodeHint')}</p>
                        </form>
                    )}
                    {health && <p className='mt-6 text-center text-xs text-muted'>{formatVersion(health.version)}</p>}
                </div>
            </section>
        </div>
    );
}
