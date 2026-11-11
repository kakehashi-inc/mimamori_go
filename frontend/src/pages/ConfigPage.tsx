import { useEffect, useMemo, useState, type FormEvent } from 'react';
import { useTranslation } from 'react-i18next';
import { getConfig, updateConfig } from '../api/client';
import { Badge } from '../components/Badge';
import { Button } from '../components/Button';
import { checkboxClass, Field, inputClass } from '../components/FormControls';
import { Card, PageHeader } from '../components/PageHeader';
import { Alert, ErrorAlert, Loading } from '../components/StateViews';
import { useToast } from '../context/ToastContext';
import type ja from '../i18n/ja.json';
import { useApi } from '../hooks/useApi';
import type { CentralConfig, ConfigItem } from '../types';
import { errorMessage } from '../utils/format';

// Items grouped by subject, in display order. Items the server reports but
// that are not listed here are shown in the last group.
type GroupKey = keyof typeof ja.config.groups;
type ItemKey = keyof typeof ja.config.items;

const GROUPS: { key: GroupKey; items: string[] }[] = [
    {
        key: 'server',
        items: ['web_listen', 'agent_listen', 'server_names', 'workers', 'queue_timeout_sec', 'builtin_agent'],
    },
    {
        key: 'agents',
        items: ['token_valid_days', 'token_renew_before_days', 'default_check_interval_sec', 'retention_days'],
    },
    { key: 'sessions', items: ['session_hours', 'session_remember_days'] },
    {
        key: 'notify',
        items: ['notify.min_interval_min', 'notify.dedupe_hours', 'notify.max_lines', 'notify.subject_prefix'],
    },
    {
        key: 'smtp',
        items: [
            'smtp.host',
            'smtp.port',
            'smtp.security',
            'smtp.auth',
            'smtp.username',
            'smtp.password',
            'smtp.from',
            'smtp.to',
            'smtp.timeout_sec',
            'smtp.helo_name',
        ],
    },
];

const SMTP_SECURITY = ['starttls', 'tls', 'none'] as const;

// i18n keys cannot contain the dots of the item names.
const itemKey = (name: string) => name.replace(/\./g, '_') as ItemKey;

interface Draft {
    values: Record<string, string>;
    resets: Set<string>;
    password: string;
    clearPassword: boolean;
}

function initialDraft(config: CentralConfig): Draft {
    const values: Record<string, string> = {};
    for (const item of config.items) values[item.name] = item.value;
    return { values, resets: new Set(), password: '', clearPassword: false };
}

export function ConfigPage() {
    const { t } = useTranslation();
    const { showToast } = useToast();
    const { data, error, loading, reload, setData } = useApi(getConfig, []);
    const [draft, setDraft] = useState<Draft | null>(null);
    const [busy, setBusy] = useState(false);
    const [saveError, setSaveError] = useState<string | null>(null);

    useEffect(() => {
        if (data) setDraft(initialDraft(data));
    }, [data]);

    const byName = useMemo(() => new Map((data?.items ?? []).map(i => [i.name, i])), [data]);
    const groups = useMemo(() => {
        const listed = new Set(GROUPS.flatMap(g => g.items));
        const rest = (data?.items ?? []).map(i => i.name).filter(n => !listed.has(n));
        return rest.length > 0 ? [...GROUPS, { key: 'other' as GroupKey, items: rest }] : GROUPS;
    }, [data]);

    if (loading && !data) return <Loading />;
    if (!data || !draft) return <ErrorAlert error={error} onRetry={reload} />;

    const pending = data.items.filter(i => i.pending).map(i => i.name);

    // The update holds only what differs from the stored state.
    const changes = (() => {
        const set: Record<string, string> = {};
        const unset: string[] = [];
        for (const item of data.items) {
            if (item.kind === 'secret') {
                if (draft.clearPassword && item.stored) unset.push(item.name);
                else if (draft.password !== '') set[item.name] = draft.password;
                continue;
            }
            if (draft.resets.has(item.name)) {
                if (item.stored) unset.push(item.name);
            } else if (draft.values[item.name] !== item.value) {
                set[item.name] = draft.values[item.name];
            }
        }
        return { set, unset };
    })();
    const changed = Object.keys(changes.set).length + changes.unset.length > 0;

    const setValue = (name: string, value: string) =>
        setDraft(d => {
            if (!d) return d;
            const resets = new Set(d.resets);
            resets.delete(name);
            return { ...d, values: { ...d.values, [name]: value }, resets };
        });

    const toggleReset = (item: ConfigItem) =>
        setDraft(d => {
            if (!d) return d;
            const resets = new Set(d.resets);
            const values = { ...d.values };
            if (resets.has(item.name)) {
                resets.delete(item.name);
                values[item.name] = item.value;
            } else {
                resets.add(item.name);
                values[item.name] = item.default;
            }
            return { ...d, values, resets };
        });

    async function submit(e: FormEvent) {
        e.preventDefault();
        if (!changed) return;
        setBusy(true);
        setSaveError(null);
        try {
            const next = await updateConfig(changes);
            setData(next);
            const restart = [...Object.keys(changes.set), ...changes.unset].some(n => byName.get(n)?.restart);
            showToast(restart ? t('config.savedRestart') : t('config.saved'));
        } catch (err) {
            setSaveError(errorMessage(t, err));
        } finally {
            setBusy(false);
        }
    }

    function input(item: ConfigItem) {
        const id = `cfg-${itemKey(item.name)}`;
        const value = draft!.values[item.name] ?? '';
        if (item.kind === 'secret') {
            return (
                <div className='flex flex-col gap-2'>
                    <input
                        id={id}
                        type='password'
                        className={inputClass}
                        value={draft!.password}
                        disabled={draft!.clearPassword}
                        autoComplete='new-password'
                        placeholder={item.is_set ? t('config.secretSet') : t('config.secretNotSet')}
                        onChange={e => setDraft(d => (d ? { ...d, password: e.target.value } : d))}
                    />
                    {item.is_set && (
                        <label className='flex items-center gap-2 text-sm text-ink'>
                            <input
                                type='checkbox'
                                className={checkboxClass}
                                checked={draft!.clearPassword}
                                onChange={e =>
                                    setDraft(d => (d ? { ...d, clearPassword: e.target.checked, password: '' } : d))
                                }
                            />
                            {t('config.secretClear')}
                        </label>
                    )}
                </div>
            );
        }
        if (item.kind === 'bool') {
            return (
                <label className='flex items-center gap-2 text-sm text-ink'>
                    <input
                        id={id}
                        type='checkbox'
                        className={checkboxClass}
                        checked={value === 'true'}
                        onChange={e => setValue(item.name, e.target.checked ? 'true' : 'false')}
                    />
                    {t('config.enabled')}
                </label>
            );
        }
        if (item.name === 'smtp.security') {
            return (
                <select
                    id={id}
                    className={inputClass}
                    value={value}
                    onChange={e => setValue(item.name, e.target.value)}
                >
                    {SMTP_SECURITY.map(s => (
                        <option key={s} value={s}>
                            {t(`config.security.${s}`)}
                        </option>
                    ))}
                </select>
            );
        }
        return (
            <input
                id={id}
                type={item.kind === 'int' ? 'number' : 'text'}
                min={item.kind === 'int' ? 1 : undefined}
                inputMode={item.kind === 'int' ? 'numeric' : undefined}
                className={`${inputClass} ${item.kind === 'int' ? 'sm:max-w-48' : ''}`}
                value={value}
                spellCheck={false}
                onChange={e => setValue(item.name, e.target.value)}
            />
        );
    }

    function row(item: ConfigItem) {
        const key = itemKey(item.name);
        const reset = draft!.resets.has(item.name);
        const hint = (
            <>
                {t(`config.items.${key}.hint`)}
                {item.kind === 'list' && <> {t('config.listHint')}</>}
                {item.kind !== 'secret' && item.default !== '' && (
                    <> {t('config.defaultValue', { value: item.default })}</>
                )}
            </>
        );
        return (
            <div key={item.name} className='grid grid-cols-1 gap-2 border-b border-line pb-4 last:border-b-0 last:pb-0'>
                <div className='flex flex-wrap items-center gap-2'>
                    {item.restart ? (
                        <Badge tone='warning' icon='restart_alt' title={t('config.restartTitle')}>
                            {t('config.restart')}
                        </Badge>
                    ) : (
                        <Badge tone='success' icon='bolt' title={t('config.immediateTitle')}>
                            {t('config.immediate')}
                        </Badge>
                    )}
                    {item.pending && (
                        <Badge tone='danger' icon='pending'>
                            {t('config.pending')}
                        </Badge>
                    )}
                    {!item.stored && !reset && <Badge>{t('config.usingDefault')}</Badge>}
                    <span className='font-mono text-xs text-muted'>{item.name}</span>
                    {item.stored && item.kind !== 'secret' && (
                        <button
                            type='button'
                            onClick={() => toggleReset(item)}
                            className='ml-auto rounded px-1 text-xs text-accent hover:underline focus:outline-none focus-visible:ring-2 focus-visible:ring-accent'
                        >
                            {reset ? t('config.undoReset') : t('config.reset')}
                        </button>
                    )}
                </div>
                <Field label={t(`config.items.${key}.label`)} htmlFor={`cfg-${key}`} hint={hint}>
                    {input(item)}
                </Field>
            </div>
        );
    }

    return (
        <>
            <PageHeader
                title={t('config.title')}
                description={t('config.description')}
                actions={
                    <Button icon='refresh' onClick={reload} busy={loading}>
                        {t('common.reload')}
                    </Button>
                }
            />
            <form onSubmit={e => void submit(e)} className='flex max-w-3xl flex-col gap-4'>
                <Alert tone='info'>{t('config.applyNote')}</Alert>
                {pending.length > 0 && (
                    <Alert tone='warning' title={t('config.pendingTitle')}>
                        {t('config.pendingBody', { items: pending.join(', ') })}
                    </Alert>
                )}
                {groups.map(group => (
                    <Card key={group.key} title={t(`config.groups.${group.key}`)}>
                        <div className='flex flex-col gap-4'>
                            {group.items
                                .map(n => byName.get(n))
                                .filter((i): i is ConfigItem => !!i)
                                .map(row)}
                        </div>
                    </Card>
                ))}
                {saveError && <Alert tone='danger'>{saveError}</Alert>}
                <div className='sticky bottom-0 flex justify-end gap-2 border-t border-line bg-canvas/90 py-3 backdrop-blur'>
                    <Button
                        type='button'
                        icon='undo'
                        disabled={!changed || busy}
                        onClick={() => {
                            setDraft(initialDraft(data));
                            setSaveError(null);
                        }}
                    >
                        {t('config.discard')}
                    </Button>
                    <Button type='submit' variant='primary' icon='save' busy={busy} disabled={!changed}>
                        {t('config.save')}
                    </Button>
                </div>
            </form>
        </>
    );
}
