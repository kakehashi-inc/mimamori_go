import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import type { Category, IfMissing, RuleElement } from '../../types';
import { Button } from '../../components/Button';
import { inputSmAutoClass, inputSmClass } from '../../components/FormControls';

/** Rows of value + if_missing with add/remove buttons. */
export function ElementListEditor({
    value,
    onChange,
    placeholder,
    disabled,
}: {
    value: RuleElement[];
    onChange: (v: RuleElement[]) => void;
    placeholder?: string;
    disabled?: boolean;
}) {
    const { t } = useTranslation();
    function update(i: number, patch: Partial<RuleElement>) {
        onChange(value.map((e, j) => (j === i ? { ...e, ...patch } : e)));
    }
    return (
        <div className='flex flex-col gap-2'>
            {value.map((e, i) => (
                <div key={i} className='flex flex-wrap items-center gap-2 sm:flex-nowrap'>
                    <input
                        className={`${inputSmClass} min-w-0 flex-1 font-mono`}
                        value={e.value}
                        placeholder={placeholder}
                        disabled={disabled}
                        onChange={ev => update(i, { value: ev.target.value })}
                        aria-label={t('ruleEditor.value')}
                    />
                    <select
                        className={`${inputSmAutoClass} shrink-0`}
                        value={e.if_missing}
                        disabled={disabled}
                        onChange={ev => update(i, { if_missing: ev.target.value as IfMissing })}
                        aria-label={t('ruleEditor.ifMissing')}
                    >
                        <option value='ignore'>{t('ruleEditor.ifMissingIgnore')}</option>
                        <option value='warn'>{t('ruleEditor.ifMissingWarn')}</option>
                    </select>
                    <RemoveButton disabled={disabled} onClick={() => onChange(value.filter((_, j) => j !== i))} />
                </div>
            ))}
            <div>
                <Button
                    size='sm'
                    icon='add'
                    disabled={disabled}
                    onClick={() => onChange([...value, { value: '', if_missing: 'ignore' }])}
                >
                    {t('common.add')}
                </Button>
            </div>
        </div>
    );
}

/** Rows of plain strings with add/remove buttons. */
export function StringListEditor({
    value,
    onChange,
    placeholder,
    disabled,
}: {
    value: string[];
    onChange: (v: string[]) => void;
    placeholder?: string;
    disabled?: boolean;
}) {
    const { t } = useTranslation();
    return (
        <div className='flex flex-col gap-2'>
            {value.map((s, i) => (
                <div key={i} className='flex items-center gap-2'>
                    <input
                        className={`${inputSmClass} min-w-0 flex-1 font-mono`}
                        value={s}
                        placeholder={placeholder}
                        disabled={disabled}
                        onChange={ev => onChange(value.map((x, j) => (j === i ? ev.target.value : x)))}
                        aria-label={t('ruleEditor.value')}
                    />
                    <RemoveButton disabled={disabled} onClick={() => onChange(value.filter((_, j) => j !== i))} />
                </div>
            ))}
            <div>
                <Button size='sm' icon='add' disabled={disabled} onClick={() => onChange([...value, ''])}>
                    {t('common.add')}
                </Button>
            </div>
        </div>
    );
}

function parseList(text: string): string[] {
    return text
        .split(',')
        .map(s => s.trim())
        .filter(s => s !== '');
}

function sameList(a: string[], b: string[]): boolean {
    return a.length === b.length && a.every((v, i) => v === b[i]);
}

/** A single text box holding a comma separated list, e.g. "sh, cgi". */
export function CommaListInput({
    value,
    onChange,
    placeholder,
    disabled,
    id,
    'aria-label': ariaLabel,
}: {
    value: string[];
    onChange: (v: string[]) => void;
    placeholder?: string;
    disabled?: boolean;
    id?: string;
    /** Accessible name when no <label> points at the input. */
    'aria-label'?: string;
}) {
    const [text, setText] = useState(value.join(', '));

    // Follow outside changes (e.g. the JSON tab) without fighting the user's typing.
    useEffect(() => {
        setText(prev => (sameList(parseList(prev), value) ? prev : value.join(', ')));
    }, [value]);

    return (
        <input
            id={id}
            aria-label={ariaLabel}
            className={`${inputSmClass} font-mono`}
            value={text}
            placeholder={placeholder}
            disabled={disabled}
            onChange={ev => {
                setText(ev.target.value);
                onChange(parseList(ev.target.value));
            }}
        />
    );
}

/** Select for a detection category, including "not specified". */
export function CategorySelect({
    value,
    onChange,
    disabled,
    id,
    'aria-label': ariaLabel,
    required,
}: {
    value: Category;
    onChange: (v: Category) => void;
    disabled?: boolean;
    id?: string;
    /** Accessible name when no <label> points at the select. */
    'aria-label'?: string;
    /** "Not specified" cannot be chosen (it is shown, disabled, only while it is the value). */
    required?: boolean;
}) {
    const { t } = useTranslation();
    return (
        <select
            id={id}
            aria-label={ariaLabel}
            className={`${inputSmAutoClass} shrink-0`}
            value={value}
            disabled={disabled}
            onChange={ev => onChange(ev.target.value as Category)}
        >
            {(!required || value === '') && (
                <option value='' disabled={required}>
                    {t('category.none')}
                </option>
            )}
            <option value='alert'>{t('category.alert')}</option>
            <option value='normal'>{t('category.normal')}</option>
        </select>
    );
}

export function RemoveButton({ onClick, disabled }: { onClick: () => void; disabled?: boolean }) {
    const { t } = useTranslation();
    return (
        <button
            type='button'
            onClick={onClick}
            disabled={disabled}
            aria-label={t('common.remove')}
            title={t('common.remove')}
            className='flex h-8 w-8 shrink-0 items-center justify-center rounded-md text-muted hover:bg-danger-soft hover:text-danger focus:outline-none focus-visible:ring-2 focus-visible:ring-accent disabled:opacity-50'
        >
            <span className='material-icons-outlined text-lg' aria-hidden='true'>
                delete
            </span>
        </button>
    );
}
