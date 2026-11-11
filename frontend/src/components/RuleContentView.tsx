import type { ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import type { Category, DetectionType, RuleContent, RuleElement } from '../types';
import { enabledTypes, normalizeRuleContent } from '../utils/ruleContent';
import { Badge } from './Badge';
import { CategoryBadge } from './StatusBadges';

/** Read-only, human readable rendering of a RuleContent. */
export function RuleContentView({ content }: { content: RuleContent }) {
    const { t } = useTranslation();
    const c = normalizeRuleContent(content);
    const enabled = enabledTypes(c);

    const excl =
        c.exclude_interactive === null
            ? t('ruleEditor.unspecified')
            : c.exclude_interactive
              ? t('ruleEditor.excludeTrue')
              : t('ruleEditor.excludeFalse');

    const interval = (v: number, def: number) =>
        v === 0 ? t('ruleView.defaultSeconds', { seconds: def }) : t('ruleView.seconds', { seconds: v });

    return (
        <div className='flex flex-col gap-4'>
            <ViewSection title={t('ruleEditor.general')}>
                <ViewRow label={t('rule.users')}>
                    <ElementList items={c.users} />
                </ViewRow>
                <ViewRow label={t('rule.excludeInteractive')}>{excl}</ViewRow>
                <ViewRow label={t('rule.checkInterval')}>{interval(c.check_interval_sec, 60)}</ViewRow>
                <ViewRow label={t('rule.scanInterval')}>{interval(c.scan_interval_sec, 3600)}</ViewRow>
            </ViewSection>

            <div className='grid grid-cols-1 gap-4 lg:grid-cols-2'>
                <TypeSection type='exec' enabled={enabled.exec} category={c.exec.category} />
                <TypeSection type='denied' enabled={enabled.denied} category={c.denied.category} />
                <TypeSection type='read' enabled={enabled.read} category={c.read.category}>
                    <ViewRow label={t('rule.readDirs')}>
                        <ElementList items={c.read.dirs} />
                    </ViewRow>
                </TypeSection>
                <TypeSection type='network' enabled={enabled.network} category={c.network.category}>
                    <ViewRow label={t('rule.networkExecutables')}>
                        <ElementList items={c.network.executables} />
                    </ViewRow>
                </TypeSection>
                <TypeSection type='writable' enabled={enabled.writable} category={c.writable.category}>
                    <ViewRow label={t('rule.writableBase')}>
                        {c.writable.base ? <Mono>{c.writable.base}</Mono> : <None />}
                    </ViewRow>
                    <ViewRow label={t('rule.allowedPaths')}>
                        <StringList items={c.writable.allowed_paths} />
                    </ViewRow>
                </TypeSection>
                <TypeSection type='exec_file' enabled={enabled.exec_file} category={c.exec_files.other_category}>
                    <ViewRow label={t('rule.extensions')}>
                        <StringList items={c.exec_files.extensions} inline />
                    </ViewRow>
                    <ViewRow label={t('rule.legit')}>
                        {c.exec_files.legit.length === 0 ? (
                            <None />
                        ) : (
                            <ul className='flex flex-col gap-1'>
                                {c.exec_files.legit.map((l, i) => (
                                    <li key={i} className='flex flex-wrap items-center gap-2'>
                                        <Mono>{l.path || '-'}</Mono>
                                        <span className='text-xs text-muted'>
                                            {l.extensions.length ? l.extensions.join(', ') : t('ruleView.anyExtension')}
                                        </span>
                                        <CategoryBadge category={l.category} />
                                    </li>
                                ))}
                            </ul>
                        )}
                    </ViewRow>
                </TypeSection>
            </div>
        </div>
    );
}

function TypeSection({
    type,
    enabled,
    category,
    children,
}: {
    type: DetectionType;
    enabled: boolean;
    category: Category;
    children?: ReactNode;
}) {
    const { t } = useTranslation();
    return (
        <ViewSection
            title={t(`detectionType.${type}`)}
            badge={
                <Badge tone={enabled ? 'success' : 'neutral'}>
                    {enabled ? t('ruleView.enabled') : t('ruleView.disabled')}
                </Badge>
            }
        >
            <ViewRow label={type === 'exec_file' ? t('rule.otherCategory') : t('rule.category')}>
                <CategoryBadge category={category} />
            </ViewRow>
            {children}
        </ViewSection>
    );
}

function ViewSection({ title, badge, children }: { title: ReactNode; badge?: ReactNode; children: ReactNode }) {
    return (
        <section className='rounded-lg border border-line bg-surface'>
            <div className='flex items-center gap-2 border-b border-line px-4 py-2'>
                <h3 className='text-sm font-semibold text-ink'>{title}</h3>
                {badge}
            </div>
            <dl className='flex flex-col gap-2 p-4'>{children}</dl>
        </section>
    );
}

function ViewRow({ label, children }: { label: ReactNode; children: ReactNode }) {
    return (
        <div className='grid grid-cols-1 gap-1 text-sm sm:grid-cols-[11rem_1fr] sm:gap-3'>
            <dt className='text-muted'>{label}</dt>
            <dd className='min-w-0 break-words text-ink'>{children}</dd>
        </div>
    );
}

function Mono({ children }: { children: ReactNode }) {
    return <code className='rounded bg-canvas px-1 py-0.5 font-mono text-xs break-all'>{children}</code>;
}

function None() {
    const { t } = useTranslation();
    return <span className='text-muted'>{t('ruleView.none')}</span>;
}

function ElementList({ items }: { items: RuleElement[] }) {
    const { t } = useTranslation();
    if (items.length === 0) return <None />;
    return (
        <ul className='flex flex-wrap gap-1.5'>
            {items.map((e, i) => (
                <li key={i} className='flex items-center gap-1'>
                    <Mono>{e.value}</Mono>
                    {e.if_missing === 'warn' && (
                        <Badge tone='warning' title={t('ruleEditor.ifMissing')}>
                            {t('ruleEditor.ifMissingWarn')}
                        </Badge>
                    )}
                </li>
            ))}
        </ul>
    );
}

function StringList({ items, inline }: { items: string[]; inline?: boolean }) {
    if (items.length === 0) return <None />;
    return (
        <ul className={inline ? 'flex flex-wrap gap-1.5' : 'flex flex-col gap-1'}>
            {items.map((s, i) => (
                <li key={i}>
                    <Mono>{s}</Mono>
                </li>
            ))}
        </ul>
    );
}
