import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Link, NavLink, useLocation, useNavigate } from 'react-router-dom';
import { useAuth } from '../context/AuthContext';
import { useHealth } from '../hooks/useHealth';
import { formatVersion } from '../utils/format';
import { LanguageSwitch } from './LanguageSwitch';
import { RoleBadge } from './StatusBadges';

interface NavItem {
    to: string;
    key:
        | 'agents'
        | 'detections'
        | 'tokens'
        | 'rules'
        | 'templates'
        | 'suppressions'
        | 'certificates'
        | 'history'
        | 'accounts'
        | 'config'
        | 'guide';
    icon: string;
    adminOnly?: boolean;
}

const NAV: NavItem[] = [
    { to: '/agents', key: 'agents', icon: 'dns' },
    { to: '/detections', key: 'detections', icon: 'notifications' },
    { to: '/tokens', key: 'tokens', icon: 'key' },
    { to: '/rules', key: 'rules', icon: 'rule' },
    { to: '/templates', key: 'templates', icon: 'library_books' },
    { to: '/suppressions', key: 'suppressions', icon: 'block' },
    { to: '/certificates', key: 'certificates', icon: 'verified_user' },
    { to: '/history', key: 'history', icon: 'history' },
    { to: '/accounts', key: 'accounts', icon: 'manage_accounts', adminOnly: true },
    { to: '/config', key: 'config', icon: 'tune', adminOnly: true },
    { to: '/guide', key: 'guide', icon: 'menu_book' },
];

export function Header() {
    const { t } = useTranslation();
    const { health } = useHealth();
    const { me, isAdmin, logout } = useAuth();
    const navigate = useNavigate();
    const { pathname } = useLocation();
    const [busy, setBusy] = useState(false);

    async function handleLogout() {
        setBusy(true);
        try {
            await logout();
        } catch {
            // The session is dropped locally even when the request failed.
        } finally {
            setBusy(false);
            navigate('/login', { replace: true });
        }
    }

    return (
        <header className='sticky top-0 z-30 border-b border-line bg-surface/90 backdrop-blur'>
            <div className='mx-auto flex max-w-7xl items-center justify-between gap-4 px-4 py-2.5'>
                <Link
                    to='/'
                    className='flex items-center gap-2 rounded text-base font-semibold text-ink focus:outline-none focus-visible:ring-2 focus-visible:ring-accent'
                >
                    <span className='material-icons-outlined text-xl text-accent' aria-hidden='true'>
                        visibility
                    </span>
                    {t('app.title')}
                    {health && <span className='text-xs font-normal text-muted'>{formatVersion(health.version)}</span>}
                </Link>
                <div className='flex items-center gap-1 sm:gap-2'>
                    {me && (
                        <Link
                            to='/settings'
                            title={t('nav.settings')}
                            className='flex items-center gap-2 rounded-md px-2 py-1 text-sm text-ink hover:bg-line focus:outline-none focus-visible:ring-2 focus-visible:ring-accent'
                        >
                            <span className='material-icons-outlined text-lg text-muted' aria-hidden='true'>
                                account_circle
                            </span>
                            <span className='max-w-32 truncate'>{me.name}</span>
                            <span className='hidden sm:inline'>
                                <RoleBadge role={me.role} />
                            </span>
                        </Link>
                    )}
                    <LanguageSwitch />
                    <button
                        type='button'
                        onClick={() => void handleLogout()}
                        disabled={busy}
                        title={t('nav.logout')}
                        aria-label={t('nav.logout')}
                        className='flex h-8 items-center gap-1 rounded-md px-2 text-sm text-muted hover:bg-line hover:text-ink focus:outline-none focus-visible:ring-2 focus-visible:ring-accent disabled:opacity-50'
                    >
                        <span className='material-icons-outlined text-xl' aria-hidden='true'>
                            logout
                        </span>
                        <span className='hidden md:inline'>{t('nav.logout')}</span>
                    </button>
                </div>
            </div>
            <nav className='mx-auto max-w-7xl overflow-x-auto px-2' aria-label={t('nav.label')}>
                <ul className='flex gap-1'>
                    {NAV.filter(item => !item.adminOnly || isAdmin).map(item => (
                        <li key={item.to}>
                            <NavLink
                                to={item.to}
                                className={({ isActive: active }) => {
                                    // The agent list is also the home page.
                                    const isActive = active || (item.key === 'agents' && pathname === '/');
                                    return `-mb-px flex items-center gap-1.5 border-b-2 px-3 py-2 text-sm whitespace-nowrap transition-colors focus:outline-none focus-visible:ring-2 focus-visible:ring-accent ${
                                        isActive
                                            ? 'border-accent font-medium text-accent'
                                            : 'border-transparent text-muted hover:border-line hover:text-ink'
                                    }`;
                                }}
                            >
                                <span className='material-icons-outlined text-base' aria-hidden='true'>
                                    {item.icon}
                                </span>
                                {t(`nav.${item.key}`)}
                            </NavLink>
                        </li>
                    ))}
                </ul>
            </nav>
        </header>
    );
}
