import type { TFunction } from 'i18next';
import { ApiError } from '../api/client';

function pad(n: number): string {
    return String(n).padStart(2, '0');
}

/** Formats an RFC3339 timestamp as local time "YYYY-MM-DD HH:mm:ss"; '-' when unset. */
export function formatDateTime(value: string | null | undefined): string {
    if (!value) return '-';
    const d = new Date(value);
    if (Number.isNaN(d.getTime())) return value;
    return (
        `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ` +
        `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`
    );
}

/** Replaces RFC3339 timestamps embedded in a text (such as a history detail) with local time. */
export function localizeTimestamps(text: string): string {
    return text.replace(/\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})/g, m => formatDateTime(m));
}

/** Converts a `<input type="datetime-local">` value to RFC3339 (UTC); '' stays ''. */
export function localInputToRfc3339(value: string): string {
    if (!value) return '';
    const d = new Date(value);
    return Number.isNaN(d.getTime()) ? '' : d.toISOString();
}

/** Formats a build version for display: "1.2.3" -> "v1.2.3", "dev" stays. */
export function formatVersion(version: string): string {
    return /^\d/.test(version) ? `v${version}` : version;
}

/** Human readable elapsed time such as "5 minutes ago". */
export function formatElapsed(t: TFunction, seconds: number | null | undefined): string {
    if (seconds === null || seconds === undefined) return t('time.never');
    const s = Math.max(0, Math.floor(seconds));
    if (s < 60) return t('time.secondsAgo', { count: s });
    if (s < 3600) return t('time.minutesAgo', { count: Math.floor(s / 60) });
    if (s < 86400) return t('time.hoursAgo', { count: Math.floor(s / 3600) });
    return t('time.daysAgo', { count: Math.floor(s / 86400) });
}

/** Days from now until the given timestamp (negative when already past). */
export function daysUntil(value: string | null | undefined): number | null {
    if (!value) return null;
    const d = new Date(value).getTime();
    if (Number.isNaN(d)) return null;
    return Math.floor((d - Date.now()) / 86400000);
}

/** A user-facing message for an error thrown by the API client or elsewhere. */
export function errorMessage(t: TFunction, e: unknown): string {
    if (e instanceof ApiError) {
        if (e.status === 0) return t('errors.network');
        if (e.message && !e.message.startsWith('HTTP ')) return e.message;
        if (e.status === 403) return t('errors.forbidden');
        if (e.status === 404) return t('errors.notFound');
        if (e.status === 503) return t('errors.busy');
        return t('errors.generic', { status: e.status });
    }
    if (e instanceof Error) return e.message;
    return String(e);
}
