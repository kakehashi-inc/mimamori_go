import { useEffect, useRef, type ReactNode } from 'react';
import { createPortal } from 'react-dom';
import { useTranslation } from 'react-i18next';

interface ModalProps {
    open: boolean;
    title: ReactNode;
    onClose: () => void;
    children: ReactNode;
    /** Buttons shown at the bottom right. */
    footer?: ReactNode;
    size?: 'sm' | 'md' | 'lg' | 'xl';
    /** Prevents closing with Escape or the backdrop, e.g. while saving. */
    locked?: boolean;
}

const widths = {
    sm: 'max-w-md',
    md: 'max-w-lg',
    lg: 'max-w-2xl',
    xl: 'max-w-4xl',
};

/** A centered dialog over a dimmed backdrop. */
export function Modal({ open, title, onClose, children, footer, size = 'md', locked }: ModalProps) {
    const { t } = useTranslation();
    const panelRef = useRef<HTMLDivElement>(null);

    useEffect(() => {
        if (!open) return;
        function handleKey(e: KeyboardEvent) {
            if (e.key === 'Escape' && !locked) onClose();
        }
        document.addEventListener('keydown', handleKey);
        return () => document.removeEventListener('keydown', handleKey);
    }, [open, locked, onClose]);

    useEffect(() => {
        if (!open) return;
        // Move focus into the dialog: the first form control, or the panel itself.
        const panel = panelRef.current;
        const first = panel?.querySelector<HTMLElement>('input, select, textarea, [data-autofocus]');
        (first ?? panel)?.focus();
        const previous = document.body.style.overflow;
        document.body.style.overflow = 'hidden';
        return () => {
            document.body.style.overflow = previous;
        };
    }, [open]);

    if (!open) return null;

    return createPortal(
        <div className='fixed inset-0 z-50 flex items-start justify-center overflow-y-auto bg-black/40 p-4 sm:items-center'>
            <div className='fixed inset-0' aria-hidden='true' onClick={() => !locked && onClose()} />
            <div
                ref={panelRef}
                role='dialog'
                aria-modal='true'
                tabIndex={-1}
                className={`relative my-8 flex w-full ${widths[size]} flex-col rounded-xl border border-line bg-surface shadow-2xl focus:outline-none`}
            >
                <div className='flex items-center justify-between gap-4 border-b border-line px-5 py-3'>
                    <h2 className='text-base font-semibold text-ink'>{title}</h2>
                    <button
                        type='button'
                        onClick={onClose}
                        disabled={locked}
                        aria-label={t('common.close')}
                        className='flex h-8 w-8 items-center justify-center rounded-md text-muted hover:bg-line hover:text-ink focus:outline-none focus-visible:ring-2 focus-visible:ring-accent disabled:opacity-50'
                    >
                        <span className='material-icons-outlined text-xl' aria-hidden='true'>
                            close
                        </span>
                    </button>
                </div>
                <div className='max-h-[70vh] overflow-y-auto px-5 py-4 text-sm text-ink'>{children}</div>
                {footer && (
                    <div className='flex flex-wrap justify-end gap-2 border-t border-line px-5 py-3'>{footer}</div>
                )}
            </div>
        </div>,
        document.body
    );
}
