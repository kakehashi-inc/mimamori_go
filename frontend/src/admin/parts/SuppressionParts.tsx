import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '../../components/Button';
import { ConfirmDialog } from '../../components/ConfirmDialog';
import { Td, Th } from '../../components/Table';
import { useToast } from '../../context/ToastContext';
import type { Suppression } from '../../types';
import { deleteSuppression } from '../api';
import { SuppressionFormModal } from '../components/SuppressionFormModal';

// Parts placed in the suppression list (slots suppressions.*).

/** Header button that creates a suppression, with its form. */
export function CreateSuppressionAction({ reload }: { reload: () => void }) {
    const { t } = useTranslation();
    const { showToast } = useToast();
    const [open, setOpen] = useState(false);
    return (
        <>
            <Button variant='primary' icon='add' onClick={() => setOpen(true)}>
                {t('suppressions.create')}
            </Button>
            <SuppressionFormModal
                open={open}
                onClose={() => setOpen(false)}
                onCreated={() => {
                    showToast(t('suppressions.created'));
                    reload();
                }}
            />
        </>
    );
}

/** Empty heading cell above the per-row action. */
export function SuppressionActionsHead() {
    return <Th />;
}

/** Delete button of a row, with its confirmation. */
export function DeleteSuppressionAction({ suppression, reload }: { suppression: Suppression; reload: () => void }) {
    const { t } = useTranslation();
    const { showToast } = useToast();
    const [open, setOpen] = useState(false);
    return (
        <Td className='text-right'>
            <Button size='sm' variant='danger' icon='delete' onClick={() => setOpen(true)}>
                {t('common.delete')}
            </Button>
            <ConfirmDialog
                open={open}
                title={t('suppressions.deleteTitle')}
                message={t('suppressions.deleteConfirm')}
                confirmLabel={t('common.delete')}
                danger
                onClose={() => setOpen(false)}
                onConfirm={async () => {
                    await deleteSuppression(suppression.id);
                    showToast(t('common.deleted'));
                    reload();
                }}
            />
        </Td>
    );
}
