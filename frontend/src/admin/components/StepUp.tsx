import { useCallback, useEffect, useId, useState, type FormEvent, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { ApiError } from '../../api/client';
import { Button } from '../../components/Button';
import { CODE_LENGTH, CodeInput } from '../../components/CodeInput';
import { Field } from '../../components/FormControls';
import { Modal } from '../../components/Modal';
import { Alert, Loading } from '../../components/StateViews';
import i18n from '../../i18n/i18n';
import { errorMessage } from '../../utils/format';
import { requestStepUp, verifyStepUp } from '../api';

// Changes of command sets need a code mailed in this login session within
// the last hour. The server answers such a change with 428 otherwise; the
// dialog here then asks for a mailed code and repeats the change.

/** Thrown when the person closed the code dialog without entering a code. */
export class StepUpCancelled extends Error {
    constructor() {
        super(i18n.t('stepUp.cancelled'));
        this.name = 'StepUpCancelled';
    }
}

interface PendingChange {
    repeat: () => Promise<unknown>;
    resolve: (v: unknown) => void;
    reject: (e: unknown) => void;
}

/**
 * Returns `run`, which runs a change and, when the server asks for
 * re-authentication, shows the code dialog and repeats the change after the
 * code was accepted; and `dialog`, which the caller renders.
 */
export function useStepUp(): { run: <T>(change: () => Promise<T>) => Promise<T>; dialog: ReactNode } {
    const [pending, setPending] = useState<PendingChange | null>(null);

    const run = useCallback(async <T,>(change: () => Promise<T>): Promise<T> => {
        try {
            return await change();
        } catch (e) {
            if (!(e instanceof ApiError && e.status === 428)) throw e;
            return new Promise<T>((resolve, reject) =>
                setPending({ repeat: change, resolve: v => resolve(v as T), reject })
            );
        }
    }, []);

    const end = useCallback(() => setPending(null), []);
    const dialog = <StepUpDialog pending={pending} onEnd={end} />;
    return { run, dialog };
}

type Phase = 'sending' | 'code' | 'failed';

function StepUpDialog({ pending, onEnd }: { pending: PendingChange | null; onEnd: () => void }) {
    const { t } = useTranslation();
    const inputId = useId();
    const [phase, setPhase] = useState<Phase>('sending');
    const [code, setCode] = useState('');
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState<string | null>(null);

    const finish = useCallback(
        async (p: PendingChange) => {
            try {
                p.resolve(await p.repeat());
            } catch (e) {
                p.reject(e);
            }
            onEnd();
        },
        [onEnd]
    );

    const sendCode = useCallback(
        async (p: PendingChange) => {
            setPhase('sending');
            setCode('');
            setError(null);
            try {
                const r = await requestStepUp();
                if (!r.required) {
                    await finish(p);
                    return;
                }
                setPhase('code');
            } catch (e) {
                setError(errorMessage(t, e));
                setPhase('failed');
            }
        },
        [finish, t]
    );

    // A code is mailed once for each change that needs one (not again when
    // the language or the callbacks change).
    useEffect(() => {
        if (pending) void sendCode(pending);
    }, [pending]);

    if (!pending) return null;

    function cancel() {
        pending?.reject(new StepUpCancelled());
        onEnd();
    }

    async function submit(e: FormEvent) {
        // The dialog is rendered in a portal, but React passes its events to
        // the parents of the component, which may be another form.
        e.preventDefault();
        e.stopPropagation();
        if (!pending || code.length !== CODE_LENGTH) return;
        setBusy(true);
        setError(null);
        try {
            await verifyStepUp(code);
        } catch (err) {
            setBusy(false);
            if (err instanceof ApiError && err.status === 400) {
                setError(t('stepUp.invalidCode'));
                setCode('');
            } else if (err instanceof ApiError && err.status === 410) {
                setError(t('stepUp.expired'));
                setPhase('failed');
            } else {
                setError(errorMessage(t, err));
            }
            return;
        }
        await finish(pending);
        setBusy(false);
    }

    return (
        <Modal
            open
            title={t('stepUp.title')}
            onClose={cancel}
            size='sm'
            locked={busy}
            footer={
                <>
                    <Button onClick={cancel} disabled={busy}>
                        {t('common.cancel')}
                    </Button>
                    {phase === 'failed' ? (
                        <Button variant='primary' icon='mail' onClick={() => void sendCode(pending)}>
                            {t('stepUp.resend')}
                        </Button>
                    ) : (
                        <Button
                            type='submit'
                            form={`${inputId}-form`}
                            variant='primary'
                            busy={busy}
                            disabled={phase !== 'code' || code.length !== CODE_LENGTH}
                        >
                            {t('stepUp.confirm')}
                        </Button>
                    )}
                </>
            }
        >
            <form id={`${inputId}-form`} onSubmit={e => void submit(e)} className='flex flex-col gap-3'>
                <p className='leading-relaxed'>{t('stepUp.why')}</p>
                {phase === 'sending' && <Loading label={t('stepUp.sending')} />}
                {phase === 'code' && (
                    <>
                        <Alert tone='info'>{t('stepUp.sent')}</Alert>
                        <Field label={t('stepUp.code')} htmlFor={inputId} hint={t('stepUp.codeHint')}>
                            <CodeInput id={inputId} value={code} onChange={setCode} autoFocus />
                        </Field>
                    </>
                )}
                {error && <Alert tone='danger'>{error}</Alert>}
            </form>
        </Modal>
    );
}
