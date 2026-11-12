import { useEffect, useState, type FormEvent } from 'react';
import { useTranslation } from 'react-i18next';
import { useNavigate } from 'react-router-dom';
import { getRule, listRules } from '../../api/client';
import { Button } from '../../components/Button';
import { ConfirmDialog } from '../../components/ConfirmDialog';
import { Field, inputClass } from '../../components/FormControls';
import { Modal } from '../../components/Modal';
import { Alert } from '../../components/StateViews';
import { useToast } from '../../context/ToastContext';
import type {
    RuleContent,
    RuleSummary,
    RuleVersionInfo,
    TemplateCreate,
    TemplateDetail,
    TemplateSummary,
} from '../../types';
import { errorMessage } from '../../utils/format';
import { normalizeRuleContent } from '../../utils/ruleContent';
import { createTemplate, deleteTemplate, updateTemplate, validateRule } from '../api';
import { RuleContentEditor } from '../components/RuleContentEditor';
import { ValidationResult } from '../components/ValidationResult';

// Parts placed in the template list and the template page (slots templates.*
// and templateDetail.*).

/** Header button that creates a template, with its form. */
export function CreateTemplateAction({ templates }: { templates: TemplateSummary[] }) {
    const { t } = useTranslation();
    const { showToast } = useToast();
    const navigate = useNavigate();
    const [open, setOpen] = useState(false);
    return (
        <>
            <Button variant='primary' icon='add' onClick={() => setOpen(true)}>
                {t('templates.create')}
            </Button>
            <CreateTemplateModal
                open={open}
                templates={templates}
                onClose={() => setOpen(false)}
                onCreated={name => {
                    showToast(t('templates.created'));
                    navigate(`/templates/${encodeURIComponent(name)}`);
                }}
            />
        </>
    );
}

/** Tells administrators that a built-in template is changed by copying it. */
export function BuiltinTemplateNotice({ template }: { template: TemplateDetail }) {
    const { t } = useTranslation();
    if (!template.builtin) return null;
    return <Alert tone='info'>{t('templates.builtinReadOnly')}</Alert>;
}

/** Edit and delete buttons of a template made by an administrator (not built in). */
export function TemplateActions({
    template: tpl,
    panelOpen: editing,
    setPanelOpen: setEditing,
}: {
    template: TemplateDetail;
    panelOpen: boolean;
    setPanelOpen: (v: boolean) => void;
}) {
    const { t } = useTranslation();
    const { showToast } = useToast();
    const navigate = useNavigate();
    const [deleteOpen, setDeleteOpen] = useState(false);
    if (tpl.builtin) return null;
    return (
        <>
            {!editing && (
                <>
                    <Button icon='edit' onClick={() => setEditing(true)}>
                        {t('common.edit')}
                    </Button>
                    <Button icon='delete' variant='danger' onClick={() => setDeleteOpen(true)}>
                        {t('common.delete')}
                    </Button>
                </>
            )}
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
        </>
    );
}

/** Editor of a template's description and content (the page's panel, opened by the edit button). */
export function TemplateEditor({
    template: tpl,
    onClose,
}: {
    template: TemplateDetail;
    onClose: (updated?: TemplateDetail) => void;
}) {
    const { t } = useTranslation();
    const { showToast } = useToast();
    const [content, setContent] = useState<RuleContent>(() => normalizeRuleContent(tpl.content));
    const [description, setDescription] = useState(tpl.description);
    const [validation, setValidation] = useState<string[] | null>(null);
    const [busy, setBusy] = useState(false);
    const [saveError, setSaveError] = useState<string | null>(null);
    // True while the JSON tab holds invalid JSON: content is then stale, so nothing is sent.
    const [jsonInvalid, setJsonInvalid] = useState(false);

    if (tpl.builtin) return null;

    async function save() {
        if (jsonInvalid) return;
        setBusy(true);
        setSaveError(null);
        try {
            const errors = await validateRule(content);
            setValidation(errors);
            if (errors.length > 0) return;
            const saved = await updateTemplate(tpl.name, { description: description.trim(), content });
            onClose(saved);
            showToast(t('common.saved'));
        } catch (e) {
            setSaveError(errorMessage(t, e));
        } finally {
            setBusy(false);
        }
    }

    return (
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
                <Button onClick={() => onClose()} disabled={busy}>
                    {t('common.cancel')}
                </Button>
                <Button variant='primary' icon='save' busy={busy} disabled={jsonInvalid} onClick={() => void save()}>
                    {t('common.save')}
                </Button>
            </div>
        </>
    );
}

type Source = 'empty' | 'builtin' | 'rule';

function CreateTemplateModal({
    open,
    templates,
    onClose,
    onCreated,
}: {
    open: boolean;
    templates: TemplateSummary[];
    onClose: () => void;
    onCreated: (name: string) => void;
}) {
    const { t } = useTranslation();
    const builtins = templates.filter(tpl => tpl.builtin);
    const [name, setName] = useState('');
    const [description, setDescription] = useState('');
    const [source, setSource] = useState<Source>('empty');
    const [builtin, setBuiltin] = useState('');
    const [rules, setRules] = useState<RuleSummary[]>([]);
    const [ruleId, setRuleId] = useState('');
    const [versions, setVersions] = useState<RuleVersionInfo[]>([]);
    const [version, setVersion] = useState('');
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState<string | null>(null);

    useEffect(() => {
        if (!open) return;
        setName('');
        setDescription('');
        setSource('empty');
        setBuiltin('');
        setRuleId('');
        setError(null);
        listRules()
            .then(setRules)
            .catch(() => setRules([]));
    }, [open]);

    useEffect(() => {
        setVersions([]);
        setVersion('');
        if (!ruleId) return;
        let active = true;
        getRule(ruleId)
            .then(r => {
                if (!active) return;
                setVersions([...(r.versions ?? [])].sort((a, b) => b.version - a.version));
                setVersion(String(r.latest_version));
            })
            .catch(() => undefined);
        return () => {
            active = false;
        };
    }, [ruleId]);

    async function submit(e: FormEvent) {
        e.preventDefault();
        const body: TemplateCreate = { name: name.trim(), description: description.trim() };
        if (source === 'builtin') {
            if (!builtin) {
                setError(t('templates.chooseBuiltin'));
                return;
            }
            body.from_builtin = builtin;
        } else if (source === 'rule') {
            if (!ruleId || !version) {
                setError(t('templates.chooseRule'));
                return;
            }
            body.from_rule = { rule_id: Number(ruleId), version: Number(version) };
        }
        setBusy(true);
        setError(null);
        try {
            const tpl = await createTemplate(body);
            onClose();
            onCreated(tpl.name);
        } catch (err) {
            setError(errorMessage(t, err));
        } finally {
            setBusy(false);
        }
    }

    return (
        <Modal
            open={open}
            title={t('templates.createTitle')}
            onClose={onClose}
            locked={busy}
            footer={
                <>
                    <Button onClick={onClose} disabled={busy}>
                        {t('common.cancel')}
                    </Button>
                    <Button variant='primary' type='submit' form='create-template-form' busy={busy} icon='add'>
                        {t('templates.create')}
                    </Button>
                </>
            }
        >
            <form id='create-template-form' onSubmit={e => void submit(e)} className='flex flex-col gap-4'>
                <Field label={t('templates.name')} htmlFor='ct-name' required>
                    <input
                        id='ct-name'
                        className={inputClass}
                        value={name}
                        required
                        onChange={e => setName(e.target.value)}
                    />
                </Field>
                <Field label={t('templates.descriptionLabel')} htmlFor='ct-desc'>
                    <textarea
                        id='ct-desc'
                        className={inputClass}
                        rows={2}
                        value={description}
                        onChange={e => setDescription(e.target.value)}
                    />
                </Field>
                <fieldset className='flex flex-col gap-2'>
                    <legend className='mb-1 text-sm font-medium'>{t('templates.source')}</legend>
                    {(['empty', 'builtin', 'rule'] as Source[]).map(s => (
                        <label key={s} className='flex items-center gap-2 text-sm'>
                            <input
                                type='radio'
                                name='ct-source'
                                className='accent-accent'
                                checked={source === s}
                                onChange={() => setSource(s)}
                            />
                            {t(`templates.source_${s}`)}
                        </label>
                    ))}
                </fieldset>
                {source === 'builtin' && (
                    <Field label={t('templates.builtin')} htmlFor='ct-builtin'>
                        <select
                            id='ct-builtin'
                            className={inputClass}
                            value={builtin}
                            onChange={e => setBuiltin(e.target.value)}
                        >
                            <option value=''>-</option>
                            {builtins.map(b => (
                                <option key={b.name} value={b.name}>
                                    {b.name}
                                </option>
                            ))}
                        </select>
                    </Field>
                )}
                {source === 'rule' && (
                    <div className='grid grid-cols-1 gap-4 sm:grid-cols-2'>
                        <Field label={t('rules.name')} htmlFor='ct-rule'>
                            <select
                                id='ct-rule'
                                className={inputClass}
                                value={ruleId}
                                onChange={e => setRuleId(e.target.value)}
                            >
                                <option value=''>-</option>
                                {rules.map(r => (
                                    <option key={r.id} value={r.id}>
                                        {r.name}
                                    </option>
                                ))}
                            </select>
                        </Field>
                        <Field label={t('rules.version')} htmlFor='ct-version'>
                            <select
                                id='ct-version'
                                className={inputClass}
                                value={version}
                                disabled={!ruleId}
                                onChange={e => setVersion(e.target.value)}
                            >
                                {versions.map(v => (
                                    <option key={v.version} value={v.version}>
                                        v{v.version}
                                    </option>
                                ))}
                            </select>
                        </Field>
                    </div>
                )}
                {error && <Alert tone='danger'>{error}</Alert>}
            </form>
        </Modal>
    );
}
