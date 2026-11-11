import { useTranslation } from 'react-i18next';
import type { ApplyStatus, Category, CertState, Role, TokenStatus } from '../types';
import { Badge, type BadgeTone } from './Badge';

const tokenTone: Record<TokenStatus, BadgeTone> = {
    unused: 'accent',
    active: 'success',
    expired: 'warning',
    revoked: 'danger',
};

export function TokenStatusBadge({ status }: { status: TokenStatus }) {
    const { t } = useTranslation();
    return <Badge tone={tokenTone[status] ?? 'neutral'}>{t(`tokenStatus.${status}`)}</Badge>;
}

const applyTone: Record<ApplyStatus, BadgeTone> = {
    none: 'neutral',
    pending: 'accent',
    ok: 'success',
    warning: 'warning',
    error: 'danger',
};

const applyIcon: Record<ApplyStatus, string> = {
    none: 'remove',
    pending: 'schedule',
    ok: 'check',
    warning: 'warning',
    error: 'error',
};

export function ApplyStatusBadge({ status }: { status: ApplyStatus }) {
    const { t } = useTranslation();
    return (
        <Badge tone={applyTone[status] ?? 'neutral'} icon={applyIcon[status]}>
            {t(`applyStatus.${status}`)}
        </Badge>
    );
}

/** Detection category: alert is red, normal is neutral, '' is "not specified". */
export function CategoryBadge({ category }: { category: Category }) {
    const { t } = useTranslation();
    if (category === 'alert') {
        return (
            <Badge tone='danger' icon='notifications_active'>
                {t('category.alert')}
            </Badge>
        );
    }
    if (category === 'normal') return <Badge tone='neutral'>{t('category.normal')}</Badge>;
    return <span className='text-xs text-muted'>{t('category.none')}</span>;
}

const certTone: Record<CertState, BadgeTone> = { active: 'success', next: 'accent', retired: 'neutral' };

export function CertStatusBadge({ status }: { status: CertState }) {
    const { t } = useTranslation();
    return <Badge tone={certTone[status] ?? 'neutral'}>{t(`certStatus.${status}`)}</Badge>;
}

export function RoleBadge({ role }: { role: Role }) {
    const { t } = useTranslation();
    return <Badge tone={role === 'admin' ? 'accent' : 'neutral'}>{t(`role.${role}`)}</Badge>;
}
