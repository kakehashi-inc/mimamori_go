import type { ButtonHTMLAttributes } from 'react';

type Variant = 'primary' | 'secondary' | 'danger' | 'ghost';
type Size = 'sm' | 'md';

const base =
    'inline-flex items-center justify-center gap-1.5 rounded-md font-medium whitespace-nowrap transition-colors focus:outline-none focus-visible:ring-2 focus-visible:ring-accent focus-visible:ring-offset-2 focus-visible:ring-offset-surface disabled:cursor-not-allowed disabled:opacity-50';

const variants: Record<Variant, string> = {
    primary: 'bg-accent text-accent-contrast hover:bg-accent-hover',
    secondary: 'border border-line bg-surface text-ink hover:bg-line',
    danger: 'bg-danger text-danger-contrast hover:bg-danger-hover',
    ghost: 'text-muted hover:bg-line hover:text-ink',
};

const sizes: Record<Size, string> = {
    sm: 'px-2.5 py-1 text-xs',
    md: 'px-4 py-2 text-sm',
};

/** Class names of a button, for links that should look like one. */
export function buttonClass(variant: Variant = 'secondary', size: Size = 'md'): string {
    return `${base} ${variants[variant]} ${sizes[size]}`;
}

interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
    variant?: Variant;
    size?: Size;
    /** Material icon name shown before the label. */
    icon?: string;
    busy?: boolean;
}

export function Button({
    variant = 'secondary',
    size = 'md',
    icon,
    busy,
    className = '',
    children,
    type = 'button',
    disabled,
    ...rest
}: ButtonProps) {
    return (
        <button
            type={type}
            disabled={disabled || busy}
            className={`${buttonClass(variant, size)} ${className}`}
            {...rest}
        >
            {busy ? (
                <span
                    className={`material-icons-outlined animate-spin ${size === 'sm' ? 'text-sm' : 'text-base'}`}
                    aria-hidden='true'
                >
                    progress_activity
                </span>
            ) : (
                icon && (
                    <span
                        className={`material-icons-outlined ${size === 'sm' ? 'text-sm' : 'text-base'}`}
                        aria-hidden='true'
                    >
                        {icon}
                    </span>
                )
            )}
            {children}
        </button>
    );
}
