import { createContext, useCallback, useContext, useEffect, useMemo, useState, type ReactNode } from 'react';
import {
    getMe,
    login as apiLogin,
    loginVerify as apiLoginVerify,
    logout as apiLogout,
    setOwnLanguage,
    setUnauthorizedHandler,
} from '../api/client';
import type { LangSetting } from '../constants';
import { leaveExtensions, loadExtensions } from '../extensions/loader';
import { applyLangSetting } from '../i18n/i18n';
import type { Me } from '../types';

type AuthStatus = 'loading' | 'authenticated' | 'anonymous';

/** Outcome of the password step: logged in, or the mailed code is needed. */
export type LoginStep = { kind: 'done' } | { kind: 'code'; challenge: string };

interface AuthContextValue {
    status: AuthStatus;
    me: Me | null;
    isAdmin: boolean;
    /** Password step of the login. */
    login: (email: string, password: string, remember: boolean) => Promise<LoginStep>;
    /** Code step of the login (after login answered 'code'). */
    verify: (challenge: string, code: string, trust: boolean) => Promise<void>;
    logout: () => Promise<void>;
    /** Replaces the signed-in account after an own change (display name, e-mail address). */
    updateMe: (me: Me) => void;
    /** Reloads the signed-in account from the server. */
    refreshMe: () => Promise<void>;
    /** The language setting in effect: the account's, or the temporary one before login. */
    langSetting: LangSetting;
    /** Changes the display language; saved to the account when signed in. */
    setLanguage: (setting: LangSetting) => Promise<void>;
}

const AuthContext = createContext<AuthContextValue | null>(null);

/** Holds the signed-in account (GET /auth/me) and drops it on any 401. */
export function AuthProvider({ children }: { children: ReactNode }) {
    const [me, setMe] = useState<Me | null>(null);
    const [status, setStatus] = useState<AuthStatus>('loading');
    // Language chosen on the login screen; kept only until the page is left.
    const [anonLang, setAnonLang] = useState<LangSetting>('auto');

    useEffect(() => {
        setUnauthorizedHandler(() => {
            if (leaveExtensions()) return;
            setMe(null);
            setStatus('anonymous');
        });
        let active = true;
        getMe()
            .then(async m => {
                // The extension modules of the account register their routes
                // before any page is shown, so that their URLs never show
                // "not found" while loading.
                await loadExtensions(m.role);
                if (!active) return;
                setMe(m);
                setStatus('authenticated');
                void applyLangSetting(m.language);
            })
            .catch(() => {
                if (!active) return;
                setMe(null);
                setStatus('anonymous');
            });
        return () => {
            active = false;
            setUnauthorizedHandler(null);
        };
    }, []);

    const signedIn = useCallback(async (m: Me) => {
        await loadExtensions(m.role);
        setMe(m);
        setStatus('authenticated');
        await applyLangSetting(m.language);
    }, []);

    const login = useCallback(
        async (email: string, password: string, remember: boolean): Promise<LoginStep> => {
            const r = await apiLogin(email, password, remember);
            if (r.kind === 'code') return r;
            await signedIn(r.me);
            return { kind: 'done' };
        },
        [signedIn]
    );

    const verify = useCallback(
        async (challenge: string, code: string, trust: boolean) => {
            await signedIn(await apiLoginVerify(challenge, code, trust));
        },
        [signedIn]
    );

    const updateMe = useCallback((m: Me) => setMe(m), []);

    const refreshMe = useCallback(async () => {
        setMe(await getMe());
    }, []);

    const setLanguage = useCallback(
        async (setting: LangSetting) => {
            if (me) {
                const m = await setOwnLanguage(setting);
                setMe(m);
            } else {
                setAnonLang(setting);
            }
            await applyLangSetting(setting);
        },
        [me]
    );

    const logout = useCallback(async () => {
        try {
            await apiLogout();
        } finally {
            if (!leaveExtensions()) {
                setMe(null);
                setStatus('anonymous');
                setAnonLang('auto');
                void applyLangSetting('auto');
            }
        }
    }, []);

    const value = useMemo<AuthContextValue>(
        () => ({
            status,
            me,
            isAdmin: me?.role === 'admin',
            login,
            verify,
            logout,
            updateMe,
            refreshMe,
            langSetting: me ? me.language : anonLang,
            setLanguage,
        }),
        [status, me, anonLang, login, verify, logout, updateMe, refreshMe, setLanguage]
    );

    return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth(): AuthContextValue {
    const ctx = useContext(AuthContext);
    if (!ctx) throw new Error('useAuth must be used inside AuthProvider');
    return ctx;
}
