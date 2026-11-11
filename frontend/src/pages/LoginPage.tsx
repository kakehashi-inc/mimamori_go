import { useState, type FormEvent } from 'react';
import { useTranslation } from 'react-i18next';
import { Navigate, useLocation, useNavigate } from 'react-router-dom';
import { ApiError } from '../api/client';
import { Button } from '../components/Button';
import { Field, inputClass } from '../components/FormControls';
import { LanguageSwitch } from '../components/LanguageSwitch';
import { Alert, Loading } from '../components/StateViews';
import { useAuth } from '../context/AuthContext';
import { useHealth } from '../hooks/useHealth';
import { errorMessage, formatVersion } from '../utils/format';

export function LoginPage() {
    const { t } = useTranslation();
    const { status, login } = useAuth();
    const { health } = useHealth();
    const navigate = useNavigate();
    const location = useLocation();
    const from = (location.state as { from?: string } | null)?.from ?? '/';
    const [name, setName] = useState('');
    const [password, setPassword] = useState('');
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState<string | null>(null);

    if (status === 'loading') return <Loading />;
    if (status === 'authenticated' && !busy) return <Navigate to={from} replace />;

    async function submit(e: FormEvent) {
        e.preventDefault();
        setBusy(true);
        setError(null);
        try {
            await login(name.trim(), password);
            navigate(from, { replace: true });
        } catch (err) {
            if (err instanceof ApiError && err.status === 401) setError(t('login.invalid'));
            else if (err instanceof ApiError && err.status === 423) setError(t('login.locked'));
            else setError(errorMessage(t, err));
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
                        <p className='mt-1 text-sm text-muted'>{t('login.subtitle')}</p>
                    </div>
                    <form onSubmit={e => void submit(e)} className='flex flex-col gap-4'>
                        <Field label={t('login.name')} htmlFor='login-name'>
                            <input
                                id='login-name'
                                className={inputClass}
                                autoComplete='username'
                                autoFocus
                                required
                                value={name}
                                onChange={e => setName(e.target.value)}
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
                        {error && <Alert tone='danger'>{error}</Alert>}
                        <Button type='submit' variant='primary' busy={busy} icon='login' className='w-full'>
                            {t('login.submit')}
                        </Button>
                    </form>
                    {health && <p className='mt-6 text-center text-xs text-muted'>{formatVersion(health.version)}</p>}
                </div>
            </section>
        </div>
    );
}
