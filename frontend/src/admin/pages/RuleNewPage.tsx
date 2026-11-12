import { useState, type FormEvent } from 'react';
import { useTranslation } from 'react-i18next';
import { Link, useNavigate } from 'react-router-dom';
import { listTemplates } from '../../api/client';
import { createRule, mergePreview } from '../api';
import { Badge } from '../../components/Badge';
import { Button, buttonClass } from '../../components/Button';
import { checkboxClass, Field, inputClass } from '../../components/FormControls';
import { Card, PageHeader } from '../../components/PageHeader';
import { RuleContentView } from '../../components/RuleContentView';
import { Alert, EmptyState, ErrorAlert, Loading } from '../../components/StateViews';
import { Tabs } from '../../components/Tabs';
import { useToast } from '../../context/ToastContext';
import { useApi } from '../../hooks/useApi';
import type { MergePreview } from '../../types';
import { errorMessage } from '../../utils/format';

type Mode = 'templates' | 'empty';

export function RuleNewPage() {
    const { t } = useTranslation();
    const navigate = useNavigate();
    const { showToast } = useToast();
    const [mode, setMode] = useState<Mode>('templates');
    const [name, setName] = useState('');
    const [description, setDescription] = useState('');
    const [selected, setSelected] = useState<string[]>([]);
    const [preview, setPreview] = useState<MergePreview | null>(null);
    const [choices, setChoices] = useState<Record<string, string>>({});
    const [busy, setBusy] = useState<'preview' | 'create' | null>(null);
    const [error, setError] = useState<string | null>(null);
    const templates = useApi(listTemplates, []);

    function toggle(tplName: string) {
        setPreview(null);
        setSelected(prev => (prev.includes(tplName) ? prev.filter(n => n !== tplName) : [...prev, tplName]));
    }

    async function runPreview() {
        setBusy('preview');
        setError(null);
        try {
            const p = await mergePreview(selected);
            setPreview(p);
            const initial: Record<string, string> = {};
            for (const c of p.conflicts ?? []) {
                if (c.options.length > 0) initial[c.field] = c.options[0].template;
            }
            setChoices(initial);
        } catch (e) {
            setError(errorMessage(t, e));
        } finally {
            setBusy(null);
        }
    }

    async function create(e: FormEvent) {
        e.preventDefault();
        setBusy('create');
        setError(null);
        try {
            if (mode === 'templates') {
                const rule = await createRule({
                    name: name.trim(),
                    description: description.trim(),
                    templates: selected,
                    choices,
                });
                showToast(t('rules.created'));
                navigate(`/rules/${rule.id}`);
            } else {
                const rule = await createRule({ name: name.trim(), description: description.trim() });
                showToast(t('rules.created'));
                navigate(`/rules/${rule.id}/edit`);
            }
        } catch (err) {
            setError(errorMessage(t, err));
        } finally {
            setBusy(null);
        }
    }

    const canCreate = name.trim() !== '' && (mode === 'empty' || preview !== null);

    return (
        <>
            <PageHeader
                breadcrumb={
                    <Link to='/rules' className='text-accent hover:text-accent-hover hover:underline'>
                        {t('rules.title')}
                    </Link>
                }
                title={t('rules.newTitle')}
                description={t('rules.newDescription')}
            />
            <div className='flex flex-col gap-4'>
                <Tabs
                    value={mode}
                    onChange={m => {
                        setMode(m);
                        setError(null);
                    }}
                    items={[
                        { value: 'templates', label: t('rules.fromTemplates'), icon: 'library_books' },
                        { value: 'empty', label: t('rules.fromEmpty'), icon: 'note_add' },
                    ]}
                />

                {mode === 'templates' && (
                    <>
                        <Card title={t('rules.stepTemplates')}>
                            <p className='mb-3 text-xs text-muted'>{t('rules.templatesOrderHint')}</p>
                            <ErrorAlert error={templates.error} onRetry={templates.reload} />
                            {templates.loading && !templates.data ? (
                                <Loading />
                            ) : templates.data && templates.data.length === 0 ? (
                                <EmptyState icon='library_books'>{t('templates.empty')}</EmptyState>
                            ) : (
                                <ul className='grid grid-cols-1 gap-2 md:grid-cols-2'>
                                    {(templates.data ?? []).map(tpl => {
                                        const order = selected.indexOf(tpl.name);
                                        return (
                                            <li key={tpl.name}>
                                                <label
                                                    className={`flex cursor-pointer items-start gap-3 rounded-lg border px-3 py-2 transition-colors hover:bg-canvas ${
                                                        order >= 0 ? 'border-accent bg-accent-soft' : 'border-line'
                                                    }`}
                                                >
                                                    <input
                                                        type='checkbox'
                                                        className={`${checkboxClass} mt-0.5`}
                                                        checked={order >= 0}
                                                        onChange={() => toggle(tpl.name)}
                                                    />
                                                    <span className='min-w-0 flex-1'>
                                                        <span className='flex items-center gap-2 font-medium'>
                                                            {tpl.name}
                                                            {tpl.builtin && <Badge>{t('templates.builtin')}</Badge>}
                                                        </span>
                                                        <span className='block text-xs wrap-anywhere text-muted'>
                                                            {tpl.description || '-'}
                                                        </span>
                                                    </span>
                                                    {order >= 0 && (
                                                        <span className='flex h-6 w-6 items-center justify-center rounded-full bg-accent text-xs font-semibold text-accent-contrast'>
                                                            {order + 1}
                                                        </span>
                                                    )}
                                                </label>
                                            </li>
                                        );
                                    })}
                                </ul>
                            )}
                            <div className='mt-4'>
                                <Button
                                    icon='merge'
                                    disabled={selected.length === 0 || busy !== null}
                                    busy={busy === 'preview'}
                                    onClick={() => void runPreview()}
                                >
                                    {t('rules.mergePreview')}
                                </Button>
                            </div>
                        </Card>

                        {preview && (
                            <>
                                {(preview.conflicts ?? []).length > 0 && (
                                    <Card title={t('rules.conflicts')}>
                                        <p className='mb-3 text-xs text-muted'>{t('rules.conflictsHint')}</p>
                                        <div className='flex flex-col gap-4'>
                                            {(preview.conflicts ?? []).map(c => (
                                                <fieldset key={c.field} className='rounded-lg border border-line p-3'>
                                                    <legend className='px-1 font-mono text-xs font-semibold'>
                                                        {c.field}
                                                    </legend>
                                                    <div className='flex flex-col gap-2'>
                                                        {c.options.map(o => (
                                                            <label
                                                                key={o.template}
                                                                className='flex cursor-pointer items-start gap-2 text-sm'
                                                            >
                                                                <input
                                                                    type='radio'
                                                                    name={`conflict-${c.field}`}
                                                                    className='mt-1 accent-accent'
                                                                    checked={choices[c.field] === o.template}
                                                                    onChange={() =>
                                                                        setChoices(prev => ({
                                                                            ...prev,
                                                                            [c.field]: o.template,
                                                                        }))
                                                                    }
                                                                />
                                                                <span className='font-medium'>{o.template}</span>
                                                                <code className='rounded bg-canvas px-1 font-mono text-xs break-all'>
                                                                    {JSON.stringify(o.value)}
                                                                </code>
                                                            </label>
                                                        ))}
                                                    </div>
                                                </fieldset>
                                            ))}
                                        </div>
                                    </Card>
                                )}
                                <Card title={t('rules.mergedContent')}>
                                    {(preview.conflicts ?? []).length > 0 && (
                                        <p className='mb-3 text-xs text-muted'>{t('rules.mergedContentNote')}</p>
                                    )}
                                    <RuleContentView content={preview.content} />
                                </Card>
                            </>
                        )}
                    </>
                )}

                {mode === 'empty' && <Alert tone='info'>{t('rules.fromEmptyNote')}</Alert>}

                <Card title={t('rules.stepName')}>
                    <form id='create-rule-form' onSubmit={e => void create(e)} className='flex flex-col gap-4'>
                        <Field label={t('rules.name')} htmlFor='nr-name' required>
                            <input
                                id='nr-name'
                                className={inputClass}
                                value={name}
                                required
                                onChange={e => setName(e.target.value)}
                            />
                        </Field>
                        <Field label={t('rules.descriptionLabel')} htmlFor='nr-desc'>
                            <textarea
                                id='nr-desc'
                                className={inputClass}
                                rows={2}
                                value={description}
                                onChange={e => setDescription(e.target.value)}
                            />
                        </Field>
                        {error && <Alert tone='danger'>{error}</Alert>}
                        <div className='flex justify-end gap-2'>
                            <Link to='/rules' className={buttonClass('secondary')}>
                                {t('common.cancel')}
                            </Link>
                            <Button
                                type='submit'
                                variant='primary'
                                icon='add'
                                disabled={!canCreate || busy !== null}
                                busy={busy === 'create'}
                            >
                                {mode === 'empty' ? t('rules.createAndEdit') : t('rules.create')}
                            </Button>
                        </div>
                        {mode === 'templates' && !preview && (
                            <p className='text-right text-xs text-muted'>{t('rules.previewFirst')}</p>
                        )}
                    </form>
                </Card>
            </div>
        </>
    );
}
