// Texts of a history row (GET /history, the token history of an agent) in the current language.
import type { TFunction } from 'i18next';
import type { HistoryItem } from '../types';
import { formatDateTime, localizeTimestamps } from './format';
import { serverText } from './serverText';

/**
 * The operation of a row. The recorded action is a stable word per category (a rejected connection
 * records its reason, with spaces); one that is not known here is shown as recorded.
 */
export function historyActionText(t: TFunction, h: Pick<HistoryItem, 'category' | 'action'>): string {
    return serverText(t, { code: `history.action.${h.category}.${h.action.replace(/ /g, '_')}` }, h.action);
}

/** The target of a row; mostly a value (a name, an address), shown as recorded. */
export function historyTargetText(t: TFunction, h: Pick<HistoryItem, 'target' | 'target_i18n'>): string {
    return serverText(t, h.target_i18n, h.target);
}

/**
 * The detail of a row. Only the token and certificate history carry timestamps written by the system,
 * which are shown in local time; the other categories hold text typed by users, shown unchanged.
 */
export function historyDetailText(t: TFunction, h: Pick<HistoryItem, 'category' | 'detail' | 'detail_i18n'>): string {
    const times = h.category === 'token' || h.category === 'certificate';
    const fallback = times ? localizeTimestamps(h.detail) : h.detail;
    return serverText(t, h.detail_i18n, fallback, times ? { time: formatDateTime } : undefined);
}
