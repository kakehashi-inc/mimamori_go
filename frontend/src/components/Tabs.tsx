interface TabItem<T extends string> {
    value: T;
    label: string;
    icon?: string;
}

/** A row of tab buttons; the caller renders the selected panel. */
export function Tabs<T extends string>({
    value,
    onChange,
    items,
}: {
    value: T;
    onChange: (v: T) => void;
    items: TabItem<T>[];
}) {
    return (
        <div className='flex gap-1 overflow-x-auto border-b border-line' role='tablist'>
            {items.map(item => {
                const active = item.value === value;
                return (
                    <button
                        key={item.value}
                        type='button'
                        role='tab'
                        aria-selected={active}
                        onClick={() => onChange(item.value)}
                        className={`-mb-px flex items-center gap-1.5 border-b-2 px-3 py-2 text-sm font-medium whitespace-nowrap transition-colors focus:outline-none focus-visible:ring-2 focus-visible:ring-accent ${
                            active
                                ? 'border-accent text-accent'
                                : 'border-transparent text-muted hover:border-line hover:text-ink'
                        }`}
                    >
                        {item.icon && (
                            <span className='material-icons-outlined text-base' aria-hidden='true'>
                                {item.icon}
                            </span>
                        )}
                        {item.label}
                    </button>
                );
            })}
        </div>
    );
}
