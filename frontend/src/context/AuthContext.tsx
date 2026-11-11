import { createContext, useCallback, useContext, useEffect, useMemo, useState, type ReactNode } from 'react';
import { getMe, login as apiLogin, logout as apiLogout, setUnauthorizedHandler } from '../api/client';
import type { Me } from '../types';

type AuthStatus = 'loading' | 'authenticated' | 'anonymous';

interface AuthContextValue {
    status: AuthStatus;
    me: Me | null;
    isAdmin: boolean;
    login: (name: string, password: string) => Promise<void>;
    logout: () => Promise<void>;
}

const AuthContext = createContext<AuthContextValue | null>(null);

/** Holds the signed-in account (GET /auth/me) and drops it on any 401. */
export function AuthProvider({ children }: { children: ReactNode }) {
    const [me, setMe] = useState<Me | null>(null);
    const [status, setStatus] = useState<AuthStatus>('loading');

    useEffect(() => {
        setUnauthorizedHandler(() => {
            setMe(null);
            setStatus('anonymous');
        });
        let active = true;
        getMe()
            .then(m => {
                if (!active) return;
                setMe(m);
                setStatus('authenticated');
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

    const login = useCallback(async (name: string, password: string) => {
        const m = await apiLogin(name, password);
        setMe(m);
        setStatus('authenticated');
    }, []);

    const logout = useCallback(async () => {
        try {
            await apiLogout();
        } finally {
            setMe(null);
            setStatus('anonymous');
        }
    }, []);

    const value = useMemo<AuthContextValue>(
        () => ({ status, me, isAdmin: me?.role === 'admin', login, logout }),
        [status, me, login, logout]
    );

    return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth(): AuthContextValue {
    const ctx = useContext(AuthContext);
    if (!ctx) throw new Error('useAuth must be used inside AuthProvider');
    return ctx;
}
