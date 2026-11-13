import { createContext, useCallback, useContext, useEffect, useMemo, useState, type ReactNode } from 'react';
import {
    ApiError,
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

/**
 * 'error': the session check failed for another reason than 401 (busy server,
 * network failure) even after the retries, so whether a session exists is unknown.
 */
type AuthStatus = 'loading' | 'authenticated' | 'anonymous' | 'error';

// Pauses before the repeated session checks after a failure other than 401.
const CHECK_RETRY_MS = [500, 1500];

function wait(ms: number): Promise<void> {
    return new Promise(resolve => window.setTimeout(resolve, ms));
}

/** Outcome of the password step: logged in, or the mailed code is needed. */
export type LoginStep = { kind: 'done' } | { kind: 'code'; challenge: string };

interface AuthContextValue {
    status: AuthStatus;
    me: Me | null;
    isAdmin: boolean;
    /** Error of the last session check while status is 'error'. */
    checkError: unknown;
    /** Checks the session again after status became 'error'. */
    retryCheck: () => void;
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

/**
 * Holds the signed-in account (GET /auth/me) and drops it on any 401. A failed
 * session check other than 401 leaves status 'error' (see the check below).
 */
export function AuthProvider({ children }: { children: ReactNode }) {
    const [me, setMe] = useState<Me | null>(null);
    const [status, setStatus] = useState<AuthStatus>('loading');
    // Language chosen on the login screen; kept only until the page is left.
    const [anonLang, setAnonLang] = useState<LangSetting>('auto');

    const [checkError, setCheckError] = useState<unknown>(null);
    // Incremented by retryCheck to run the session check again.
    const [checkRun, setCheckRun] = useState(0);

    useEffect(() => {
        setUnauthorizedHandler(() => {
            if (leaveExtensions()) return;
            setMe(null);
            setStatus('anonymous');
        });
        return () => setUnauthorizedHandler(null);
    }, []);

    // Only a 401 means "not signed in". Any other failure (a busy server, a
    // network failure) is retried a few times and then shown as an error with
    // a retry action, so that a signed-in person is not sent to the login screen.
    useEffect(() => {
        let active = true;
        const check = async () => {
            for (let attempt = 0; ; attempt++) {
                try {
                    const m = await getMe();
                    // The extension modules of the account register their routes
                    // before any page is shown, so that their URLs never show
                    // "not found" while loading.
                    await loadExtensions(m.role);
                    if (!active) return;
                    setMe(m);
                    setCheckError(null);
                    setStatus('authenticated');
                    void applyLangSetting(m.language);
                    return;
                } catch (e) {
                    if (!active) return;
                    if (e instanceof ApiError && e.status === 401) {
                        setMe(null);
                        setCheckError(null);
                        setStatus('anonymous');
                        return;
                    }
                    if (attempt >= CHECK_RETRY_MS.length) {
                        setCheckError(e);
                        setStatus('error');
                        return;
                    }
                    await wait(CHECK_RETRY_MS[attempt]);
                    if (!active) return;
                }
            }
        };
        void check();
        return () => {
            active = false;
        };
    }, [checkRun]);

    const retryCheck = useCallback(() => {
        setCheckError(null);
        setStatus('loading');
        setCheckRun(n => n + 1);
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
            checkError,
            retryCheck,
            login,
            verify,
            logout,
            updateMe,
            refreshMe,
            langSetting: me ? me.language : anonLang,
            setLanguage,
        }),
        [status, me, checkError, retryCheck, anonLang, login, verify, logout, updateMe, refreshMe, setLanguage]
    );

    return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth(): AuthContextValue {
    const ctx = useContext(AuthContext);
    if (!ctx) throw new Error('useAuth must be used inside AuthProvider');
    return ctx;
}
