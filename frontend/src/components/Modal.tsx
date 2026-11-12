import { useEffect, useId, useRef, type ReactNode } from 'react';
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

const FOCUSABLE =
    'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';

/** A centered dialog over a dimmed backdrop. */
export function Modal({ open, title, onClose, children, footer, size = 'md', locked }: ModalProps) {
    const { t } = useTranslation();
    const panelRef = useRef<HTMLDivElement>(null);
    const titleId = useId();

    useEffect(() => {
        if (!open) return;
        function handleKey(e: KeyboardEvent) {
            if (e.key === 'Escape' && !locked) onClose();
            if (e.key === 'Tab') keepFocusInside(e);
        }
        // Tab and Shift+Tab cycle through the controls of the topmost dialog
        // instead of leaving it for the page behind.
        function keepFocusInside(e: KeyboardEvent) {
            const panel = panelRef.current;
            const dialogs = document.querySelectorAll('[role=dialog][aria-modal=true]');
            if (!panel || dialogs[dialogs.length - 1] !== panel) return;
            const items = [...panel.querySelectorAll<HTMLElement>(FOCUSABLE)].filter(
                el => el.getClientRects().length > 0
            );
            if (items.length === 0) {
                e.preventDefault();
                panel.focus();
                return;
            }
            const first = items[0];
            const last = items[items.length - 1];
            const active = document.activeElement;
            if (e.shiftKey && (active === first || active === panel || !panel.contains(active))) {
                e.preventDefault();
                last.focus();
            } else if (!e.shiftKey && (active === last || !panel.contains(active))) {
                e.preventDefault();
                first.focus();
            }
        }
        document.addEventListener('keydown', handleKey);
        return () => document.removeEventListener('keydown', handleKey);
    }, [open, locked, onClose]);

    useEffect(() => {
        if (!open) return;
        // Focus goes back to where it was (usually the button that opened the dialog) on close.
        const opener = document.activeElement instanceof HTMLElement ? document.activeElement : null;
        // Move focus into the dialog: the first form control, or the panel itself.
        const panel = panelRef.current;
        const first = panel?.querySelector<HTMLElement>('input, select, textarea, [data-autofocus]');
        (first ?? panel)?.focus();
        const previous = document.body.style.overflow;
        document.body.style.overflow = 'hidden';
        return () => {
            document.body.style.overflow = previous;
            if (opener?.isConnected) opener.focus();
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
                aria-labelledby={titleId}
                tabIndex={-1}
                className={`relative my-8 flex w-full ${widths[size]} flex-col rounded-xl border border-line bg-surface shadow-2xl focus:outline-none`}
            >
                <div className='flex items-center justify-between gap-4 border-b border-line px-5 py-3'>
                    <h2 id={titleId} className='text-base font-semibold text-ink'>
                        {title}
                    </h2>
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
