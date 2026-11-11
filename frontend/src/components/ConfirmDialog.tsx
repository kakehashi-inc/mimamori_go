import { useEffect, useState, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { errorMessage } from '../utils/format';
import { Button } from './Button';
import { Modal } from './Modal';
import { Alert } from './StateViews';

interface ConfirmDialogProps {
    open: boolean;
    title: ReactNode;
    message: ReactNode;
    confirmLabel?: string;
    danger?: boolean;
    /** Runs on confirm; a thrown error is shown in the dialog, which stays open. */
    onConfirm: () => Promise<unknown>;
    onClose: () => void;
    /** Overrides the text shown for some errors, e.g. a 409. */
    mapError?: (e: unknown) => string | null;
}

/** Asks before a destructive or important action. */
export function ConfirmDialog({
    open,
    title,
    message,
    confirmLabel,
    danger,
    onConfirm,
    onClose,
    mapError,
}: ConfirmDialogProps) {
    const { t } = useTranslation();
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState<string | null>(null);

    useEffect(() => {
        if (open) setError(null);
    }, [open]);

    async function confirm() {
        setBusy(true);
        setError(null);
        try {
            await onConfirm();
            onClose();
        } catch (e) {
            setError(mapError?.(e) ?? errorMessage(t, e));
        } finally {
            setBusy(false);
        }
    }

    return (
        <Modal
            open={open}
            title={title}
            onClose={onClose}
            size='sm'
            locked={busy}
            footer={
                <>
                    <Button onClick={onClose} disabled={busy}>
                        {t('common.cancel')}
                    </Button>
                    <Button
                        variant={danger ? 'danger' : 'primary'}
                        busy={busy}
                        onClick={() => void confirm()}
                        data-autofocus
                    >
                        {confirmLabel ?? t('common.ok')}
                    </Button>
                </>
            }
        >
            <div className='flex flex-col gap-3'>
                <div className='leading-relaxed'>{message}</div>
                {error && <Alert tone='danger'>{error}</Alert>}
            </div>
        </Modal>
    );
}
