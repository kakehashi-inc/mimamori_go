import type { ReactNode } from 'react';

/** Shared look of text inputs, selects and textareas. */
export const inputClass =
    'block w-full rounded-md border border-line bg-surface px-3 py-2 text-sm text-ink placeholder:text-muted focus:border-accent focus:outline-none focus:ring-2 focus:ring-accent/30 disabled:opacity-60';

/** A compact variant for table cells and dense filter bars. */
export const inputSmClass =
    'block w-full rounded-md border border-line bg-surface px-2 py-1 text-sm text-ink placeholder:text-muted focus:border-accent focus:outline-none focus:ring-2 focus:ring-accent/30 disabled:opacity-60';

// Same look as inputSmClass but sized by its content, for selects placed
// next to a growing input in one row (w-full would squeeze the input).
export const inputSmAutoClass = inputSmClass.replace('w-full', 'w-auto');

export const checkboxClass = 'h-4 w-4 rounded border-line accent-accent focus-visible:ring-2 focus-visible:ring-accent';

interface FieldProps {
    label: ReactNode;
    htmlFor?: string;
    hint?: ReactNode;
    error?: ReactNode;
    required?: boolean;
    children: ReactNode;
    className?: string;
}

/** A labelled form row with an optional hint and error text. */
export function Field({ label, htmlFor, hint, error, required, children, className = '' }: FieldProps) {
    return (
        <div className={`flex flex-col gap-1 ${className}`}>
            <label htmlFor={htmlFor} className='text-sm font-medium text-ink'>
                {label}
                {required && <span className='ml-0.5 text-danger'>*</span>}
            </label>
            {children}
            {hint && <p className='text-xs text-muted'>{hint}</p>}
            {error && <p className='text-xs text-danger'>{error}</p>}
        </div>
    );
}
