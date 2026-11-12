import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Link, useNavigate, useParams } from 'react-router-dom';
import { deleteTemplate, getTemplate, updateTemplate, validateRule } from '../api/client';
import { Badge } from '../components/Badge';
import { Button } from '../components/Button';
import { ConfirmDialog } from '../components/ConfirmDialog';
import { Field, inputClass } from '../components/FormControls';
import { PageHeader } from '../components/PageHeader';
import { RuleContentEditor } from '../components/RuleContentEditor';
import { RuleContentView } from '../components/RuleContentView';
import { Alert, ErrorAlert, Loading } from '../components/StateViews';
import { ValidationResult } from '../components/ValidationResult';
import { useAuth } from '../context/AuthContext';
import { useToast } from '../context/ToastContext';
import { useApi } from '../hooks/useApi';
import type { RuleContent } from '../types';
import { errorMessage, formatDateTime } from '../utils/format';
import { normalizeRuleContent } from '../utils/ruleContent';

export function TemplateDetailPage() {
    const { t } = useTranslation();
    const { name = '' } = useParams();
    const navigate = useNavigate();
    const { isAdmin } = useAuth();
    const { showToast } = useToast();
    const { data: tpl, error, loading, reload, setData } = useApi(() => getTemplate(name), [name]);
    const [editing, setEditing] = useState(false);
    const [content, setContent] = useState<RuleContent | null>(null);
    const [description, setDescription] = useState('');
    const [validation, setValidation] = useState<string[] | null>(null);
    const [busy, setBusy] = useState(false);
    const [saveError, setSaveError] = useState<string | null>(null);
    // True while the JSON tab holds invalid JSON: content is then stale, so nothing is sent.
    const [jsonInvalid, setJsonInvalid] = useState(false);
    const [deleteOpen, setDeleteOpen] = useState(false);

    const canEdit = isAdmin && tpl !== null && !tpl.builtin;

    function startEdit() {
        if (!tpl) return;
        setContent(normalizeRuleContent(tpl.content));
        setDescription(tpl.description);
        setValidation(null);
        setSaveError(null);
        setJsonInvalid(false);
        setEditing(true);
    }

    async function save() {
        if (!content || !tpl || jsonInvalid) return;
        setBusy(true);
        setSaveError(null);
        try {
            const errors = await validateRule(content);
            setValidation(errors);
            if (errors.length > 0) return;
            const saved = await updateTemplate(tpl.name, { description: description.trim(), content });
            setData(saved);
            setEditing(false);
            showToast(t('common.saved'));
        } catch (e) {
            setSaveError(errorMessage(t, e));
        } finally {
            setBusy(false);
        }
    }

    return (
        <>
            <PageHeader
                breadcrumb={
                    <Link to='/templates' className='text-accent hover:text-accent-hover hover:underline'>
                        {t('templates.title')}
                    </Link>
                }
                title={
                    <span className='flex flex-wrap items-center gap-2'>
                        {tpl?.name ?? name}
                        {tpl?.builtin && <Badge icon='lock'>{t('templates.builtin')}</Badge>}
                    </span>
                }
                description={
                    !editing && tpl?.description ? <span className='wrap-anywhere'>{tpl.description}</span> : undefined
                }
                actions={
                    canEdit &&
                    !editing && (
                        <>
                            <Button icon='edit' onClick={startEdit}>
                                {t('common.edit')}
                            </Button>
                            <Button icon='delete' variant='danger' onClick={() => setDeleteOpen(true)}>
                                {t('common.delete')}
                            </Button>
                        </>
                    )
                }
            />
            <ErrorAlert error={error} onRetry={reload} />
            {loading && !tpl ? (
                <Loading />
            ) : (
                tpl && (
                    <div className='flex flex-col gap-4'>
                        {tpl.builtin && <Alert tone='info'>{t('templates.builtinReadOnly')}</Alert>}
                        {!editing && (
                            <>
                                <p className='text-xs text-muted'>
                                    {t('templates.updatedAt')}: {formatDateTime(tpl.updated_at)}
                                </p>
                                <RuleContentView content={tpl.content} />
                            </>
                        )}
                        {editing && content && (
                            <>
                                <Field label={t('templates.descriptionLabel')} htmlFor='te-desc'>
                                    <textarea
                                        id='te-desc'
                                        className={inputClass}
                                        rows={2}
                                        value={description}
                                        onChange={e => setDescription(e.target.value)}
                                    />
                                </Field>
                                <RuleContentEditor
                                    value={content}
                                    onChange={c => {
                                        setContent(c);
                                        setValidation(null);
                                    }}
                                    disabled={busy}
                                    onJsonInvalidChange={setJsonInvalid}
                                />
                                <ValidationResult errors={validation} />
                                {saveError && <Alert tone='danger'>{saveError}</Alert>}
                                <div className='sticky bottom-0 -mx-4 flex justify-end gap-2 border-t border-line bg-canvas/95 px-4 py-3 backdrop-blur'>
                                    <Button onClick={() => setEditing(false)} disabled={busy}>
                                        {t('common.cancel')}
                                    </Button>
                                    <Button
                                        variant='primary'
                                        icon='save'
                                        busy={busy}
                                        disabled={jsonInvalid}
                                        onClick={() => void save()}
                                    >
                                        {t('common.save')}
                                    </Button>
                                </div>
                            </>
                        )}
                    </div>
                )
            )}
            {canEdit && tpl && (
                <ConfirmDialog
                    open={deleteOpen}
                    title={t('templates.deleteTitle')}
                    message={t('templates.deleteConfirm', { name: tpl.name })}
                    confirmLabel={t('common.delete')}
                    danger
                    onClose={() => setDeleteOpen(false)}
                    onConfirm={async () => {
                        await deleteTemplate(tpl.name);
                        showToast(t('common.deleted'));
                        navigate('/templates', { replace: true });
                    }}
                />
            )}
        </>
    );
}
