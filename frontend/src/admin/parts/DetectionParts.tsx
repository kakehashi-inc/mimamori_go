import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '../../components/Button';
import { checkboxClass } from '../../components/FormControls';
import { ErrorAlert } from '../../components/StateViews';
import { Td, Th } from '../../components/Table';
import { useToast } from '../../context/ToastContext';
import { rowKey } from '../../pages/DetectionsPage';
import type { Detection } from '../../types';
import { ackDetections } from '../api';
import { SuppressionFormModal, type SuppressionPrefill } from '../components/SuppressionFormModal';

// Parts placed in the detection list (slots detections.*): selection,
// acknowledgement and turning a detection into a suppression.

type Selection = {
    selected: Set<string>;
    setSelected: (s: Set<string>) => void;
};

function selectable(items: Detection[]): Detection[] {
    return items.filter(d => !d.acknowledged_at);
}

/** Button that acknowledges the selected detections. */
export function AckAction({
    items,
    selected,
    setSelected,
    reload,
}: Selection & { items: Detection[]; reload: () => void }) {
    const { t } = useTranslation();
    const { showToast } = useToast();
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState<unknown>(null);

    async function acknowledge() {
        const targets = items.filter(d => selected.has(rowKey(d))).map(d => ({ agent_id: d.agent_id, id: d.id }));
        if (targets.length === 0) return;
        setBusy(true);
        setError(null);
        try {
            const r = await ackDetections(targets);
            showToast(t('detections.acked', { count: r.acknowledged }));
            setSelected(new Set());
            reload();
        } catch (e) {
            setError(e);
        } finally {
            setBusy(false);
        }
    }

    return (
        <>
            <Button
                variant='primary'
                icon='done_all'
                disabled={selected.size === 0}
                busy={busy}
                onClick={() => void acknowledge()}
            >
                {t('detections.ack')}
                {selected.size > 0 && ` (${selected.size})`}
            </Button>
            {error ? <ErrorAlert error={error} /> : null}
        </>
    );
}

/** Heading cell with the "select all" check box. */
export function SelectAllHead({ items, selected, setSelected }: Selection & { items: Detection[] }) {
    const { t } = useTranslation();
    const list = selectable(items);
    const allSelected = list.length > 0 && list.every(d => selected.has(rowKey(d)));
    return (
        <Th className='w-8'>
            <input
                type='checkbox'
                className={checkboxClass}
                checked={allSelected}
                disabled={list.length === 0}
                onChange={() => setSelected(allSelected ? new Set() : new Set(list.map(rowKey)))}
                aria-label={t('detections.selectAll')}
            />
        </Th>
    );
}

/** Check box of a row. */
export function SelectRow({ detection: d, selected, setSelected }: Selection & { detection: Detection }) {
    const { t } = useTranslation();
    const k = rowKey(d);
    function toggle() {
        const next = new Set(selected);
        if (next.has(k)) next.delete(k);
        else next.add(k);
        setSelected(next);
    }
    return (
        <Td>
            <input
                type='checkbox'
                className={checkboxClass}
                checked={selected.has(k)}
                disabled={!!d.acknowledged_at}
                onChange={toggle}
                aria-label={t('detections.select')}
            />
        </Td>
    );
}

/** Empty heading cell above the per-row action. */
export function EmptyHead() {
    return <Th />;
}

/** Button that opens the suppression form prefilled from the detection. */
export function ToSuppressionAction({ detection: d, reload }: { detection: Detection; reload: () => void }) {
    const { t } = useTranslation();
    const { showToast } = useToast();
    const [prefill, setPrefill] = useState<SuppressionPrefill | null>(null);
    return (
        <Td>
            <Button
                size='sm'
                icon='block'
                onClick={() =>
                    setPrefill({
                        agent_id: d.agent_id,
                        agent_name: d.agent_name,
                        type: d.type,
                        os_user: d.os_user,
                        exe: d.exe,
                        target: d.target,
                    })
                }
            >
                {t('detections.toSuppression')}
            </Button>
            <SuppressionFormModal
                open={prefill !== null}
                prefill={prefill}
                onClose={() => setPrefill(null)}
                onCreated={() => {
                    showToast(t('suppressions.created'));
                    reload();
                }}
            />
        </Td>
    );
}
