import { inputClass } from './FormControls';

/** Number of digits of a mailed verification code. */
export const CODE_LENGTH = 6;

/** Input for a mailed verification code: digits only, filled in by the browser from the mail where supported. */
export function CodeInput({
    id,
    value,
    onChange,
    autoFocus,
}: {
    id: string;
    value: string;
    onChange: (code: string) => void;
    autoFocus?: boolean;
}) {
    return (
        <input
            id={id}
            className={`${inputClass} font-mono text-lg tracking-[0.4em]`}
            inputMode='numeric'
            autoComplete='one-time-code'
            pattern={`[0-9]{${CODE_LENGTH}}`}
            maxLength={CODE_LENGTH}
            autoFocus={autoFocus}
            required
            value={value}
            onChange={e => onChange(e.target.value.replace(/\D/g, '').slice(0, CODE_LENGTH))}
        />
    );
}
