import { Fragment, type ReactNode } from 'react';

/** Page title with an optional description, breadcrumb and action buttons. */
export function PageHeader({
    title,
    description,
    actions,
    breadcrumb,
}: {
    title: ReactNode;
    description?: ReactNode;
    actions?: ReactNode;
    breadcrumb?: ReactNode;
}) {
    return (
        <div className='mb-5 flex flex-col gap-3 sm:flex-row sm:items-end sm:justify-between'>
            <div className='min-w-0'>
                {breadcrumb && <div className='mb-1 text-xs text-muted'>{breadcrumb}</div>}
                <h1 className='text-xl font-bold tracking-tight break-words text-ink'>{title}</h1>
                {description && <p className='mt-1 text-sm text-muted'>{description}</p>}
            </div>
            {actions && <div className='flex flex-wrap gap-2'>{actions}</div>}
        </div>
    );
}

/** A bordered panel with an optional heading row. */
export function Card({
    title,
    actions,
    children,
    className = '',
}: {
    title?: ReactNode;
    actions?: ReactNode;
    children: ReactNode;
    className?: string;
}) {
    return (
        <section className={`rounded-lg border border-line bg-surface ${className}`}>
            {(title || actions) && (
                <div className='flex flex-wrap items-center justify-between gap-2 border-b border-line px-4 py-3'>
                    {title && <h2 className='text-sm font-semibold text-ink'>{title}</h2>}
                    {actions && <div className='flex flex-wrap gap-2'>{actions}</div>}
                </div>
            )}
            <div className='p-4'>{children}</div>
        </section>
    );
}

/** One label/value pair of a DescriptionList. */
export function DescriptionItem({ label, value }: { label: ReactNode; value: ReactNode }) {
    return (
        <div className='contents'>
            <dt className='text-muted'>{label}</dt>
            <dd className='min-w-0 break-words text-ink'>{value}</dd>
        </div>
    );
}

/** An entry of a DescriptionList: a label/value pair, or nodes that render DescriptionItem (such as a slot). */
export type DescriptionEntry = { label: ReactNode; value: ReactNode } | { node: ReactNode };

/** A two-column list of label/value pairs. */
export function DescriptionList({ items }: { items: DescriptionEntry[] }) {
    return (
        <dl className='grid grid-cols-1 gap-x-6 gap-y-3 text-sm sm:grid-cols-[max-content_1fr]'>
            {items.map((item, i) =>
                'node' in item ? (
                    <Fragment key={i}>{item.node}</Fragment>
                ) : (
                    <DescriptionItem key={i} label={item.label} value={item.value} />
                )
            )}
        </dl>
    );
}
