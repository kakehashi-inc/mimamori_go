import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Link, useNavigate, useParams } from 'react-router-dom';
import { ApiError } from '../../api/client';
import { Button, buttonClass } from '../../components/Button';
import { PageHeader } from '../../components/PageHeader';
import { Alert, ErrorAlert, Loading } from '../../components/StateViews';
import { useToast } from '../../context/ToastContext';
import { useApi } from '../../hooks/useApi';
import type { CommandSetList } from '../../types';
import { errorMessage } from '../../utils/format';
import { codedText, type CodedText } from '../../utils/serverText';
import { createCommandSet, listCommandSets, updateCommandSet, validateCommandSet } from '../api';
import { CommandSetEditor } from '../components/CommandSetEditor';
import {
    draftProblems,
    draftToSpec,
    hostZoneText,
    newSetDraft,
    setDraftFrom,
    type SetDraft,
} from '../components/commandSetDraft';
import { StepUpCancelled, useStepUp } from '../components/StepUp';

const linkClass =
    'rounded text-accent hover:text-accent-hover hover:underline focus:outline-none focus-visible:ring-2 focus-visible:ring-accent';

/** Creates a command set of an agent (no set in the address) or edits one. */
export function CommandSetEditPage() {
    const { t } = useTranslation();
    const { id = '', set } = useParams();
    const { data, error, loading, reload } = useApi(() => listCommandSets(id), [id]);
    const editing = set !== undefined;
    const current = editing ? data?.sets?.find(s => String(s.id) === set) : undefined;
    const back = `/agents/${id}`;

    return (
        <>
            <PageHeader
                breadcrumb={
                    <span className='flex flex-wrap items-center gap-1'>
                        <Link to='/agents' className={linkClass}>
                            {t('agents.title')}
                        </Link>
                        <span aria-hidden='true'>/</span>
                        <Link to={back} className={linkClass}>
                            {data?.agent_name ?? t('agentDetail.title')}
                        </Link>
                    </span>
                }
                title={editing ? t('commandSets.editTitle') : t('commandSets.createTitle')}
                description={current?.name}
            />
            <ErrorAlert error={error} onRetry={reload} />
            {loading && !data ? (
                <Loading />
            ) : data && editing && !current ? (
                <Alert tone='danger'>{t('commandSets.notFound')}</Alert>
            ) : (
                data && <EditForm key={current?.id ?? 'new'} list={data} setId={current?.id} back={back} />
            )}
        </>
    );
}

function EditForm({ list, setId, back }: { list: CommandSetList; setId?: number; back: string }) {
    const { t } = useTranslation();
    const navigate = useNavigate();
    const { showToast } = useToast();
    const stepUp = useStepUp();
    const current = setId !== undefined ? list.sets?.find(s => s.id === setId) : undefined;
    const [draft, setDraft] = useState<SetDraft>(() =>
        current ? setDraftFrom(current) : newSetDraft(list.default_os_user)
    );
    const [problems, setProblems] = useState<CodedText[] | null>(null);
    const [busy, setBusy] = useState<'check' | 'save' | null>(null);
    const [saveError, setSaveError] = useState<string | null>(null);
    const zone = hostZoneText(list.time_zone, list.utc_offset_sec, list.time_zone !== '');

    // A change makes an earlier check result stale.
    useEffect(() => setProblems(null), [draft]);

    // The problems the form finds come first, then those of the server.
    async function check(): Promise<boolean> {
        const local = draftProblems(t, draft).map(text => ({ text }));
        const errors = await validateCommandSet(list.agent_id, draftToSpec(draft), setId);
        const all: CodedText[] = [...local, ...errors];
        setProblems(all);
        return all.length === 0;
    }

    async function runCheck() {
        setBusy('check');
        setSaveError(null);
        try {
            await check();
        } catch (e) {
            setSaveError(errorMessage(t, e));
        } finally {
            setBusy(null);
        }
    }

    async function save() {
        setBusy('save');
        setSaveError(null);
        try {
            if (!(await check())) return;
            const spec = draftToSpec(draft);
            const saved = await stepUp.run(() =>
                setId !== undefined
                    ? updateCommandSet(list.agent_id, setId, spec)
                    : createCommandSet(list.agent_id, spec)
            );
            showToast(t('commandSets.saved', { name: saved.name }), 'success');
            navigate(back);
        } catch (e) {
            if (e instanceof StepUpCancelled) setSaveError(e.message);
            else if (e instanceof ApiError && e.status === 409) setSaveError(t('commandSets.nameTaken'));
            else setSaveError(errorMessage(t, e));
        } finally {
            setBusy(null);
        }
    }

    return (
        <>
            <form
                className='flex max-w-5xl flex-col gap-4'
                onSubmit={e => {
                    e.preventDefault();
                    void save();
                }}
            >
                {current?.imported_from && (
                    <Alert tone='info'>{t('commandSets.importedFrom', { source: current.imported_from })}</Alert>
                )}
                <CommandSetEditor draft={draft} onChange={setDraft} agentUser={list.default_os_user} hostZone={zone} />
                {problems !== null &&
                    (problems.length === 0 ? (
                        <Alert tone='success'>{t('commandSets.valid')}</Alert>
                    ) : (
                        <Alert tone='danger' title={t('commandSets.invalid')}>
                            <ul className='list-disc pl-5'>
                                {problems.map((p, i) => (
                                    <li key={i} className='break-words'>
                                        {codedText(t, p)}
                                    </li>
                                ))}
                            </ul>
                        </Alert>
                    ))}
                {saveError && <Alert tone='danger'>{saveError}</Alert>}
                <div className='sticky bottom-0 flex flex-wrap justify-end gap-2 border-t border-line bg-canvas/90 py-3 backdrop-blur'>
                    <Link to={back} className={buttonClass()}>
                        {t('common.cancel')}
                    </Link>
                    <Button
                        icon='rule'
                        busy={busy === 'check'}
                        disabled={busy !== null}
                        onClick={() => void runCheck()}
                    >
                        {t('commandSets.check')}
                    </Button>
                    <Button type='submit' variant='primary' icon='save' busy={busy === 'save'} disabled={busy !== null}>
                        {t('commandSets.save')}
                    </Button>
                </div>
            </form>
            {stepUp.dialog}
        </>
    );
}
