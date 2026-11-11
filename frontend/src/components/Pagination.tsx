import { useTranslation } from 'react-i18next';
import { Button } from './Button';

interface PaginationProps {
    total: number;
    limit: number;
    offset: number;
    onChange: (offset: number) => void;
}

/** "1-100 of 250" with previous/next buttons. */
export function Pagination({ total, limit, offset, onChange }: PaginationProps) {
    const { t } = useTranslation();
    const from = total === 0 ? 0 : offset + 1;
    const to = Math.min(offset + limit, total);
    const page = Math.floor(offset / limit) + 1;
    const pages = Math.max(1, Math.ceil(total / limit));
    return (
        <div className='flex flex-wrap items-center justify-between gap-2 text-sm text-muted'>
            <span>{t('pagination.range', { from, to, total })}</span>
            <div className='flex items-center gap-2'>
                <Button
                    size='sm'
                    icon='chevron_left'
                    disabled={offset <= 0}
                    onClick={() => onChange(Math.max(0, offset - limit))}
                >
                    {t('pagination.prev')}
                </Button>
                <span>{t('pagination.page', { page, pages })}</span>
                <Button size='sm' disabled={offset + limit >= total} onClick={() => onChange(offset + limit)}>
                    {t('pagination.next')}
                    <span className='material-icons-outlined text-sm' aria-hidden='true'>
                        chevron_right
                    </span>
                </Button>
            </div>
        </div>
    );
}
