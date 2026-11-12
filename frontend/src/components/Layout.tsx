import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Navigate, Outlet, useLocation } from 'react-router-dom';
import { getOverview } from '../api/client';
import { useAuth } from '../context/AuthContext';
import type { Overview } from '../types';
import { Header } from './Header';
import { Loading } from './StateViews';

// How often the overview (builtin agent state) is refreshed.
const OVERVIEW_POLL_MS = 60000;

/** Frame of every signed-in page: header, builtin agent warning and content. */
export function Layout() {
    const { t } = useTranslation();
    const { status } = useAuth();
    const location = useLocation();
    const [overview, setOverview] = useState<Overview | null>(null);

    useEffect(() => {
        if (status !== 'authenticated') return;
        let active = true;
        // The periodic refresh is sent as a background request so that an
        // open but unused page does not keep the login session alive.
        const load = (background: boolean) => {
            getOverview(background)
                .then(o => {
                    if (active) setOverview(o);
                })
                .catch(() => {
                    // Keep the previous state; the page itself reports errors.
                });
        };
        load(false);
        const timer = window.setInterval(() => load(true), OVERVIEW_POLL_MS);
        return () => {
            active = false;
            window.clearInterval(timer);
        };
    }, [status]);

    if (status === 'loading') return <Loading />;
    if (status === 'anonymous')
        return <Navigate to='/login' replace state={{ from: location.pathname + location.search }} />;

    // A built-in agent disabled in the configuration is the administrator's
    // choice and is not reported as stopped.
    const stopped = overview && overview.builtin_agent.enabled && !overview.builtin_agent.running;

    return (
        <div className='flex min-h-screen flex-col'>
            <Header />
            {stopped && (
                <div className='border-b border-danger/30 bg-danger-soft' role='alert'>
                    <div className='mx-auto flex max-w-7xl items-start gap-2 px-4 py-2 text-sm text-ink'>
                        <span className='material-icons-outlined text-lg text-danger' aria-hidden='true'>
                            report
                        </span>
                        <div>
                            <span className='font-semibold text-danger'>{t('layout.builtinStopped')}</span>
                            {overview.builtin_agent.message && (
                                <span className='ml-2 break-words'>{overview.builtin_agent.message}</span>
                            )}
                        </div>
                    </div>
                </div>
            )}
            <main className='mx-auto flex w-full max-w-7xl flex-1 flex-col px-4 py-6'>
                <Outlet />
            </main>
        </div>
    );
}
