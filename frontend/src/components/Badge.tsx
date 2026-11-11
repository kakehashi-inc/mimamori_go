import type { ReactNode } from 'react';

export type BadgeTone = 'neutral' | 'accent' | 'success' | 'warning' | 'danger';

const tones: Record<BadgeTone, string> = {
    neutral: 'border-line bg-canvas text-muted',
    accent: 'border-accent/30 bg-accent-soft text-accent',
    success: 'border-success/30 bg-success-soft text-success',
    warning: 'border-warning/30 bg-warning-soft text-warning',
    danger: 'border-danger/30 bg-danger-soft text-danger',
};

interface BadgeProps {
    tone?: BadgeTone;
    icon?: string;
    title?: string;
    children: ReactNode;
}

/** A small rounded status label. */
export function Badge({ tone = 'neutral', icon, title, children }: BadgeProps) {
    return (
        <span
            title={title}
            className={`inline-flex items-center gap-1 rounded-full border px-2 py-0.5 text-xs font-medium whitespace-nowrap ${tones[tone]}`}
        >
            {icon && (
                <span className='material-icons-outlined text-sm leading-none' aria-hidden='true'>
                    {icon}
                </span>
            )}
            {children}
        </span>
    );
}
