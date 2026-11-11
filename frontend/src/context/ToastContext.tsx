import { createContext, useCallback, useContext, useMemo, useRef, useState, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';

type ToastTone = 'success' | 'error' | 'info';

interface ToastItem {
    id: number;
    tone: ToastTone;
    message: string;
}

interface ToastContextValue {
    showToast: (message: string, tone?: ToastTone) => void;
}

const ToastContext = createContext<ToastContextValue | null>(null);

// How long a toast stays on screen.
const TOAST_MS = 4000;

const toneClass: Record<ToastTone, string> = {
    success: 'border-success/40 text-success',
    error: 'border-danger/40 text-danger',
    info: 'border-line text-ink',
};

const toneIcon: Record<ToastTone, string> = {
    success: 'check_circle',
    error: 'error',
    info: 'info',
};

/** Short, self-dismissing notifications shown in the bottom right corner. */
export function ToastProvider({ children }: { children: ReactNode }) {
    const { t } = useTranslation();
    const [items, setItems] = useState<ToastItem[]>([]);
    const nextId = useRef(1);

    const dismiss = useCallback((id: number) => {
        setItems(list => list.filter(i => i.id !== id));
    }, []);

    const showToast = useCallback(
        (message: string, tone: ToastTone = 'success') => {
            const id = nextId.current++;
            setItems(list => [...list, { id, tone, message }]);
            window.setTimeout(() => dismiss(id), TOAST_MS);
        },
        [dismiss]
    );

    const value = useMemo(() => ({ showToast }), [showToast]);

    return (
        <ToastContext.Provider value={value}>
            {children}
            <div
                className='pointer-events-none fixed right-4 bottom-4 z-[60] flex w-[calc(100%-2rem)] max-w-sm flex-col gap-2'
                aria-live='polite'
            >
                {items.map(item => (
                    <div
                        key={item.id}
                        role='status'
                        className={`pointer-events-auto flex items-start gap-2 rounded-lg border bg-surface px-4 py-3 text-sm shadow-lg ${toneClass[item.tone]}`}
                    >
                        <span className='material-icons-outlined text-lg' aria-hidden='true'>
                            {toneIcon[item.tone]}
                        </span>
                        <span className='flex-1 break-words text-ink'>{item.message}</span>
                        <button
                            type='button'
                            onClick={() => dismiss(item.id)}
                            className='rounded text-muted hover:text-ink focus:outline-none focus-visible:ring-2 focus-visible:ring-accent'
                            aria-label={t('common.close')}
                        >
                            <span className='material-icons-outlined text-base' aria-hidden='true'>
                                close
                            </span>
                        </button>
                    </div>
                ))}
            </div>
        </ToastContext.Provider>
    );
}

export function useToast(): ToastContextValue {
    const ctx = useContext(ToastContext);
    if (!ctx) throw new Error('useToast must be used inside ToastProvider');
    return ctx;
}
