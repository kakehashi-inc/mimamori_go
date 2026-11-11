import { useCallback, useEffect, useState, type DependencyList } from 'react';

export interface ApiState<T> {
    data: T | null;
    error: unknown;
    loading: boolean;
    /** Runs the request again, keeping the current data until it answers. */
    reload: () => void;
    setData: (data: T | null) => void;
}

/**
 * Runs `fn` on mount and whenever `deps` change. Results of a request that
 * finished after a newer one started are discarded.
 */
export function useApi<T>(fn: () => Promise<T>, deps: DependencyList): ApiState<T> {
    const [data, setData] = useState<T | null>(null);
    const [error, setError] = useState<unknown>(null);
    const [loading, setLoading] = useState(true);
    const [nonce, setNonce] = useState(0);

    useEffect(() => {
        let active = true;
        setLoading(true);
        fn()
            .then(v => {
                if (!active) return;
                setData(v);
                setError(null);
            })
            .catch((e: unknown) => {
                if (active) setError(e);
            })
            .finally(() => {
                if (active) setLoading(false);
            });
        return () => {
            active = false;
        };
        // fn is recreated on every render; deps decide when to refetch.
    }, [...deps, nonce]);

    const reload = useCallback(() => setNonce(n => n + 1), []);

    return { data, error, loading, reload, setData };
}
