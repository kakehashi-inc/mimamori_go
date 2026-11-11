import type { ReactNode, TdHTMLAttributes, ThHTMLAttributes } from 'react';

/** A table inside a bordered card that scrolls horizontally on small screens. */
export function Table({ children, className = '' }: { children: ReactNode; className?: string }) {
    return (
        <div className={`overflow-x-auto rounded-lg border border-line bg-surface ${className}`}>
            <table className='min-w-full divide-y divide-line text-sm'>{children}</table>
        </div>
    );
}

export function THead({ children }: { children: ReactNode }) {
    return (
        <thead className='bg-canvas'>
            <tr>{children}</tr>
        </thead>
    );
}

export function TBody({ children }: { children: ReactNode }) {
    return <tbody className='divide-y divide-line'>{children}</tbody>;
}

/** Table row with a hover highlight. */
export function Tr({ children, selected }: { children: ReactNode; selected?: boolean }) {
    return <tr className={`transition-colors hover:bg-canvas ${selected ? 'bg-accent-soft' : ''}`}>{children}</tr>;
}

export function Th({ className = '', children, ...rest }: ThHTMLAttributes<HTMLTableCellElement>) {
    return (
        <th
            scope='col'
            className={`px-3 py-2 text-left text-xs font-semibold tracking-wide whitespace-nowrap text-muted ${className}`}
            {...rest}
        >
            {children}
        </th>
    );
}

export function Td({ className = '', children, ...rest }: TdHTMLAttributes<HTMLTableCellElement>) {
    return (
        <td className={`px-3 py-2 align-top text-ink ${className}`} {...rest}>
            {children}
        </td>
    );
}
