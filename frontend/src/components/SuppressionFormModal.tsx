import { useEffect, useState, type FormEvent } from 'react';
import { useTranslation } from 'react-i18next';
import { createSuppression, listAgents } from '../api/client';
import type { AgentSummary, DetectionType, Suppression } from '../types';
import { errorMessage } from '../utils/format';
import { DETECTION_TYPES } from '../utils/ruleContent';
import { Button } from './Button';
import { checkboxClass, Field, inputClass } from './FormControls';
import { Modal } from './Modal';
import { Alert } from './StateViews';

/** Values copied from a detection when registering it as a suppression. */
export interface SuppressionPrefill {
    agent_id: number;
    agent_name: string;
    type: DetectionType;
    os_user: string;
    exe: string;
    target: string;
}

interface SuppressionFormModalProps {
    open: boolean;
    onClose: () => void;
    onCreated: (s: Suppression) => void;
    /** When set, the form starts from a detection and offers "this agent only". */
    prefill?: SuppressionPrefill | null;
}

/** Form for POST /suppressions, blank or prefilled from a detection. */
export function SuppressionFormModal({ open, onClose, onCreated, prefill }: SuppressionFormModalProps) {
    const { t } = useTranslation();
    const [agents, setAgents] = useState<AgentSummary[]>([]);
    const [agentId, setAgentId] = useState('');
    const [thisAgentOnly, setThisAgentOnly] = useState(true);
    const [type, setType] = useState<DetectionType | ''>('');
    const [osUser, setOsUser] = useState('');
    const [exe, setExe] = useState('');
    const [target, setTarget] = useState('');
    const [comment, setComment] = useState('');
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState<string | null>(null);

    useEffect(() => {
        if (!open) return;
        setError(null);
        setComment('');
        setThisAgentOnly(true);
        setAgentId('');
        setType(prefill?.type ?? '');
        setOsUser(prefill?.os_user ?? '');
        setExe(prefill?.exe ?? '');
        setTarget(prefill?.target ?? '');
        if (!prefill) {
            listAgents()
                .then(setAgents)
                .catch(() => setAgents([]));
        }
    }, [open, prefill]);

    async function submit(e: FormEvent) {
        e.preventDefault();
        setBusy(true);
        setError(null);
        try {
            let agent: number | undefined;
            if (prefill) agent = thisAgentOnly ? prefill.agent_id : undefined;
            else agent = agentId ? Number(agentId) : undefined;
            const s = await createSuppression({
                agent_id: agent,
                type,
                os_user: osUser.trim(),
                exe: exe.trim(),
                target: target.trim(),
                comment: comment.trim(),
            });
            onCreated(s);
            onClose();
        } catch (err) {
            setError(errorMessage(t, err));
        } finally {
            setBusy(false);
        }
    }

    return (
        <Modal
            open={open}
            title={t('suppressions.createTitle')}
            onClose={onClose}
            size='lg'
            locked={busy}
            footer={
                <>
                    <Button onClick={onClose} disabled={busy}>
                        {t('common.cancel')}
                    </Button>
                    <Button variant='primary' type='submit' form='suppression-form' busy={busy} icon='block'>
                        {t('suppressions.create')}
                    </Button>
                </>
            }
        >
            <form id='suppression-form' onSubmit={e => void submit(e)} className='flex flex-col gap-4'>
                <p className='text-xs text-muted'>{t('suppressions.formHint')}</p>
                {prefill ? (
                    <label className='flex items-center gap-2 text-sm'>
                        <input
                            type='checkbox'
                            className={checkboxClass}
                            checked={thisAgentOnly}
                            onChange={e => setThisAgentOnly(e.target.checked)}
                        />
                        {t('suppressions.thisAgentOnly', { name: prefill.agent_name })}
                    </label>
                ) : (
                    <Field label={t('suppressions.agent')} htmlFor='sup-agent'>
                        <select
                            id='sup-agent'
                            className={inputClass}
                            value={agentId}
                            onChange={e => setAgentId(e.target.value)}
                        >
                            <option value=''>{t('suppressions.allAgents')}</option>
                            {agents.map(a => (
                                <option key={a.id} value={a.id}>
                                    {a.display_name} ({a.hostname || '-'})
                                </option>
                            ))}
                        </select>
                    </Field>
                )}
                <Field label={t('detections.type')} htmlFor='sup-type'>
                    <select
                        id='sup-type'
                        className={inputClass}
                        value={type}
                        onChange={e => setType(e.target.value as DetectionType | '')}
                    >
                        <option value=''>{t('suppressions.any')}</option>
                        {DETECTION_TYPES.map(dt => (
                            <option key={dt} value={dt}>
                                {t(`detectionType.${dt}`)}
                            </option>
                        ))}
                    </select>
                </Field>
                <Field label={t('detections.osUser')} htmlFor='sup-user' hint={t('suppressions.emptyIsAny')}>
                    <input
                        id='sup-user'
                        className={inputClass}
                        value={osUser}
                        onChange={e => setOsUser(e.target.value)}
                    />
                </Field>
                <Field label={t('detections.exe')} htmlFor='sup-exe' hint={t('suppressions.wildcardHint')}>
                    <input
                        id='sup-exe'
                        className={`${inputClass} font-mono`}
                        value={exe}
                        onChange={e => setExe(e.target.value)}
                    />
                </Field>
                <Field label={t('detections.target')} htmlFor='sup-target' hint={t('suppressions.wildcardHint')}>
                    <input
                        id='sup-target'
                        className={`${inputClass} font-mono`}
                        value={target}
                        onChange={e => setTarget(e.target.value)}
                    />
                </Field>
                <Field label={t('suppressions.comment')} htmlFor='sup-comment'>
                    <textarea
                        id='sup-comment'
                        className={inputClass}
                        rows={2}
                        value={comment}
                        onChange={e => setComment(e.target.value)}
                    />
                </Field>
                {error && <Alert tone='danger'>{error}</Alert>}
            </form>
        </Modal>
    );
}
