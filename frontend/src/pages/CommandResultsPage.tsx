import { useTranslation } from 'react-i18next';
import { useSearchParams } from 'react-router-dom';
import { getCommandResultsByName, listAgents } from '../api/client';
import { Button } from '../components/Button';
import { inputClass } from '../components/FormControls';
import { Card, PageHeader } from '../components/PageHeader';
import { EmptyState, ErrorAlert, Loading } from '../components/StateViews';
import { Tabs } from '../components/Tabs';
import { useApi } from '../hooks/useApi';
import { AgentCommandResults, LatestTable } from './AgentCommandResults';

type View = 'agent' | 'set';

/**
 * Results of the command sets across agents: by agent (the latest values of
 * one agent's sets) or by command set (the sets of one name on every agent).
 * The choice is kept in the address so that it survives a reload.
 */
export function CommandResultsPage() {
    const { t } = useTranslation();
    const [params, setParams] = useSearchParams();
    const view: View = params.get('view') === 'set' ? 'set' : 'agent';

    const change = (next: Record<string, string>) => {
        const p = new URLSearchParams();
        for (const [k, v] of Object.entries(next)) if (v) p.set(k, v);
        setParams(p, { replace: true });
    };

    return (
        <>
            <PageHeader title={t('commandResults.title')} description={t('commandResults.description')} />
            <div className='flex flex-col gap-4'>
                <Tabs
                    value={view}
                    onChange={v => change({ view: v === 'set' ? 'set' : '' })}
                    items={[
                        { value: 'agent', label: t('commandResults.byAgent'), icon: 'dns' },
                        { value: 'set', label: t('commandResults.bySet'), icon: 'terminal' },
                    ]}
                />
                {view === 'agent' ? (
                    <ByAgent agentId={params.get('agent') ?? ''} onSelect={id => change({ agent: id })} />
                ) : (
                    <BySet name={params.get('name') ?? ''} onSelect={name => change({ view: 'set', name })} />
                )}
            </div>
        </>
    );
}

function ByAgent({ agentId, onSelect }: { agentId: string; onSelect: (id: string) => void }) {
    const { t } = useTranslation();
    const { data: agents, error, loading, reload } = useApi(listAgents, []);
    const selected = agents?.find(a => String(a.id) === agentId);

    return (
        <div className='flex flex-col gap-4'>
            <div className='flex flex-col gap-1 sm:max-w-md'>
                <label htmlFor='cr-agent' className='text-sm font-medium text-ink'>
                    {t('commandResults.agent')}
                </label>
                <select
                    id='cr-agent'
                    className={inputClass}
                    value={selected ? agentId : ''}
                    onChange={e => onSelect(e.target.value)}
                    disabled={!agents}
                >
                    <option value=''>{t('commandResults.selectAgent')}</option>
                    {(agents ?? []).map(a => (
                        <option key={a.id} value={a.id}>
                            {a.hostname ? `${a.display_name} (${a.hostname})` : a.display_name}
                        </option>
                    ))}
                </select>
            </div>
            <ErrorAlert error={error} onRetry={reload} />
            {loading && !agents ? (
                <Loading />
            ) : (
                selected && (
                    <AgentCommandResults key={selected.id} agentId={selected.id} title={selected.display_name} />
                )
            )}
        </div>
    );
}

function BySet({ name, onSelect }: { name: string; onSelect: (name: string) => void }) {
    const { t } = useTranslation();
    const { data, error, loading, reload } = useApi(() => getCommandResultsByName(name), [name]);
    const names = data?.names ?? [];
    const results = data?.results ?? [];

    return (
        <div className='flex flex-col gap-4'>
            <div className='flex flex-col gap-1 sm:max-w-md'>
                <label htmlFor='cr-set' className='text-sm font-medium text-ink'>
                    {t('commandResults.set')}
                </label>
                <select
                    id='cr-set'
                    className={inputClass}
                    value={names.includes(name) ? name : ''}
                    onChange={e => onSelect(e.target.value)}
                    disabled={!data}
                >
                    <option value=''>{t('commandResults.selectSet')}</option>
                    {names.map(n => (
                        <option key={n} value={n}>
                            {n}
                        </option>
                    ))}
                </select>
            </div>
            <ErrorAlert error={error} onRetry={reload} />
            {loading && !data ? (
                <Loading />
            ) : data && names.length === 0 ? (
                <EmptyState icon='terminal'>{t('commandResults.noNames')}</EmptyState>
            ) : (
                data &&
                name && (
                    <Card
                        title={name}
                        actions={
                            <Button size='sm' icon='refresh' onClick={reload} busy={loading}>
                                {t('common.reload')}
                            </Button>
                        }
                    >
                        {results.length === 0 ? (
                            <EmptyState icon='terminal'>{t('commandResults.noRows')}</EmptyState>
                        ) : (
                            <LatestTable items={results} byAgent />
                        )}
                    </Card>
                )
            )}
        </div>
    );
}
