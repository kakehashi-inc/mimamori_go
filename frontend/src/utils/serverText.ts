// Texts the server and the agents send for display, shown in the current
// language. The server sends every such text in English (the CLI and the logs
// use it) and, next to it, a message: a stable code and its parameters
// (fields named "<field>_i18n", see the API specification in Documents/). The
// codes are the keys of the "srv" sections of the text files; a message whose
// code is not known here (an administrator text for another account, a newer
// server, an agent of an older build) is shown with the English it came with.
import type { TFunction } from 'i18next';
import i18n from '../i18n/i18n';
import type { ServerMsg, ServerProblem } from '../types';

export type { ServerMsg, ServerProblem };

/** Options of serverText and problemText. */
export interface ServerTextOptions {
    /** Formats a parameter that is an RFC3339 timestamp (the history shows them in local time). */
    time?: (value: string) => string;
}

/** A text of a list the server answered (validation problems, warnings) with its message when it has one. */
export interface CodedText {
    text: string;
    i18n?: ServerProblem | null;
}

// Separator between the location parts of a problem and its message (as the server joins them).
const WHERE_SEPARATOR = ': ';
const RFC3339 = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/;

// The keys are built at run time, so the typed key check of i18next does not apply here:
// this is the one place that looks up a key by a code received from the server.
type DynamicT = (key: string, options?: Record<string, unknown>) => string;

function isMessage(v: unknown): v is ServerProblem {
    return typeof v === 'object' && v !== null && typeof (v as { code?: unknown }).code === 'string';
}

// A message that ends a sentence of its own (the Japanese full stop, U+3002, or a full-width exclamation or question mark); the next message of a list follows it
// directly instead of after a separator, so no punctuation is doubled. English texts never end so.
const SENTENCE_END = /[\u3002\uff01\uff1f]$/;

/** Joins the items of a list parameter by the same rule as the server's English. */
function joinList(tt: DynamicT, name: string, items: unknown[], parts: string[]): string {
    if (name === 'lines') return parts.join(tt('srv.sep.lines'));
    if (!items.some(isMessage)) return parts.join(tt('srv.sep.list'));
    const sep = tt('srv.sep.problems');
    return parts.reduce((all, part, i) => (i === 0 ? part : all + (SENTENCE_END.test(all) ? '' : sep) + part), '');
}

/** A parameter value as text; null when it holds a message that cannot be shown in this language. */
function renderValue(tt: DynamicT, name: string, v: unknown, opts: ServerTextOptions | undefined): string | null {
    if (v === null || v === undefined) return '';
    if (typeof v === 'string') return opts?.time && RFC3339.test(v) ? opts.time(v) : v;
    if (typeof v === 'number' || typeof v === 'boolean') return String(v);
    if (Array.isArray(v)) {
        const parts: string[] = [];
        for (const item of v) {
            const s = renderValue(tt, name, item, opts);
            if (s === null) return null;
            parts.push(s);
        }
        return joinList(tt, name, v, parts);
    }
    if (isMessage(v)) return renderProblem(tt, v, opts);
    return null;
}

/** A message in the current language; null when its code (or a nested one) is not known here. */
function renderMsg(tt: DynamicT, m: ServerMsg, opts: ServerTextOptions | undefined): string | null {
    const params = m.params ?? {};
    if (m.code === '') return typeof params.text === 'string' ? params.text : null;
    const key = `srv.${m.code}`;
    // A code that names a group of texts (a newer server, a recorded action with a dot) is not known here.
    if (!i18n.exists(key) || typeof tt(key, { returnObjects: true }) !== 'string') return null;
    const values: Record<string, string> = {};
    for (const [name, v] of Object.entries(params)) {
        const s = renderValue(tt, name, v, opts);
        if (s === null) return null;
        values[name] = s;
    }
    // "replace" keeps parameter names apart from the options of i18next (count, context, ...).
    return tt(key, { replace: values, interpolation: { escapeValue: false } });
}

function renderProblem(tt: DynamicT, p: ServerProblem, opts: ServerTextOptions | undefined): string | null {
    const parts: string[] = [];
    for (const w of p.where ?? []) {
        if (!isMessage(w)) return null;
        const s = renderMsg(tt, w, opts);
        if (s === null) return null;
        parts.push(s);
    }
    const s = renderMsg(tt, p, opts);
    if (s === null) return null;
    parts.push(s);
    return parts.join(WHERE_SEPARATOR);
}

/** The text of a server message in the current language; fallback (its English) when it cannot be shown. */
export function serverText(
    t: TFunction,
    m: ServerMsg | null | undefined,
    fallback: string,
    opts?: ServerTextOptions
): string {
    if (!isMessage(m)) return fallback;
    return renderMsg(t as unknown as DynamicT, m, opts) ?? fallback;
}

/** The text of a problem (a message with its location) in the current language; fallback when it cannot be shown. */
export function problemText(
    t: TFunction,
    p: ServerProblem | null | undefined,
    fallback: string,
    opts?: ServerTextOptions
): string {
    if (!isMessage(p)) return fallback;
    return renderProblem(t as unknown as DynamicT, p, opts) ?? fallback;
}

/**
 * Pairs the English texts of a list with their messages. The messages are used only when the server sent
 * one per text (an older server sends none); otherwise every text is shown in English.
 */
export function codedTexts(
    texts: readonly string[] | null | undefined,
    coded: readonly (ServerProblem | null)[] | null | undefined
): CodedText[] {
    const list = texts ?? [];
    const msgs = coded && coded.length === list.length ? coded : null;
    return list.map((text, i) => ({ text, i18n: msgs ? msgs[i] : null }));
}

/** The text of an item made by codedTexts, in the current language. */
export function codedText(t: TFunction, item: CodedText, opts?: ServerTextOptions): string {
    return problemText(t, item.i18n, item.text, opts);
}
