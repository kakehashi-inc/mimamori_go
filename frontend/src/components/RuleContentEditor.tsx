import { useEffect, useState, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import type { DetectionType, LegitLocation, RuleContent } from '../types';
import { enabledTypes, normalizeRuleContent } from '../utils/ruleContent';
import { Badge } from './Badge';
import { Button } from './Button';
import { inputClass, inputSmAutoClass, inputSmClass } from './FormControls';
import { CategorySelect, CommaListInput, ElementListEditor, RemoveButton, StringListEditor } from './ListEditors';
import { Tabs } from './Tabs';

interface RuleContentEditorProps {
    value: RuleContent;
    onChange: (v: RuleContent) => void;
    disabled?: boolean;
    /**
     * Called with true while the JSON tab holds text that does not parse, and
     * with false once it parses again or the tab is left. While it is invalid,
     * `value` is the last valid content, not what the tab shows.
     */
    onJsonInvalidChange?: (invalid: boolean) => void;
}

/** Structured form editor for RuleContent, with a raw JSON tab. */
export function RuleContentEditor({ value, onChange, disabled, onJsonInvalidChange }: RuleContentEditorProps) {
    const { t } = useTranslation();
    const [tab, setTab] = useState<'form' | 'json'>('form');
    return (
        <div className='flex flex-col gap-4'>
            <Tabs
                value={tab}
                onChange={v => {
                    setTab(v);
                    // Leaving the JSON tab drops its text; the form shows the last valid content.
                    onJsonInvalidChange?.(false);
                }}
                items={[
                    { value: 'form', label: t('ruleEditor.tabForm'), icon: 'tune' },
                    { value: 'json', label: t('ruleEditor.tabJson'), icon: 'data_object' },
                ]}
            />
            {tab === 'form' ? (
                <RuleContentForm value={value} onChange={onChange} disabled={disabled} />
            ) : (
                <RuleContentJson
                    value={value}
                    onChange={onChange}
                    disabled={disabled}
                    onJsonInvalidChange={onJsonInvalidChange}
                />
            )}
        </div>
    );
}

function EditorSection({
    title,
    hint,
    enabled,
    children,
}: {
    title: ReactNode;
    hint?: ReactNode;
    enabled?: boolean;
    children: ReactNode;
}) {
    const { t } = useTranslation();
    return (
        <fieldset className='rounded-lg border border-line bg-surface p-4'>
            <legend className='flex items-center gap-2 px-1 text-sm font-semibold text-ink'>
                {title}
                {enabled !== undefined && (
                    <Badge tone={enabled ? 'success' : 'neutral'}>
                        {enabled ? t('ruleView.enabled') : t('ruleView.disabled')}
                    </Badge>
                )}
            </legend>
            {hint && <p className='mb-3 text-xs text-muted'>{hint}</p>}
            <div className='flex flex-col gap-3'>{children}</div>
        </fieldset>
    );
}

function Row({ label, htmlFor, children }: { label: ReactNode; htmlFor?: string; children: ReactNode }) {
    return (
        <div className='grid grid-cols-1 gap-1 sm:grid-cols-[12rem_1fr] sm:items-start sm:gap-3'>
            <label htmlFor={htmlFor} className='pt-1 text-sm text-muted'>
                {label}
            </label>
            <div className='min-w-0'>{children}</div>
        </div>
    );
}

function RuleContentForm({ value: c, onChange, disabled }: RuleContentEditorProps) {
    const { t } = useTranslation();
    const enabled = enabledTypes(c);
    const set = (patch: Partial<RuleContent>) => onChange({ ...c, ...patch });
    const typeTitle = (type: DetectionType) => t(`detectionType.${type}`);

    function setLegit(i: number, patch: Partial<LegitLocation>) {
        set({
            exec_files: {
                ...c.exec_files,
                legit: c.exec_files.legit.map((l, j) => (j === i ? { ...l, ...patch } : l)),
            },
        });
    }

    return (
        <div className='flex flex-col gap-4'>
            <EditorSection title={t('ruleEditor.general')}>
                <Row label={t('rule.users')}>
                    <ElementListEditor
                        value={c.users}
                        onChange={users => set({ users })}
                        placeholder='appuser'
                        disabled={disabled}
                    />
                    <p className='mt-1 text-xs text-muted'>{t('ruleEditor.usersHint')}</p>
                </Row>
                <Row label={t('rule.excludeInteractive')} htmlFor='re-excl'>
                    <select
                        id='re-excl'
                        className={`${inputSmAutoClass} shrink-0`}
                        disabled={disabled}
                        value={c.exclude_interactive === null ? '' : String(c.exclude_interactive)}
                        onChange={ev =>
                            set({ exclude_interactive: ev.target.value === '' ? null : ev.target.value === 'true' })
                        }
                    >
                        <option value=''>{t('ruleEditor.unspecified')}</option>
                        <option value='true'>{t('ruleEditor.excludeTrue')}</option>
                        <option value='false'>{t('ruleEditor.excludeFalse')}</option>
                    </select>
                </Row>
                <Row label={t('rule.checkInterval')} htmlFor='re-check'>
                    <NumberInput
                        id='re-check'
                        value={c.check_interval_sec}
                        onChange={v => set({ check_interval_sec: v })}
                        disabled={disabled}
                    />
                    <p className='mt-1 text-xs text-muted'>{t('ruleEditor.checkIntervalHint')}</p>
                </Row>
                <Row label={t('rule.scanInterval')} htmlFor='re-scan'>
                    <NumberInput
                        id='re-scan'
                        value={c.scan_interval_sec}
                        onChange={v => set({ scan_interval_sec: v })}
                        disabled={disabled}
                    />
                    <p className='mt-1 text-xs text-muted'>{t('ruleEditor.scanIntervalHint')}</p>
                </Row>
            </EditorSection>

            <EditorSection title={typeTitle('exec')} hint={t('ruleEditor.condExec')} enabled={enabled.exec}>
                <Row label={t('rule.category')} htmlFor='re-exec-cat'>
                    <CategorySelect
                        id='re-exec-cat'
                        value={c.exec.category}
                        onChange={category => set({ exec: { category } })}
                        disabled={disabled}
                    />
                </Row>
            </EditorSection>

            <EditorSection title={typeTitle('denied')} hint={t('ruleEditor.condDenied')} enabled={enabled.denied}>
                <Row label={t('rule.category')} htmlFor='re-denied-cat'>
                    <CategorySelect
                        id='re-denied-cat'
                        value={c.denied.category}
                        onChange={category => set({ denied: { category } })}
                        disabled={disabled}
                    />
                </Row>
            </EditorSection>

            <EditorSection title={typeTitle('read')} hint={t('ruleEditor.condRead')} enabled={enabled.read}>
                <Row label={t('rule.readDirs')}>
                    <ElementListEditor
                        value={c.read.dirs}
                        onChange={dirs => set({ read: { ...c.read, dirs } })}
                        placeholder='/etc/ssl/private'
                        disabled={disabled}
                    />
                </Row>
                <Row label={t('rule.category')} htmlFor='re-read-cat'>
                    <CategorySelect
                        id='re-read-cat'
                        value={c.read.category}
                        onChange={category => set({ read: { ...c.read, category } })}
                        disabled={disabled}
                    />
                </Row>
            </EditorSection>

            <EditorSection title={typeTitle('network')} hint={t('ruleEditor.condNetwork')} enabled={enabled.network}>
                <Row label={t('rule.networkExecutables')}>
                    <ElementListEditor
                        value={c.network.executables}
                        onChange={executables => set({ network: { ...c.network, executables } })}
                        placeholder='/usr/local/bin/app'
                        disabled={disabled}
                    />
                </Row>
                <Row label={t('rule.category')} htmlFor='re-network-cat'>
                    <CategorySelect
                        id='re-network-cat'
                        value={c.network.category}
                        onChange={category => set({ network: { ...c.network, category } })}
                        disabled={disabled}
                    />
                </Row>
            </EditorSection>

            <EditorSection title={typeTitle('writable')} hint={t('ruleEditor.condWritable')} enabled={enabled.writable}>
                <Row label={t('rule.writableBase')} htmlFor='re-base'>
                    <input
                        id='re-base'
                        className={`${inputSmClass} font-mono`}
                        value={c.writable.base}
                        placeholder='/var/www'
                        disabled={disabled}
                        onChange={ev => set({ writable: { ...c.writable, base: ev.target.value } })}
                    />
                </Row>
                <Row label={t('rule.allowedPaths')}>
                    <StringListEditor
                        value={c.writable.allowed_paths}
                        onChange={allowed_paths => set({ writable: { ...c.writable, allowed_paths } })}
                        placeholder='uploads'
                        disabled={disabled}
                    />
                    <p className='mt-1 text-xs text-muted'>{t('ruleEditor.allowedPathsHint')}</p>
                </Row>
                <Row label={t('rule.category')} htmlFor='re-writable-cat'>
                    <CategorySelect
                        id='re-writable-cat'
                        value={c.writable.category}
                        onChange={category => set({ writable: { ...c.writable, category } })}
                        disabled={disabled}
                    />
                </Row>
            </EditorSection>

            <EditorSection
                title={typeTitle('exec_file')}
                hint={t('ruleEditor.condExecFile')}
                enabled={enabled.exec_file}
            >
                <Row label={t('rule.extensions')} htmlFor='re-ext'>
                    <CommaListInput
                        id='re-ext'
                        value={c.exec_files.extensions}
                        onChange={extensions => set({ exec_files: { ...c.exec_files, extensions } })}
                        placeholder='sh, cgi'
                        disabled={disabled}
                    />
                    <p className='mt-1 text-xs text-muted'>{t('ruleEditor.commaHint')}</p>
                </Row>
                <Row label={t('rule.legit')}>
                    <div className='flex flex-col gap-2'>
                        {c.exec_files.legit.map((l, i) => (
                            <div
                                key={i}
                                className='flex flex-col gap-2 rounded-md border border-line p-2 sm:flex-row sm:items-center'
                            >
                                <input
                                    className={`${inputSmClass} min-w-0 flex-[2] font-mono`}
                                    value={l.path}
                                    placeholder='app/cache'
                                    disabled={disabled}
                                    aria-label={t('rule.legitPath')}
                                    onChange={ev => setLegit(i, { path: ev.target.value })}
                                />
                                <div className='min-w-0 flex-1'>
                                    <CommaListInput
                                        value={l.extensions}
                                        onChange={extensions => setLegit(i, { extensions })}
                                        placeholder='sh'
                                        disabled={disabled}
                                        aria-label={t('rule.extensions')}
                                    />
                                </div>
                                <div className='flex items-center gap-2'>
                                    <CategorySelect
                                        value={l.category}
                                        onChange={category => setLegit(i, { category })}
                                        disabled={disabled}
                                        aria-label={t('rule.category')}
                                    />
                                    <RemoveButton
                                        disabled={disabled}
                                        onClick={() =>
                                            set({
                                                exec_files: {
                                                    ...c.exec_files,
                                                    legit: c.exec_files.legit.filter((_, j) => j !== i),
                                                },
                                            })
                                        }
                                    />
                                </div>
                            </div>
                        ))}
                        <div>
                            <Button
                                size='sm'
                                icon='add'
                                disabled={disabled}
                                onClick={() =>
                                    set({
                                        exec_files: {
                                            ...c.exec_files,
                                            legit: [...c.exec_files.legit, { path: '', extensions: [], category: '' }],
                                        },
                                    })
                                }
                            >
                                {t('common.add')}
                            </Button>
                        </div>
                        <p className='text-xs text-muted'>{t('ruleEditor.legitHint')}</p>
                    </div>
                </Row>
                <Row label={t('rule.otherCategory')} htmlFor='re-other-cat'>
                    <CategorySelect
                        id='re-other-cat'
                        value={c.exec_files.other_category}
                        onChange={other_category => set({ exec_files: { ...c.exec_files, other_category } })}
                        disabled={disabled}
                    />
                </Row>
            </EditorSection>
        </div>
    );
}

function NumberInput({
    id,
    value,
    onChange,
    disabled,
}: {
    id?: string;
    value: number;
    onChange: (v: number) => void;
    disabled?: boolean;
}) {
    return (
        <input
            id={id}
            type='number'
            min={0}
            step={1}
            className={`${inputSmClass} w-40`}
            value={value}
            disabled={disabled}
            onChange={ev => {
                const n = Number(ev.target.value);
                onChange(Number.isFinite(n) && n >= 0 ? Math.floor(n) : 0);
            }}
        />
    );
}

function RuleContentJson({ value, onChange, disabled, onJsonInvalidChange }: RuleContentEditorProps) {
    const { t } = useTranslation();
    const [text, setText] = useState(() => JSON.stringify(value, null, 2));
    const [parseError, setParseError] = useState<string | null>(null);

    // Re-render the text when the content changes from outside while it parses cleanly.
    useEffect(() => {
        setText(prev => {
            try {
                if (JSON.stringify(normalizeRuleContent(JSON.parse(prev))) === JSON.stringify(value)) return prev;
            } catch {
                return prev;
            }
            return JSON.stringify(value, null, 2);
        });
    }, [value]);

    return (
        <div className='flex flex-col gap-2'>
            <textarea
                className={`${inputClass} min-h-[28rem] font-mono text-xs`}
                value={text}
                disabled={disabled}
                spellCheck={false}
                onChange={ev => {
                    setText(ev.target.value);
                    try {
                        onChange(normalizeRuleContent(JSON.parse(ev.target.value)));
                        setParseError(null);
                        onJsonInvalidChange?.(false);
                    } catch (e) {
                        setParseError(e instanceof Error ? e.message : String(e));
                        onJsonInvalidChange?.(true);
                    }
                }}
                aria-label={t('ruleEditor.tabJson')}
            />
            {parseError ? (
                <p className='text-xs text-danger'>{t('ruleEditor.jsonError', { message: parseError })}</p>
            ) : (
                <p className='text-xs text-muted'>{t('ruleEditor.jsonHint')}</p>
            )}
        </div>
    );
}
