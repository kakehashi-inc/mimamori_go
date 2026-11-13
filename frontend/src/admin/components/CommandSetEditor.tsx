import { useEffect, useId, useMemo, useState, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '../../components/Button';
import { checkboxClass, Field, inputClass, inputSmAutoClass, inputSmClass } from '../../components/FormControls';
import { Modal } from '../../components/Modal';
import { Card } from '../../components/PageHeader';
import { Alert } from '../../components/StateViews';
import type { CodeSpecKind, CommandPurpose, Joiner, OnProblems, ScheduleKind } from '../../types';
import {
    CODE_KINDS,
    codeCoverage,
    codeDraftValid,
    draftSchedule,
    EVERY_HOURS,
    EVERY_MINUTES,
    newCommandDraft,
    newKey,
    ON_PROBLEMS,
    SCHEDULE_KINDS,
    scheduleSummary,
    WEEKDAYS,
    type CodeDraft,
    type CommandDraft,
    type SetDraft,
} from './commandSetDraft';
import { splitShellCommands } from './splitCommands';

// Editor of one command set: the set fields, the schedule with its summary
// and the ordered list of commands.

interface EditorProps {
    draft: SetDraft;
    onChange: (d: SetDraft) => void;
    /** OS user the agent runs as ('' when it never reported). */
    agentUser: string;
    /** Host time zone text such as "JST, UTC+09:00" ('' when unknown). */
    hostZone: string;
}

/** Moves the item at index i by delta places (no change at the ends). */
function move<T>(list: T[], i: number, delta: number): T[] {
    const j = i + delta;
    if (j < 0 || j >= list.length) return list;
    const next = [...list];
    [next[i], next[j]] = [next[j], next[i]];
    return next;
}

export function CommandSetEditor({ draft, onChange, agentUser, hostZone }: EditorProps) {
    const { t } = useTranslation();
    const id = useId();
    const set = <K extends keyof SetDraft>(key: K, value: SetDraft[K]) => onChange({ ...draft, [key]: value });
    const collect = draft.purpose === 'collect_output';

    const setCommand = (i: number, c: CommandDraft) =>
        set(
            'commands',
            draft.commands.map((x, j) => (j === i ? c : x))
        );

    const [pasting, setPasting] = useState(false);
    // Key of the first pasted command, scrolled into view once it is rendered.
    const [revealKey, setRevealKey] = useState<number | null>(null);
    const addPasted = (lines: string[]) => {
        const added = lines.map(line => ({ ...newCommandDraft(), commandLine: line }));
        set('commands', [...draft.commands, ...added]);
        setPasting(false);
        setRevealKey(added[0].key);
    };
    useEffect(() => {
        if (revealKey === null) return;
        const el = document.getElementById(`${id}-command-${revealKey}`);
        const reduce = window.matchMedia('(prefers-reduced-motion: reduce)').matches;
        el?.scrollIntoView({ block: 'start', behavior: reduce ? 'auto' : 'smooth' });
        el?.querySelector('textarea')?.focus({ preventScroll: true });
        setRevealKey(null);
    }, [revealKey, id]);

    return (
        <div className='flex flex-col gap-4'>
            <Card title={t('commandSets.editor.basics')}>
                <div className='grid grid-cols-1 gap-4 md:grid-cols-2'>
                    <Field label={t('commandSets.fields.name')} htmlFor={`${id}-name`} required>
                        <input
                            id={`${id}-name`}
                            className={inputClass}
                            value={draft.name}
                            maxLength={100}
                            onChange={e => set('name', e.target.value)}
                        />
                    </Field>
                    <Field
                        label={t('commandSets.fields.osUser')}
                        htmlFor={`${id}-user`}
                        required
                        hint={
                            agentUser
                                ? t('commandSets.editor.osUserHint', { user: agentUser })
                                : t('commandSets.editor.osUserUnknown')
                        }
                    >
                        <input
                            id={`${id}-user`}
                            className={`${inputClass} font-mono`}
                            value={draft.osUser}
                            spellCheck={false}
                            autoCapitalize='none'
                            onChange={e => set('osUser', e.target.value)}
                        />
                    </Field>
                    <Field label={t('commandSets.fields.description')} htmlFor={`${id}-desc`} className='md:col-span-2'>
                        <textarea
                            id={`${id}-desc`}
                            className={inputClass}
                            rows={2}
                            maxLength={1000}
                            value={draft.description}
                            onChange={e => set('description', e.target.value)}
                        />
                    </Field>
                    <Field
                        label={t('commandSets.fields.workDir')}
                        htmlFor={`${id}-dir`}
                        hint={t('commandSets.editor.workDirHint')}
                        className='md:col-span-2'
                    >
                        <input
                            id={`${id}-dir`}
                            className={`${inputClass} font-mono`}
                            value={draft.workDir}
                            spellCheck={false}
                            placeholder={t('commandSets.editor.workDirPlaceholder')}
                            onChange={e => set('workDir', e.target.value)}
                        />
                    </Field>
                </div>
                <fieldset className='mt-4 flex flex-col gap-2'>
                    <legend className='mb-1 text-sm font-medium text-ink'>{t('commandSets.fields.purpose')}</legend>
                    <div className='grid grid-cols-1 gap-2 md:grid-cols-2'>
                        {(['collect_output', 'run_only'] as CommandPurpose[]).map(p => (
                            <ChoiceCard
                                key={p}
                                name={`${id}-purpose`}
                                checked={draft.purpose === p}
                                onSelect={() => set('purpose', p)}
                                title={t(`commandSets.purpose.${p}`)}
                                hint={t(`commandSets.purposeHint.${p}`)}
                            />
                        ))}
                    </div>
                </fieldset>
            </Card>

            <ScheduleCard draft={draft} onChange={onChange} hostZone={hostZone} />

            <Card title={t('commandSets.editor.results')}>
                <div className='grid grid-cols-1 gap-4 md:grid-cols-2'>
                    <Field
                        label={t('commandSets.fields.keepRuns')}
                        htmlFor={`${id}-keep`}
                        hint={t('commandSets.editor.keepRunsHint')}
                    >
                        <input
                            id={`${id}-keep`}
                            type='number'
                            min={1}
                            max={10000}
                            className={`${inputClass} sm:max-w-48`}
                            value={draft.keepRuns}
                            onChange={e => set('keepRuns', e.target.value)}
                        />
                    </Field>
                    {collect && (
                        <Field
                            label={t('commandSets.fields.joiner')}
                            htmlFor={`${id}-joiner`}
                            hint={t('commandSets.editor.joinerHint')}
                        >
                            <select
                                id={`${id}-joiner`}
                                className={`${inputClass} sm:max-w-48`}
                                value={draft.joiner}
                                onChange={e => set('joiner', e.target.value as Joiner)}
                            >
                                <option value='comma'>{t('commandSets.joiner.comma')}</option>
                                <option value='newline'>{t('commandSets.joiner.newline')}</option>
                            </select>
                        </Field>
                    )}
                </div>
            </Card>

            <section className='flex flex-col gap-3'>
                <div className='flex flex-wrap items-center justify-between gap-2'>
                    <h2 className='text-sm font-semibold text-ink'>{t('commandSets.editor.commands')}</h2>
                    <p className='text-xs text-muted'>{t('commandSets.editor.commandsHint')}</p>
                </div>
                {draft.commands.map((c, i) => (
                    <div key={c.key} id={`${id}-command-${c.key}`} className='scroll-mt-28'>
                        <CommandEditor
                            index={i}
                            count={draft.commands.length}
                            command={c}
                            collect={collect}
                            onChange={next => setCommand(i, next)}
                            onMove={delta => set('commands', move(draft.commands, i, delta))}
                            onRemove={() =>
                                set(
                                    'commands',
                                    draft.commands.filter((_, j) => j !== i)
                                )
                            }
                        />
                    </div>
                ))}
                <div className='flex flex-wrap gap-2'>
                    <Button icon='add' onClick={() => set('commands', [...draft.commands, newCommandDraft()])}>
                        {t('commandSets.editor.addCommand')}
                    </Button>
                    <Button icon='content_paste' onClick={() => setPasting(true)}>
                        {t('commandSets.editor.pasteCommands')}
                    </Button>
                </div>
            </section>
            {pasting && (
                <PasteCommandsDialog
                    existing={draft.commands.length}
                    onAdd={addPasted}
                    onClose={() => setPasting(false)}
                />
            )}
        </div>
    );
}

/** The preview lists at most this many commands, so that a very large paste stays responsive. */
const PREVIEW_MAX = 200;

/**
 * Dialog that splits pasted text into commands (see splitShellCommands) and
 * shows them before they are added after the existing commands.
 */
function PasteCommandsDialog({
    existing,
    onAdd,
    onClose,
}: {
    /** Number of commands the set already has. */
    existing: number;
    onAdd: (lines: string[]) => void;
    onClose: () => void;
}) {
    const { t } = useTranslation();
    const id = useId();
    const [text, setText] = useState('');
    const commands = useMemo(() => splitShellCommands(text), [text]);

    return (
        <Modal
            open
            title={t('commandSets.editor.pasteTitle')}
            onClose={onClose}
            size='lg'
            footer={
                <>
                    <Button onClick={onClose}>{t('common.cancel')}</Button>
                    <Button
                        variant='primary'
                        icon='add'
                        disabled={commands.length === 0}
                        onClick={() => onAdd(commands)}
                    >
                        {t('commandSets.editor.pasteAdd')}
                    </Button>
                </>
            }
        >
            <div className='flex flex-col gap-4'>
                <Field
                    label={t('commandSets.editor.pasteText')}
                    htmlFor={`${id}-text`}
                    hint={t('commandSets.editor.pasteHint')}
                >
                    <textarea
                        id={`${id}-text`}
                        className={`${inputClass} font-mono text-xs leading-5`}
                        rows={8}
                        spellCheck={false}
                        autoCapitalize='none'
                        value={text}
                        onChange={e => setText(e.target.value)}
                    />
                </Field>
                <div className='flex flex-col gap-2'>
                    <p className='text-sm font-medium text-ink' aria-live='polite'>
                        {t('commandSets.editor.pastePreview', { count: commands.length })}
                    </p>
                    {commands.length === 0 ? (
                        <p className='text-xs text-muted'>{t('commandSets.editor.pasteNone')}</p>
                    ) : (
                        <ol className='flex flex-col gap-2'>
                            {commands.slice(0, PREVIEW_MAX).map((c, i) => (
                                <li key={i} className='flex flex-col gap-1'>
                                    <span className='text-xs text-muted'>
                                        {t('commandSets.editor.command', { n: existing + i + 1 })}
                                    </span>
                                    <pre className='rounded-md border border-line bg-canvas px-3 py-2 font-mono text-xs leading-5 break-all whitespace-pre-wrap text-ink'>
                                        {c}
                                    </pre>
                                </li>
                            ))}
                        </ol>
                    )}
                    {commands.length > PREVIEW_MAX && (
                        <p className='text-xs text-muted'>
                            {t('commandSets.editor.pasteMore', {
                                shown: PREVIEW_MAX,
                                count: commands.length - PREVIEW_MAX,
                            })}
                        </p>
                    )}
                </div>
            </div>
        </Modal>
    );
}

/** A radio button shown as a bordered card with a title and a hint. */
function ChoiceCard({
    name,
    checked,
    onSelect,
    title,
    hint,
}: {
    name: string;
    checked: boolean;
    onSelect: () => void;
    title: ReactNode;
    hint: ReactNode;
}) {
    return (
        <label
            className={`flex cursor-pointer items-start gap-3 rounded-lg border px-3 py-2 transition-colors hover:bg-canvas ${
                checked ? 'border-accent bg-accent-soft' : 'border-line'
            }`}
        >
            <input type='radio' name={name} checked={checked} onChange={onSelect} className='mt-1 accent-accent' />
            <span className='flex flex-col gap-0.5'>
                <span className='text-sm font-medium text-ink'>{title}</span>
                <span className='text-xs text-muted'>{hint}</span>
            </span>
        </label>
    );
}

function ScheduleCard({
    draft,
    onChange,
    hostZone,
}: {
    draft: SetDraft;
    onChange: (d: SetDraft) => void;
    hostZone: string;
}) {
    const { t } = useTranslation();
    const id = useId();
    const set = <K extends keyof SetDraft>(key: K, value: SetDraft[K]) => onChange({ ...draft, [key]: value });
    const scheduled = draft.runMode === 'schedule';

    return (
        <Card title={t('commandSets.editor.schedule')}>
            <div className='flex flex-col gap-4'>
                <div className='grid grid-cols-1 gap-2 md:grid-cols-2'>
                    <ChoiceCard
                        name={`${id}-mode`}
                        checked={!scheduled}
                        onSelect={() => set('runMode', 'manual')}
                        title={t('commandSets.runMode.manual')}
                        hint={t('commandSets.runModeHint.manual')}
                    />
                    <ChoiceCard
                        name={`${id}-mode`}
                        checked={scheduled}
                        onSelect={() => set('runMode', 'schedule')}
                        title={t('commandSets.runMode.schedule')}
                        hint={t('commandSets.runModeHint.schedule')}
                    />
                </div>
                {scheduled && (
                    <div className='flex flex-col gap-4 rounded-lg border border-line p-3'>
                        <Field label={t('commandSets.editor.repeat')} htmlFor={`${id}-kind`}>
                            <select
                                id={`${id}-kind`}
                                className={`${inputClass} sm:max-w-72`}
                                value={draft.kind}
                                onChange={e => set('kind', e.target.value as ScheduleKind)}
                            >
                                {SCHEDULE_KINDS.map(k => (
                                    <option key={k} value={k}>
                                        {t(`commandSets.scheduleKind.${k}`)}
                                    </option>
                                ))}
                            </select>
                        </Field>
                        {draft.kind === 'once' && (
                            <Field
                                label={t('commandSets.editor.onceAt')}
                                htmlFor={`${id}-at`}
                                hint={t('commandSets.editor.onceAtHint')}
                            >
                                <input
                                    id={`${id}-at`}
                                    type='datetime-local'
                                    className={`${inputClass} sm:max-w-72`}
                                    value={draft.onceAt}
                                    onChange={e => set('onceAt', e.target.value)}
                                />
                            </Field>
                        )}
                        {draft.kind === 'weekly' && (
                            <fieldset>
                                <legend className='mb-1 text-sm font-medium text-ink'>
                                    {t('commandSets.editor.weekdays')}
                                </legend>
                                <div className='flex flex-wrap gap-3'>
                                    {WEEKDAYS.map(w => (
                                        <label key={w} className='flex items-center gap-1.5 text-sm text-ink'>
                                            <input
                                                type='checkbox'
                                                className={checkboxClass}
                                                checked={draft.weekdays.includes(w)}
                                                onChange={e =>
                                                    set(
                                                        'weekdays',
                                                        e.target.checked
                                                            ? [...draft.weekdays, w]
                                                            : draft.weekdays.filter(x => x !== w)
                                                    )
                                                }
                                            />
                                            {t(`commandSets.weekdays.${w}`)}
                                        </label>
                                    ))}
                                </div>
                            </fieldset>
                        )}
                        {(draft.kind === 'weekly' || draft.kind === 'daily') && (
                            <Field
                                label={t('commandSets.editor.time')}
                                htmlFor={`${id}-time`}
                                hint={t('commandSets.editor.hostTimeHint')}
                            >
                                <input
                                    id={`${id}-time`}
                                    type='time'
                                    className={`${inputClass} sm:max-w-40`}
                                    value={draft.time}
                                    onChange={e => set('time', e.target.value)}
                                />
                            </Field>
                        )}
                        {draft.kind === 'every_hours' && (
                            <Field label={t('commandSets.editor.everyHours')} htmlFor={`${id}-eh`}>
                                <select
                                    id={`${id}-eh`}
                                    className={`${inputClass} sm:max-w-40`}
                                    value={draft.everyHours}
                                    onChange={e => set('everyHours', Number(e.target.value))}
                                >
                                    {EVERY_HOURS.map(n => (
                                        <option key={n} value={n}>
                                            {t('commandSets.editor.hours', { n })}
                                        </option>
                                    ))}
                                </select>
                            </Field>
                        )}
                        {draft.kind === 'every_minutes' && (
                            <Field label={t('commandSets.editor.everyMinutes')} htmlFor={`${id}-em`}>
                                <select
                                    id={`${id}-em`}
                                    className={`${inputClass} sm:max-w-40`}
                                    value={draft.everyMinutes}
                                    onChange={e => set('everyMinutes', Number(e.target.value))}
                                >
                                    {EVERY_MINUTES.map(n => (
                                        <option key={n} value={n}>
                                            {t('commandSets.editor.minutes', { n })}
                                        </option>
                                    ))}
                                </select>
                            </Field>
                        )}
                        {draft.kind === 'hourly_at' && (
                            <Field label={t('commandSets.editor.minuteOfHour')} htmlFor={`${id}-min`}>
                                <input
                                    id={`${id}-min`}
                                    type='number'
                                    min={0}
                                    max={59}
                                    className={`${inputClass} sm:max-w-32`}
                                    value={draft.minute}
                                    onChange={e => set('minute', e.target.value)}
                                />
                            </Field>
                        )}
                    </div>
                )}
                <div className='flex items-start gap-2 rounded-lg bg-canvas px-3 py-2 text-sm text-ink'>
                    <span className='material-icons-outlined text-lg text-accent' aria-hidden='true'>
                        schedule
                    </span>
                    <span>{scheduleSummary(t, draftSchedule(draft), hostZone)}</span>
                </div>
            </div>
        </Card>
    );
}

function CommandEditor({
    index,
    count,
    command,
    collect,
    onChange,
    onMove,
    onRemove,
}: {
    index: number;
    count: number;
    command: CommandDraft;
    collect: boolean;
    onChange: (c: CommandDraft) => void;
    onMove: (delta: number) => void;
    onRemove: () => void;
}) {
    const { t } = useTranslation();
    const id = useId();
    const set = <K extends keyof CommandDraft>(key: K, value: CommandDraft[K]) =>
        onChange({ ...command, [key]: value });
    // Shown only while both specs are complete: an empty or invalid field would give a wrong list.
    const coverage =
        codeDraftValid(command.success) && codeDraftValid(command.failure)
            ? codeCoverage(command.success, command.failure)
            : null;
    const n = index + 1;

    return (
        <Card
            title={t('commandSets.editor.command', { n })}
            actions={
                <>
                    <Button
                        size='sm'
                        variant='ghost'
                        icon='arrow_upward'
                        disabled={index === 0}
                        onClick={() => onMove(-1)}
                        aria-label={t('commandSets.editor.moveUp', { n })}
                        title={t('commandSets.editor.moveUp', { n })}
                    />
                    <Button
                        size='sm'
                        variant='ghost'
                        icon='arrow_downward'
                        disabled={index === count - 1}
                        onClick={() => onMove(1)}
                        aria-label={t('commandSets.editor.moveDown', { n })}
                        title={t('commandSets.editor.moveDown', { n })}
                    />
                    <Button size='sm' variant='ghost' icon='delete' disabled={count <= 1} onClick={onRemove}>
                        {t('commandSets.editor.removeCommand')}
                    </Button>
                </>
            }
        >
            <div className='flex flex-col gap-4'>
                <Field
                    label={t('commandSets.fields.commandLine')}
                    htmlFor={`${id}-line`}
                    hint={t('commandSets.editor.commandLineHint')}
                    required
                >
                    <textarea
                        id={`${id}-line`}
                        className={`${inputClass} font-mono text-xs leading-5`}
                        rows={Math.min(12, Math.max(3, command.commandLine.split('\n').length + 1))}
                        spellCheck={false}
                        autoCapitalize='none'
                        value={command.commandLine}
                        onChange={e => set('commandLine', e.target.value)}
                    />
                </Field>
                <Field label={t('commandSets.fields.note')} htmlFor={`${id}-note`}>
                    <textarea
                        id={`${id}-note`}
                        className={inputClass}
                        rows={Math.min(6, Math.max(1, command.note.split('\n').length))}
                        maxLength={1000}
                        value={command.note}
                        onChange={e => set('note', e.target.value)}
                    />
                </Field>
                <div className='grid grid-cols-1 gap-4 md:grid-cols-2'>
                    <CodeSpecInput
                        label={t('commandSets.fields.success')}
                        value={command.success}
                        onChange={v => set('success', v)}
                    />
                    <CodeSpecInput
                        label={t('commandSets.fields.failure')}
                        value={command.failure}
                        onChange={v => set('failure', v)}
                    />
                </div>
                {coverage &&
                    (coverage.overlap ? (
                        <Alert tone='danger'>{t('commandSets.editor.codesOverlap', { codes: coverage.overlap })}</Alert>
                    ) : (
                        <p className='text-xs text-muted'>
                            {coverage.errors
                                ? t('commandSets.editor.errorCodes', { codes: coverage.errors })
                                : t('commandSets.editor.noErrorCodes')}
                        </p>
                    ))}
                <div className='grid grid-cols-1 gap-4 md:grid-cols-3'>
                    <Field
                        label={t('commandSets.fields.onProblems')}
                        htmlFor={`${id}-op`}
                        hint={t(`commandSets.onProblemsHint.${command.onProblems}`)}
                        className='md:col-span-2'
                    >
                        <select
                            id={`${id}-op`}
                            className={inputClass}
                            value={command.onProblems}
                            onChange={e => set('onProblems', e.target.value as OnProblems)}
                        >
                            {ON_PROBLEMS.map(p => (
                                <option key={p} value={p}>
                                    {t(`commandSets.onProblems.${p}`)}
                                </option>
                            ))}
                        </select>
                    </Field>
                    <Field
                        label={t('commandSets.fields.timeoutSec')}
                        htmlFor={`${id}-to`}
                        hint={t('commandSets.editor.timeoutHint')}
                    >
                        <input
                            id={`${id}-to`}
                            type='number'
                            min={1}
                            max={86400}
                            className={inputClass}
                            value={command.timeoutSec}
                            onChange={e => set('timeoutSec', e.target.value)}
                        />
                    </Field>
                </div>
                {collect && (
                    <div className='flex flex-col gap-4 border-t border-line pt-4'>
                        <Field
                            label={t('commandSets.fields.lineLimit')}
                            htmlFor={`${id}-ll`}
                            hint={t('commandSets.editor.lineLimitHint')}
                        >
                            <input
                                id={`${id}-ll`}
                                type='number'
                                min={1}
                                max={10000}
                                className={`${inputClass} sm:max-w-48`}
                                value={command.lineLimit}
                                onChange={e => set('lineLimit', e.target.value)}
                            />
                        </Field>
                        <PatternList command={command} onChange={onChange} />
                    </div>
                )}
            </div>
        </Card>
    );
}

function PatternList({ command, onChange }: { command: CommandDraft; onChange: (c: CommandDraft) => void }) {
    const { t } = useTranslation();
    const patterns = command.patterns;
    const setPatterns = (p: CommandDraft['patterns']) => onChange({ ...command, patterns: p });

    return (
        <div className='flex flex-col gap-2'>
            <div>
                <p className='text-sm font-medium text-ink'>{t('commandSets.fields.patterns')}</p>
                <p className='text-xs text-muted'>{t('commandSets.editor.patternsHint')}</p>
            </div>
            {patterns.length === 0 && <p className='text-xs text-muted'>{t('commandSets.editor.noPatterns')}</p>}
            <ol className='flex flex-col gap-2'>
                {patterns.map((p, i) => (
                    <li key={p.key} className='flex items-center gap-1'>
                        <span className='w-6 shrink-0 text-right text-xs text-muted'>{i + 1}.</span>
                        <input
                            className={`${inputSmClass} font-mono text-xs`}
                            value={p.value}
                            spellCheck={false}
                            autoCapitalize='none'
                            aria-label={t('commandSets.editor.pattern', { n: i + 1 })}
                            placeholder='(?<name>...)'
                            onChange={e =>
                                setPatterns(patterns.map(x => (x.key === p.key ? { ...x, value: e.target.value } : x)))
                            }
                        />
                        <Button
                            size='sm'
                            variant='ghost'
                            icon='arrow_upward'
                            disabled={i === 0}
                            onClick={() => setPatterns(move(patterns, i, -1))}
                            aria-label={t('commandSets.editor.patternUp', { n: i + 1 })}
                            title={t('commandSets.editor.patternUp', { n: i + 1 })}
                        />
                        <Button
                            size='sm'
                            variant='ghost'
                            icon='arrow_downward'
                            disabled={i === patterns.length - 1}
                            onClick={() => setPatterns(move(patterns, i, 1))}
                            aria-label={t('commandSets.editor.patternDown', { n: i + 1 })}
                            title={t('commandSets.editor.patternDown', { n: i + 1 })}
                        />
                        <Button
                            size='sm'
                            variant='ghost'
                            icon='close'
                            onClick={() => setPatterns(patterns.filter(x => x.key !== p.key))}
                            aria-label={t('commandSets.editor.removePattern', { n: i + 1 })}
                            title={t('commandSets.editor.removePattern', { n: i + 1 })}
                        />
                    </li>
                ))}
            </ol>
            <div>
                <Button size='sm' icon='add' onClick={() => setPatterns([...patterns, { key: newKey(), value: '' }])}>
                    {t('commandSets.editor.addPattern')}
                </Button>
            </div>
        </div>
    );
}

function CodeSpecInput({
    label,
    value,
    onChange,
}: {
    label: string;
    value: CodeDraft;
    onChange: (v: CodeDraft) => void;
}) {
    const { t } = useTranslation();
    const id = useId();
    const numberInput = (field: 'a' | 'b', aria: string) => (
        <input
            type='number'
            min={0}
            max={255}
            aria-label={`${label}: ${aria}`}
            className={inputSmClass.replace('w-full', 'w-20')}
            value={value[field]}
            onChange={e => onChange({ ...value, [field]: e.target.value })}
        />
    );
    return (
        <fieldset className='flex flex-col gap-1'>
            <legend className='mb-1 text-sm font-medium text-ink'>{label}</legend>
            <div className='flex flex-wrap items-center gap-2 text-sm text-ink'>
                <select
                    id={id}
                    aria-label={label}
                    className={inputSmAutoClass}
                    value={value.kind}
                    onChange={e => onChange({ ...value, kind: e.target.value as CodeSpecKind })}
                >
                    {CODE_KINDS.map(k => (
                        <option key={k} value={k}>
                            {t(`commandSets.codeKind.${k}`)}
                        </option>
                    ))}
                </select>
                {numberInput(
                    'a',
                    value.kind === 'range' ? t('commandSets.editor.codeFrom') : t('commandSets.editor.code')
                )}
                {value.kind === 'range' && (
                    <>
                        <span className='text-muted'>{t('commandSets.editor.codeTo')}</span>
                        {numberInput('b', t('commandSets.editor.codeUntil'))}
                    </>
                )}
            </div>
        </fieldset>
    );
}
