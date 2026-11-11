import { useTranslation } from 'react-i18next';
import type { DiffLine } from '../types';
import { EmptyState } from './StateViews';

const lineClass: Record<DiffLine['op'], string> = {
    '+': 'bg-success-soft text-success',
    '-': 'bg-danger-soft text-danger',
    ' ': 'text-ink',
};

/** Unified diff lines with +/- coloring. */
export function DiffView({ lines }: { lines: DiffLine[] }) {
    const { t } = useTranslation();
    if (lines.length === 0) return <EmptyState icon='difference'>{t('rules.diffEmpty')}</EmptyState>;
    const changed = lines.some(l => l.op !== ' ');
    return (
        <div className='overflow-x-auto rounded-lg border border-line bg-surface'>
            {!changed && <p className='border-b border-line px-3 py-2 text-xs text-muted'>{t('rules.diffSame')}</p>}
            <pre className='min-w-full font-mono text-xs leading-relaxed'>
                {lines.map((l, i) => (
                    <div key={i} className={`flex px-3 ${lineClass[l.op] ?? ''}`}>
                        <span className='w-4 shrink-0 select-none'>{l.op}</span>
                        <span className='whitespace-pre'>{l.text}</span>
                    </div>
                ))}
            </pre>
        </div>
    );
}
