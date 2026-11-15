import { useEffect, useMemo, useState, type FormEvent } from 'react';
import type { TFunction } from 'i18next';
import { useTranslation } from 'react-i18next';
import { ApiError } from '../../api/client';
import { getConfig, listAccounts, setAccountNotify, updateConfig } from '../api';
import { NotifyRecipients } from '../components/NotifyRecipients';
import { Badge } from '../../components/Badge';
import { Button } from '../../components/Button';
import { checkboxClass, Field, inputClass, invalidInputClass } from '../../components/FormControls';
import { Card, PageHeader } from '../../components/PageHeader';
import { Alert, ErrorAlert, Loading } from '../../components/StateViews';
import { useToast } from '../../context/ToastContext';
import type ja from '../i18n/ja.json';
import { useApi } from '../../hooks/useApi';
import type { Account, CentralConfig, ConfigItem, ServerProblem } from '../../types';
import { errorMessage } from '../../utils/format';
import { problemText } from '../../utils/serverText';

// Items grouped by subject, in display order. Items the server reports but
// that are not listed here are shown in the last group.
type GroupKey = keyof typeof ja.config.groups;
type ItemKey = keyof typeof ja.config.items;

const GROUPS: { key: GroupKey; items: string[] }[] = [
    {
        key: 'server',
        items: ['web_listen', 'agent_listen', 'server_names', 'builtin_agent'],
    },
    {
        key: 'agents',
        items: ['token_valid_days', 'token_renew_before_days', 'default_check_interval_sec', 'retention_days'],
    },
    { key: 'sessions', items: ['session_remember_days', 'two_step_trust_days'] },
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
            'smtp.timeout_sec',
            'smtp.helo_name',
        ],
    },
];

const SMTP_SECURITY = ['starttls', 'tls', 'none'] as const;

// i18n keys cannot contain the dots of the item names.
const itemKey = (name: string) => name.replace(/\./g, '_') as ItemKey;

const inputId = (name: string) => `cfg-${itemKey(name)}`;

// A whole number as the server reads it: the digits 0 to 9 only.
const WHOLE_NUMBER = /^[0-9]+$/;

/** The screen's own problem with the value of a number item (its range is the server's), or null. */
function intProblem(t: TFunction, item: ConfigItem, value: string): string | null {
    if (item.kind !== 'int' || item.min === undefined || item.max === undefined) return null;
    const range = { min: item.min, max: item.max };
    if (!WHOLE_NUMBER.test(value)) return t('config.intNotWhole', range);
    const n = Number(value);
    return n < item.min || n > item.max ? t('config.intOutOfRange', range) : null;
}

// Parameters in which the configuration messages of the server (config.*) name items.
const ITEM_PARAMS = ['name', 'field', 'other'] as const;

/** The problems of a save the server refused (400); empty for any other failure. */
function refusalProblems(err: unknown): ServerProblem[] {
    if (!(err instanceof ApiError) || err.status !== 400) return [];
    if (err.problems && err.problems.length > 0) return err.problems;
    return err.i18n && err.i18n.code !== 'common.problems' ? [err.i18n] : [];
}

interface Draft {
    values: Record<string, string>;
    /** Stored items that the save sets back to their default. */
    resets: Set<string>;
    /** Items that are not stored and not typed into: they keep the default, and their fields show it. */
    follow: Set<string>;
    password: string;
    clearPassword: boolean;
}

function initialDraft(config: CentralConfig): Draft {
    const values: Record<string, string> = {};
    for (const item of config.items) values[item.name] = item.value;
    const follow = new Set(config.items.filter(i => !i.stored && i.kind !== 'secret').map(i => i.name));
    return { values, resets: new Set(), follow, password: '', clearPassword: false };
}

export function ConfigPage() {
    const { t } = useTranslation();
    const { showToast } = useToast();
    const { data, error, loading, reload, setData } = useApi(getConfig, []);
    const [draft, setDraft] = useState<Draft | null>(null);
    const [busy, setBusy] = useState(false);
    // What the Save bar says about the last attempt to save: that values the
    // form does not send are marked (while such a value is left), the
    // problems the server refused it with, or another failure.
    const [bar, setBar] = useState<{ kind: 'local' | 'refused' } | { kind: 'failure'; text: string } | null>(null);
    // The problems of the last refusal with the items they name. A problem is
    // shown at its items and at the Save bar until one of its items is changed.
    const [refused, setRefused] = useState<{ text: string; named: string[] }[]>([]);
    // The item to bring into view (a new object for every attempt).
    const [scrollTo, setScrollTo] = useState<{ name: string } | null>(null);
    const accounts = useApi(listAccounts, []);
    // Notification recipients changed in the form and not saved yet (account ID to checked).
    const [notify, setNotify] = useState<Record<number, boolean>>({});

    useEffect(() => {
        if (data) setDraft(initialDraft(data));
    }, [data]);

    useEffect(() => {
        if (!scrollTo) return;
        const el = document.getElementById(inputId(scrollTo.name));
        el?.scrollIntoView({ block: 'center' });
        el?.focus({ preventScroll: true });
    }, [scrollTo]);

    const byName = useMemo(() => new Map((data?.items ?? []).map(i => [i.name, i])), [data]);
    const groups = useMemo(() => {
        const listed = new Set(GROUPS.flatMap(g => g.items));
        const rest = (data?.items ?? []).map(i => i.name).filter(n => !listed.has(n));
        return rest.length > 0 ? [...GROUPS, { key: 'other' as GroupKey, items: rest }] : GROUPS;
    }, [data]);

    if (loading && !data) return <Loading />;
    if (!data || !draft) return <ErrorAlert error={error} onRetry={reload} />;

    const pending = data.items.filter(i => i.pending).map(i => i.name);

    // The default an item gets with the form as it is: smtp.port has the
    // default of the smtp.security chosen in the form.
    const defaultOf = (item: ConfigItem) => item.security_defaults?.[draft.values['smtp.security']] ?? item.default;
    // The value the field shows: the default for an item set back to it or
    // left at it, else what was typed.
    const shown = (item: ConfigItem) =>
        draft.resets.has(item.name) || draft.follow.has(item.name) ? defaultOf(item) : (draft.values[item.name] ?? '');

    // The update holds only what differs from the state after the save
    // without it: the stored value, or the default for an item that is not stored.
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
            } else if (!draft.follow.has(item.name)) {
                const v = draft.values[item.name];
                if (v !== (item.stored ? item.value : defaultOf(item))) set[item.name] = v;
            }
        }
        return { set, unset };
    })();
    const configChanged = Object.keys(changes.set).length + changes.unset.length > 0;
    const notifyChanges = Object.entries(notify).map(([id, on]) => ({ id: Number(id), on }));
    const changed = configChanged || notifyChanges.length > 0;
    // Items in the order of the screen, for the first one to bring into view.
    const order = groups.flatMap(g => g.items);
    const localProblem = (item: ConfigItem) => intProblem(t, item, shown(item));
    const invalid = order.filter(n => {
        const item = byName.get(n);
        return item !== undefined && localProblem(item) !== null;
    });
    const barMessages = (() => {
        switch (bar?.kind) {
            case 'local':
                return invalid.length > 0 ? [t('config.invalidValues')] : [];
            case 'refused':
                return refused.map(p => p.text);
            case 'failure':
                return [bar.text];
            default:
                return [];
        }
    })();

    // A change of an item leaves the problems that named it behind.
    const forgetRefusal = (name: string) =>
        setRefused(r => (r.some(p => p.named.includes(name)) ? r.filter(p => !p.named.includes(name)) : r));

    const setValue = (name: string, value: string) => {
        forgetRefusal(name);
        setDraft(d => {
            if (!d) return d;
            const resets = new Set(d.resets);
            resets.delete(name);
            const follow = new Set(d.follow);
            follow.delete(name);
            return { ...d, values: { ...d.values, [name]: value }, resets, follow };
        });
    };

    /** A problem of the server in the current language, with the items it names given by their labels. */
    const labelled = (p: ServerProblem, fallback: string) => {
        const named: string[] = [];
        const params: Record<string, unknown> = { ...(p.params ?? {}) };
        for (const k of ITEM_PARAMS) {
            const v = params[k];
            if (typeof v === 'string' && byName.has(v)) {
                named.push(v);
                params[k] = t('config.itemName', { label: t(`config.items.${itemKey(v)}.label`) });
            }
        }
        return { text: problemText(t, { ...p, params }, fallback), named };
    };

    const toggleReset = (item: ConfigItem) => {
        forgetRefusal(item.name);
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
    };

    async function submit(e: FormEvent) {
        e.preventDefault();
        if (!changed) return;
        // Values the server would refuse are reported at their fields and not sent.
        if (invalid.length > 0) {
            setBar({ kind: 'local' });
            setScrollTo({ name: invalid[0] });
            return;
        }
        setBusy(true);
        setBar(null);
        setRefused([]);
        try {
            if (configChanged) {
                const next = await updateConfig(changes);
                setData(next);
            }
            // There is one request per account whose check box changed. The
            // saved ones leave the form state even when a later one fails.
            const saved = new Map<number, Account>();
            try {
                for (const { id, on } of notifyChanges) saved.set(id, await setAccountNotify(id, on));
            } finally {
                if (saved.size > 0) {
                    accounts.setData((accounts.data ?? []).map(a => saved.get(a.id) ?? a));
                    setNotify(n => Object.fromEntries(Object.entries(n).filter(([id]) => !saved.has(Number(id)))));
                }
            }
            const restart = [...Object.keys(changes.set), ...changes.unset].some(n => byName.get(n)?.restart);
            showToast(restart ? t('config.savedRestart') : t('config.saved'));
        } catch (err) {
            const problems = refusalProblems(err);
            if (problems.length === 0) {
                setBar({ kind: 'failure', text: errorMessage(t, err) });
                return;
            }
            const fallback = err instanceof Error ? err.message : '';
            const list = problems.map(p => labelled(p, fallback));
            setBar({ kind: 'refused' });
            setRefused(list);
            const first = order.find(n => list.some(p => p.named.includes(n)));
            if (first) setScrollTo({ name: first });
        } finally {
            setBusy(false);
        }
    }

    function input(item: ConfigItem, marked: boolean) {
        const id = inputId(item.name);
        const value = shown(item);
        const markClass = marked ? invalidInputClass : '';
        if (item.kind === 'secret') {
            return (
                <div className='flex flex-col gap-2'>
                    <input
                        id={id}
                        type='password'
                        className={`${inputClass} ${markClass}`}
                        aria-invalid={marked}
                        value={draft!.password}
                        disabled={draft!.clearPassword}
                        autoComplete='new-password'
                        placeholder={item.is_set ? t('config.secretSet') : t('config.secretNotSet')}
                        onChange={e => {
                            forgetRefusal(item.name);
                            setDraft(d => (d ? { ...d, password: e.target.value } : d));
                        }}
                    />
                    {item.is_set && (
                        <label className='flex items-center gap-2 text-sm text-ink'>
                            <input
                                type='checkbox'
                                className={checkboxClass}
                                checked={draft!.clearPassword}
                                onChange={e => {
                                    forgetRefusal(item.name);
                                    setDraft(d => (d ? { ...d, clearPassword: e.target.checked, password: '' } : d));
                                }}
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
                        aria-invalid={marked}
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
                    className={`${inputClass} ${markClass}`}
                    aria-invalid={marked}
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
        if (item.kind === 'int') {
            return (
                <input
                    id={id}
                    type='number'
                    min={item.min}
                    max={item.max}
                    step={1}
                    inputMode='numeric'
                    className={`${inputClass} sm:max-w-48 ${markClass}`}
                    aria-invalid={marked}
                    value={value}
                    onChange={e => setValue(item.name, e.target.value)}
                />
            );
        }
        return (
            <input
                id={id}
                type='text'
                className={`${inputClass} ${markClass}`}
                aria-invalid={marked}
                value={value}
                spellCheck={false}
                onChange={e => setValue(item.name, e.target.value)}
            />
        );
    }

    function row(item: ConfigItem) {
        const key = itemKey(item.name);
        const reset = draft!.resets.has(item.name);
        const problems = [
            localProblem(item),
            ...refused.filter(p => p.named.includes(item.name)).map(p => p.text),
        ].filter((p): p is string => !!p);
        const hint = (
            <>
                {t(`config.items.${key}.hint`)}
                {item.kind === 'list' && <> {t('config.listHint')}</>}
                {item.min !== undefined && item.max !== undefined && (
                    <> {t('config.range', { min: item.min, max: item.max })}</>
                )}
                {item.kind !== 'secret' && defaultOf(item) !== '' && (
                    <> {t('config.defaultValue', { value: defaultOf(item) })}</>
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
                    {draft!.follow.has(item.name) && <Badge>{t('config.usingDefault')}</Badge>}
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
                <Field
                    label={t(`config.items.${key}.label`)}
                    htmlFor={inputId(item.name)}
                    hint={hint}
                    error={
                        problems.length > 0 && (
                            <span role='alert' className='flex flex-col'>
                                {problems.map((p, i) => (
                                    <span key={i}>{p}</span>
                                ))}
                            </span>
                        )
                    }
                >
                    {input(item, problems.length > 0)}
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
            {/* The form's own texts report the values (noValidate keeps the browser's messages away). */}
            <form noValidate onSubmit={e => void submit(e)} className='flex max-w-3xl flex-col gap-4'>
                <ErrorAlert error={error} onRetry={reload} />
                <Alert tone='info'>{t('config.applyNote')}</Alert>
                {pending.length > 0 && (
                    <Alert tone='warning' title={t('config.pendingTitle')}>
                        {t('config.pendingBody', { items: pending.join(', ') })}
                    </Alert>
                )}
                {groups.map(group => (
                    <Card key={group.key} title={t(`config.groups.${group.key}`)}>
                        <div className='flex flex-col gap-4'>
                            {group.key === 'notify' && (
                                <NotifyRecipients
                                    accounts={accounts.data}
                                    error={accounts.error}
                                    onRetry={accounts.reload}
                                    value={notify}
                                    onChange={setNotify}
                                />
                            )}
                            {group.items
                                .map(n => byName.get(n))
                                .filter((i): i is ConfigItem => !!i)
                                .map(row)}
                        </div>
                    </Card>
                ))}
                <div className='sticky bottom-0 flex flex-wrap items-center justify-end gap-2 border-t border-line bg-canvas/90 py-3 backdrop-blur'>
                    {barMessages.length > 0 && (
                        <div
                            role='alert'
                            className='mr-auto flex max-h-32 min-w-0 flex-1 basis-64 items-start gap-2 overflow-y-auto text-sm text-danger'
                        >
                            <span className='material-icons-outlined text-base' aria-hidden='true'>
                                error
                            </span>
                            <ul className='flex min-w-0 flex-col gap-0.5 break-words'>
                                {barMessages.map((m, i) => (
                                    <li key={i}>{m}</li>
                                ))}
                            </ul>
                        </div>
                    )}
                    <Button
                        type='button'
                        icon='undo'
                        disabled={!changed || busy}
                        onClick={() => {
                            setDraft(initialDraft(data));
                            setNotify({});
                            setBar(null);
                            setRefused([]);
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
