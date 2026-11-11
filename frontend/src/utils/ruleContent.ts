import type { Category, DetectionType, LegitLocation, RuleContent, RuleElement } from '../types';

/** An empty rule content: nothing is monitored. */
export function emptyRuleContent(): RuleContent {
    return {
        users: [],
        exclude_interactive: null,
        check_interval_sec: 0,
        scan_interval_sec: 0,
        exec: { category: '' },
        denied: { category: '' },
        read: { dirs: [], category: '' },
        network: { executables: [], category: '' },
        writable: { base: '', allowed_paths: [], category: '' },
        exec_files: { extensions: [], legit: [], other_category: '' },
    };
}

function category(v: unknown): Category {
    return v === 'alert' || v === 'normal' ? v : '';
}

function elements(v: unknown): RuleElement[] {
    if (!Array.isArray(v)) return [];
    return v.map(e => ({
        value: typeof e?.value === 'string' ? e.value : '',
        if_missing: e?.if_missing === 'warn' ? 'warn' : 'ignore',
    }));
}

function strings(v: unknown): string[] {
    return Array.isArray(v) ? v.filter(s => typeof s === 'string') : [];
}

function legit(v: unknown): LegitLocation[] {
    if (!Array.isArray(v)) return [];
    return v.map(l => ({
        path: typeof l?.path === 'string' ? l.path : '',
        extensions: strings(l?.extensions),
        category: category(l?.category),
    }));
}

/**
 * Fills every missing field of a rule content with its empty value. The
 * server encodes empty lists as null, and the raw JSON editor accepts partial
 * objects, so every consumer goes through this first.
 */
export function normalizeRuleContent(input: unknown): RuleContent {
    const c = (input && typeof input === 'object' ? input : {}) as Record<string, any>; // Untyped JSON; each field is checked below.
    const num = (v: unknown) => (typeof v === 'number' && Number.isFinite(v) ? v : 0);
    return {
        users: elements(c.users),
        exclude_interactive: typeof c.exclude_interactive === 'boolean' ? c.exclude_interactive : null,
        check_interval_sec: num(c.check_interval_sec),
        scan_interval_sec: num(c.scan_interval_sec),
        exec: { category: category(c.exec?.category) },
        denied: { category: category(c.denied?.category) },
        read: { dirs: elements(c.read?.dirs), category: category(c.read?.category) },
        network: { executables: elements(c.network?.executables), category: category(c.network?.category) },
        writable: {
            base: typeof c.writable?.base === 'string' ? c.writable.base : '',
            allowed_paths: strings(c.writable?.allowed_paths),
            category: category(c.writable?.category),
        },
        exec_files: {
            extensions: strings(c.exec_files?.extensions),
            legit: legit(c.exec_files?.legit),
            other_category: category(c.exec_files?.other_category),
        },
    };
}

export const DETECTION_TYPES: DetectionType[] = ['exec', 'denied', 'read', 'network', 'writable', 'exec_file'];

/**
 * Whether each detection type is active with this content. A type whose
 * required fields are not all set is not monitored (see the API spec).
 */
export function enabledTypes(c: RuleContent): Record<DetectionType, boolean> {
    const hasUsers = c.users.length > 0;
    const hasBase = c.writable.base.trim() !== '';
    return {
        exec: hasUsers && c.exec.category !== '',
        denied: hasUsers && c.denied.category !== '',
        read: hasUsers && c.read.dirs.length > 0 && c.read.category !== '',
        network: c.network.executables.length > 0 && c.network.category !== '',
        writable: hasBase && c.writable.category !== '',
        exec_file:
            hasBase &&
            c.writable.allowed_paths.length > 0 &&
            c.exec_files.extensions.length > 0 &&
            c.exec_files.other_category !== '',
    };
}
