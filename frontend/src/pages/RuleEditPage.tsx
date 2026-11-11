import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Link, useNavigate, useParams } from 'react-router-dom';
import { getRule, updateRule, validateRule } from '../api/client';
import { Button, buttonClass } from '../components/Button';
import { Field, inputClass } from '../components/FormControls';
import { RequireAdmin } from '../components/Layout';
import { PageHeader } from '../components/PageHeader';
import { RuleContentEditor } from '../components/RuleContentEditor';
import { Alert, ErrorAlert, Loading } from '../components/StateViews';
import { ValidationResult } from '../components/ValidationResult';
import { useToast } from '../context/ToastContext';
import { useApi } from '../hooks/useApi';
import type { RuleContent } from '../types';
import { errorMessage } from '../utils/format';
import { normalizeRuleContent } from '../utils/ruleContent';

export function RuleEditPage() {
    return (
        <RequireAdmin>
            <RuleEditor />
        </RequireAdmin>
    );
}

function RuleEditor() {
    const { t } = useTranslation();
    const { id = '' } = useParams();
    const navigate = useNavigate();
    const { showToast } = useToast();
    const { data: rule, error, loading, reload } = useApi(() => getRule(id), [id]);
    const [content, setContent] = useState<RuleContent | null>(null);
    const [description, setDescription] = useState('');
    const [comment, setComment] = useState('');
    const [validation, setValidation] = useState<string[] | null>(null);
    const [busy, setBusy] = useState<'validate' | 'save' | null>(null);
    const [saveError, setSaveError] = useState<string | null>(null);

    useEffect(() => {
        if (!rule) return;
        setContent(normalizeRuleContent(rule.content));
        setDescription(rule.description);
    }, [rule]);

    function change(c: RuleContent) {
        setContent(c);
        setValidation(null);
    }

    async function validate() {
        if (!content) return;
        setBusy('validate');
        setSaveError(null);
        try {
            setValidation(await validateRule(content));
        } catch (e) {
            setSaveError(errorMessage(t, e));
        } finally {
            setBusy(null);
        }
    }

    async function save() {
        if (!content || !rule) return;
        setBusy('save');
        setSaveError(null);
        try {
            const errors = await validateRule(content);
            setValidation(errors);
            if (errors.length > 0) return;
            const saved = await updateRule(rule.id, {
                description: description.trim(),
                content,
                comment: comment.trim(),
            });
            showToast(t('rules.saved', { version: saved.latest_version }));
            navigate(`/rules/${rule.id}`);
        } catch (e) {
            setSaveError(errorMessage(t, e));
        } finally {
            setBusy(null);
        }
    }

    const back = `/rules/${id}`;

    return (
        <>
            <PageHeader
                breadcrumb={
                    <span className='flex gap-1'>
                        <Link to='/rules' className='text-accent hover:text-accent-hover hover:underline'>
                            {t('rules.title')}
                        </Link>
                        /
                        <Link to={back} className='text-accent hover:text-accent-hover hover:underline'>
                            {rule?.name ?? id}
                        </Link>
                    </span>
                }
                title={t('rules.editTitle', { name: rule?.name ?? '' })}
                description={rule ? t('rules.editNote', { version: rule.latest_version + 1 }) : undefined}
            />
            <ErrorAlert error={error} onRetry={reload} />
            {loading && !rule ? (
                <Loading />
            ) : (
                content && (
                    <div className='flex flex-col gap-4'>
                        <Field label={t('rules.descriptionLabel')} htmlFor='re-desc'>
                            <textarea
                                id='re-desc'
                                className={inputClass}
                                rows={2}
                                value={description}
                                onChange={e => setDescription(e.target.value)}
                            />
                        </Field>
                        <RuleContentEditor value={content} onChange={change} disabled={busy === 'save'} />
                        <Field label={t('rules.comment')} htmlFor='re-comment' hint={t('rules.commentHint')}>
                            <input
                                id='re-comment'
                                className={inputClass}
                                value={comment}
                                onChange={e => setComment(e.target.value)}
                            />
                        </Field>
                        <ValidationResult errors={validation} />
                        {saveError && <Alert tone='danger'>{saveError}</Alert>}
                        <div className='sticky bottom-0 -mx-4 flex flex-wrap justify-end gap-2 border-t border-line bg-canvas/95 px-4 py-3 backdrop-blur'>
                            <Link to={back} className={buttonClass('secondary')}>
                                {t('common.cancel')}
                            </Link>
                            <Button
                                icon='fact_check'
                                busy={busy === 'validate'}
                                disabled={busy !== null}
                                onClick={() => void validate()}
                            >
                                {t('ruleEditor.validate')}
                            </Button>
                            <Button
                                variant='primary'
                                icon='save'
                                busy={busy === 'save'}
                                disabled={busy !== null}
                                onClick={() => void save()}
                            >
                                {t('common.save')}
                            </Button>
                        </div>
                    </div>
                )
            )}
        </>
    );
}
