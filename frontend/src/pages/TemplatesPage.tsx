import { useEffect, useState, type FormEvent } from 'react';
import { useTranslation } from 'react-i18next';
import { Link, useNavigate } from 'react-router-dom';
import { createTemplate, getRule, listRules, listTemplates } from '../api/client';
import { Badge } from '../components/Badge';
import { Button } from '../components/Button';
import { Field, inputClass } from '../components/FormControls';
import { Modal } from '../components/Modal';
import { PageHeader } from '../components/PageHeader';
import { Alert, EmptyState, ErrorAlert, Loading } from '../components/StateViews';
import { Table, TBody, Td, Th, THead, Tr } from '../components/Table';
import { useAuth } from '../context/AuthContext';
import { useToast } from '../context/ToastContext';
import { useApi } from '../hooks/useApi';
import type { RuleSummary, RuleVersionInfo, TemplateCreate, TemplateSummary } from '../types';
import { errorMessage, formatDateTime } from '../utils/format';

export function TemplatesPage() {
    const { t } = useTranslation();
    const { isAdmin } = useAuth();
    const { data, error, loading, reload } = useApi(listTemplates, []);
    const [createOpen, setCreateOpen] = useState(false);
    const navigate = useNavigate();
    const { showToast } = useToast();

    return (
        <>
            <PageHeader
                title={t('templates.title')}
                description={t('templates.description')}
                actions={
                    <>
                        <Button icon='refresh' onClick={reload} busy={loading && !!data}>
                            {t('common.reload')}
                        </Button>
                        {isAdmin && (
                            <Button variant='primary' icon='add' onClick={() => setCreateOpen(true)}>
                                {t('templates.create')}
                            </Button>
                        )}
                    </>
                }
            />
            <div className='flex flex-col gap-4'>
                <ErrorAlert error={error} onRetry={reload} />
                {loading && !data ? (
                    <Loading />
                ) : data && data.length === 0 ? (
                    <EmptyState icon='library_books'>{t('templates.empty')}</EmptyState>
                ) : (
                    data && (
                        <Table>
                            <THead>
                                <Th>{t('templates.name')}</Th>
                                <Th>{t('templates.kind')}</Th>
                                <Th>{t('templates.descriptionLabel')}</Th>
                                <Th>{t('templates.updatedAt')}</Th>
                            </THead>
                            <TBody>
                                {data.map(tpl => (
                                    <Tr key={tpl.name}>
                                        <Td>
                                            <Link
                                                to={`/templates/${encodeURIComponent(tpl.name)}`}
                                                className='font-medium text-accent hover:text-accent-hover hover:underline focus:outline-none focus-visible:ring-2 focus-visible:ring-accent'
                                            >
                                                {tpl.name}
                                            </Link>
                                        </Td>
                                        <Td>
                                            {tpl.builtin ? (
                                                <Badge icon='lock'>{t('templates.builtin')}</Badge>
                                            ) : (
                                                <Badge tone='accent'>{t('templates.custom')}</Badge>
                                            )}
                                        </Td>
                                        <Td className='text-muted'>{tpl.description || '-'}</Td>
                                        <Td className='whitespace-nowrap'>{formatDateTime(tpl.updated_at)}</Td>
                                    </Tr>
                                ))}
                            </TBody>
                        </Table>
                    )
                )}
            </div>
            {isAdmin && (
                <CreateTemplateModal
                    open={createOpen}
                    templates={data ?? []}
                    onClose={() => setCreateOpen(false)}
                    onCreated={name => {
                        showToast(t('templates.created'));
                        navigate(`/templates/${encodeURIComponent(name)}`);
                    }}
                />
            )}
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
