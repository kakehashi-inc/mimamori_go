import { useCallback, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Link, NavLink, useLocation, useNavigate } from 'react-router-dom';
import { useAuth } from '../context/AuthContext';
import { useExtensions } from '../extensions/registry';
import { useClickOutside } from '../hooks/useClickOutside';
import { useHealth } from '../hooks/useHealth';
import { formatVersion } from '../utils/format';
import type { Role } from '../types';
import { LanguageSwitch } from './LanguageSwitch';
import { RoleBadge } from './StatusBadges';

interface NavItem {
    to: string;
    icon: string;
    /** Label in the current language. */
    label: string;
    /** Position among the items; extensions place their items by this number. */
    order: number;
}

type OwnKey =
    | 'agents'
    | 'commandResults'
    | 'detections'
    | 'tokens'
    | 'rules'
    | 'templates'
    | 'suppressions'
    | 'certificates'
    | 'history'
    | 'guide';

const NAV: { to: string; key: OwnKey; icon: string; order: number }[] = [
    { to: '/agents', key: 'agents', icon: 'dns', order: 10 },
    { to: '/command-results', key: 'commandResults', icon: 'terminal', order: 15 },
    { to: '/detections', key: 'detections', icon: 'notifications', order: 20 },
    { to: '/tokens', key: 'tokens', icon: 'key', order: 30 },
    { to: '/rules', key: 'rules', icon: 'rule', order: 40 },
    { to: '/templates', key: 'templates', icon: 'library_books', order: 50 },
    { to: '/suppressions', key: 'suppressions', icon: 'block', order: 60 },
    { to: '/certificates', key: 'certificates', icon: 'verified_user', order: 70 },
    { to: '/history', key: 'history', icon: 'history', order: 80 },
    { to: '/guide', key: 'guide', icon: 'menu_book', order: 110 },
];

export function Header() {
    const { t } = useTranslation();
    const { health } = useHealth();
    const { me, logout } = useAuth();
    const { navItems } = useExtensions();
    const navigate = useNavigate();
    const { pathname } = useLocation();
    const [busy, setBusy] = useState(false);

    const items: NavItem[] = [
        ...NAV.map(n => ({ to: n.to, icon: n.icon, label: t(`nav.${n.key}`), order: n.order })),
        ...navItems.map(n => ({ to: n.to, icon: n.icon, label: n.label(), order: n.order })),
    ].sort((a, b) => a.order - b.order);

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
                    {me && <AccountMenu displayName={me.display_name} role={me.role} />}
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
                    {items.map(item => (
                        <li key={item.to}>
                            <NavLink
                                to={item.to}
                                className={({ isActive: active }) => {
                                    // The agent list is also the home page.
                                    const isActive = active || (item.to === '/agents' && pathname === '/');
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
                                {item.label}
                            </NavLink>
                        </li>
                    ))}
                </ul>
            </nav>
        </header>
    );
}

/** The signed-in account at the top right: a button that opens the account menu. */
function AccountMenu({ displayName, role }: { displayName: string; role: Role }) {
    const { t } = useTranslation();
    const [open, setOpen] = useState(false);
    const ref = useRef<HTMLDivElement>(null);
    const close = useCallback(() => setOpen(false), []);
    useClickOutside(ref, open, close);

    return (
        <div className='relative' ref={ref}>
            <button
                type='button'
                aria-haspopup='menu'
                aria-expanded={open}
                title={t('nav.accountMenu')}
                onClick={() => setOpen(v => !v)}
                className='flex items-center gap-2 rounded-md px-2 py-1 text-sm text-ink hover:bg-line focus:outline-none focus-visible:ring-2 focus-visible:ring-accent'
            >
                <span className='material-icons-outlined text-lg text-muted' aria-hidden='true'>
                    account_circle
                </span>
                <span className='max-w-32 truncate'>{displayName}</span>
                <span className='hidden sm:inline'>
                    <RoleBadge role={role} />
                </span>
                <span className='material-icons-outlined text-base text-muted' aria-hidden='true'>
                    {open ? 'expand_less' : 'expand_more'}
                </span>
            </button>
            {open && (
                <div
                    role='menu'
                    className='absolute right-0 z-20 mt-1 min-w-40 overflow-hidden rounded-md border border-line bg-surface py-1 shadow-xl'
                >
                    <Link
                        to='/profile'
                        role='menuitem'
                        onClick={close}
                        className='flex w-full items-center gap-2 px-4 py-2 text-left text-sm text-ink hover:bg-line focus:bg-line focus:outline-none'
                    >
                        <span className='material-icons-outlined text-base text-muted' aria-hidden='true'>
                            person
                        </span>
                        {t('nav.profile')}
                    </Link>
                </div>
            )}
        </div>
    );
}
