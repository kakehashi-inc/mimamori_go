// Form state of the command set editor and its conversion from and to the
// definition the API takes, the schedule summary and the code spec helpers.
import type { TFunction } from 'i18next';
import type {
    CodeSpec,
    CodeSpecKind,
    CommandPurpose,
    CommandSchedule,
    CommandSet,
    CommandSetSpec,
    Joiner,
    OnProblems,
    ScheduleKind,
    Weekday,
} from '../../types';

export const WEEKDAYS: Weekday[] = ['sun', 'mon', 'tue', 'wed', 'thu', 'fri', 'sat'];
export const SCHEDULE_KINDS: ScheduleKind[] = ['once', 'weekly', 'daily', 'every_hours', 'every_minutes', 'hourly_at'];
export const EVERY_HOURS = [1, 2, 3, 4, 6, 8, 12];
export const EVERY_MINUTES = [1, 2, 3, 4, 5, 6, 10, 12, 15, 20, 30];
export const CODE_KINDS: CodeSpecKind[] = ['single', 'range', 'min', 'max'];
export const ON_PROBLEMS: OnProblems[] = ['stop', 'continue_on_failure', 'continue_always'];

// Defaults of the server (app/modules/commandspec.go).
export const DEFAULT_KEEP_RUNS = 100;
export const DEFAULT_TIMEOUT_SEC = 300;
export const DEFAULT_LINE_LIMIT = 20;

/** Numbers are kept as typed text so that a field can be empty while editing. */
export interface CodeDraft {
    kind: CodeSpecKind;
    a: string;
    b: string;
}

export interface CommandDraft {
    /** Identity of the row in the form only (commands have no identity). */
    key: number;
    commandLine: string;
    note: string;
    success: CodeDraft;
    failure: CodeDraft;
    onProblems: OnProblems;
    timeoutSec: string;
    lineLimit: string;
    patterns: { key: number; value: string }[];
}

export interface SetDraft {
    name: string;
    description: string;
    purpose: CommandPurpose;
    osUser: string;
    workDir: string;
    runMode: 'manual' | 'schedule';
    kind: ScheduleKind;
    /** Value of a datetime-local input (browser local time). */
    onceAt: string;
    /**
     * The saved once time (RFC3339) and its datetime-local value: sent as it
     * was while the field is unchanged, so that seconds are kept and an
     * unchanged past time is not refused.
     */
    onceSaved: { at: string; input: string } | null;
    weekdays: Weekday[];
    time: string;
    everyHours: number;
    everyMinutes: number;
    minute: string;
    keepRuns: string;
    joiner: Joiner;
    commands: CommandDraft[];
}

let lastKey = 0;
/** A key for a new row of the form. */
export function newKey(): number {
    lastKey += 1;
    return lastKey;
}

function codeDraft(c: CodeSpec | undefined, fallback: CodeSpec): CodeDraft {
    const s = c ?? fallback;
    return { kind: s.kind, a: String(s.a), b: s.b === undefined ? '' : String(s.b) };
}

export function newCommandDraft(): CommandDraft {
    return {
        key: newKey(),
        commandLine: '',
        note: '',
        success: { kind: 'single', a: '0', b: '' },
        failure: { kind: 'min', a: '1', b: '' },
        onProblems: 'stop',
        timeoutSec: String(DEFAULT_TIMEOUT_SEC),
        lineLimit: String(DEFAULT_LINE_LIMIT),
        patterns: [],
    };
}

/**
 * Whether a command is still the empty one a form starts with: nothing was
 * entered and no setting was changed.
 */
export function isBlankCommandDraft(c: CommandDraft): boolean {
    return (
        c.commandLine.trim() === '' &&
        c.note === '' &&
        c.patterns.length === 0 &&
        c.onProblems === 'stop' &&
        c.timeoutSec === String(DEFAULT_TIMEOUT_SEC) &&
        c.lineLimit === String(DEFAULT_LINE_LIMIT) &&
        c.success.kind === 'single' &&
        c.success.a === '0' &&
        c.failure.kind === 'min' &&
        c.failure.a === '1'
    );
}

function pad(n: number): string {
    return String(n).padStart(2, '0');
}

/** RFC3339 time to a datetime-local value in the browser's time zone. */
function toLocalInput(value: string | undefined): string {
    if (!value) return '';
    const d = new Date(value);
    if (Number.isNaN(d.getTime())) return '';
    return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

/** A datetime-local value to RFC3339 with the browser's UTC offset; '' when invalid. */
export function localInputToOffsetTime(value: string): string {
    const d = new Date(value);
    if (!value || Number.isNaN(d.getTime())) return '';
    const off = -d.getTimezoneOffset();
    const sign = off >= 0 ? '+' : '-';
    const abs = Math.abs(off);
    return (
        `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}:00` +
        `${sign}${pad(Math.floor(abs / 60))}:${pad(abs % 60)}`
    );
}

export function newSetDraft(osUser: string): SetDraft {
    return {
        name: '',
        description: '',
        purpose: 'collect_output',
        osUser,
        workDir: '',
        runMode: 'manual',
        kind: 'daily',
        onceAt: '',
        onceSaved: null,
        weekdays: ['mon'],
        time: '03:00',
        everyHours: 1,
        everyMinutes: 10,
        minute: '0',
        keepRuns: String(DEFAULT_KEEP_RUNS),
        joiner: 'comma',
        commands: [newCommandDraft()],
    };
}

export function setDraftFrom(set: CommandSet): SetDraft {
    const d = newSetDraft(set.os_user);
    const sc = set.schedule;
    return {
        ...d,
        name: set.name,
        description: set.description,
        purpose: set.purpose,
        workDir: set.work_dir,
        runMode: set.run_mode,
        kind: sc?.kind ?? d.kind,
        onceAt: sc?.kind === 'once' ? toLocalInput(sc.at) : '',
        onceSaved: sc?.kind === 'once' && sc.at ? { at: sc.at, input: toLocalInput(sc.at) } : null,
        weekdays: sc?.weekdays?.length ? sc.weekdays : d.weekdays,
        time: sc?.time || d.time,
        everyHours: sc?.kind === 'every_hours' && sc.every ? sc.every : d.everyHours,
        everyMinutes: sc?.kind === 'every_minutes' && sc.every ? sc.every : d.everyMinutes,
        minute: sc?.kind === 'hourly_at' ? String(sc.minute ?? 0) : d.minute,
        keepRuns: String(set.keep_runs),
        joiner: set.joiner,
        commands: (set.commands ?? []).map(c => ({
            key: newKey(),
            commandLine: c.command_line,
            note: c.note,
            success: codeDraft(c.success, { kind: 'single', a: 0 }),
            failure: codeDraft(c.failure, { kind: 'min', a: 1 }),
            onProblems: c.on_problems,
            timeoutSec: String(c.timeout_sec),
            lineLimit: String(c.line_limit),
            patterns: (c.patterns ?? []).map(p => ({ key: newKey(), value: p })),
        })),
    };
}

/** A typed number; an empty or invalid field becomes NaN (sent as null; see draftProblems). */
function num(s: string): number {
    return s.trim() === '' ? NaN : Number(s);
}

/** Whether a typed number is a whole number (the server takes whole numbers only). */
function isWhole(s: string): boolean {
    return /^\s*\d+\s*$/.test(s);
}

function codeSpec(c: CodeDraft): CodeSpec {
    return c.kind === 'range' ? { kind: c.kind, a: num(c.a), b: num(c.b) } : { kind: c.kind, a: num(c.a) };
}

/** The schedule entered in the form (null for manual runs). */
export function draftSchedule(d: SetDraft): CommandSchedule | null {
    if (d.runMode === 'manual') return null;
    switch (d.kind) {
        case 'once':
            return {
                kind: 'once',
                at: d.onceSaved && d.onceSaved.input === d.onceAt ? d.onceSaved.at : localInputToOffsetTime(d.onceAt),
            };
        case 'weekly':
            return { kind: 'weekly', weekdays: WEEKDAYS.filter(w => d.weekdays.includes(w)), time: d.time };
        case 'daily':
            return { kind: 'daily', time: d.time };
        case 'every_hours':
            return { kind: 'every_hours', every: d.everyHours };
        case 'every_minutes':
            return { kind: 'every_minutes', every: d.everyMinutes };
        case 'hourly_at':
            return { kind: 'hourly_at', minute: num(d.minute) };
    }
}

/**
 * The definition sent to the server. Patterns and line limits are used only
 * when output is collected: for "run only" no patterns are sent (the form
 * keeps them in case the purpose is switched back) and the hidden line limit
 * is sent as it was, or as the default when it is not a whole number.
 */
export function draftToSpec(d: SetDraft): CommandSetSpec {
    const collect = d.purpose === 'collect_output';
    return {
        name: d.name.trim(),
        description: d.description,
        purpose: d.purpose,
        os_user: d.osUser.trim(),
        work_dir: d.workDir.trim(),
        run_mode: d.runMode,
        schedule: draftSchedule(d),
        keep_runs: num(d.keepRuns),
        joiner: d.joiner,
        commands: d.commands.map(c => ({
            command_line: c.commandLine,
            note: c.note,
            success: codeSpec(c.success),
            failure: codeSpec(c.failure),
            on_problems: c.onProblems,
            timeout_sec: num(c.timeoutSec),
            line_limit: collect || isWhole(c.lineLimit) ? num(c.lineLimit) : DEFAULT_LINE_LIMIT,
            patterns: collect ? c.patterns.map(p => p.value) : [],
        })),
    };
}

/**
 * Problems the form finds before the server is asked: number fields that are
 * empty or not whole numbers (JSON would send them as null, which the server
 * takes as "use the default") and a missing once time. Ranges and everything
 * else are checked by the server.
 */
export function draftProblems(t: TFunction, d: SetDraft): string[] {
    const out: string[] = [];
    const whole = (value: string, field: string) => {
        if (!isWhole(value)) out.push(t('commandSets.local.wholeNumber', { field }));
    };
    whole(d.keepRuns, t('commandSets.fields.keepRuns'));
    if (d.runMode === 'schedule') {
        if (d.kind === 'once' && localInputToOffsetTime(d.onceAt) === '')
            out.push(t('commandSets.local.onceAt', { field: t('commandSets.editor.onceAt') }));
        if (d.kind === 'hourly_at') whole(d.minute, t('commandSets.editor.minuteOfHour'));
    }
    d.commands.forEach((c, i) => {
        const prefix = (p: string) => t('commandSets.local.inCommand', { n: i + 1, problem: p });
        const before = out.length;
        whole(c.timeoutSec, t('commandSets.fields.timeoutSec'));
        if (d.purpose === 'collect_output') whole(c.lineLimit, t('commandSets.fields.lineLimit'));
        for (const [spec, label] of [
            [c.success, t('commandSets.fields.success')],
            [c.failure, t('commandSets.fields.failure')],
        ] as const) {
            whole(spec.a, label);
            if (spec.kind === 'range') whole(spec.b, label);
        }
        for (let k = before; k < out.length; k++) out[k] = prefix(out[k]);
    });
    return out;
}

/** Whether a code spec includes the exit code (an incomplete spec includes nothing). */
function includes(c: CodeDraft, code: number): boolean {
    const a = num(c.a);
    const b = num(c.b);
    if (Number.isNaN(a)) return false;
    switch (c.kind) {
        case 'single':
            return code === a;
        case 'range':
            return !Number.isNaN(b) && code >= a && code <= b;
        case 'min':
            return code >= a;
        case 'max':
            return code <= a;
    }
}

/** Ranges of the codes 0 to 255 for which pick returns true, such as "2-125, 127". */
function codeRanges(pick: (code: number) => boolean): string {
    const parts: string[] = [];
    let start = -1;
    for (let code = 0; code <= 256; code++) {
        const on = code <= 255 && pick(code);
        if (on && start < 0) start = code;
        if (!on && start >= 0) {
            parts.push(start === code - 1 ? String(start) : `${start}-${code - 1}`);
            start = -1;
        }
    }
    return parts.join(', ');
}

/** Whether a code spec is complete and accepted by the server (whole numbers 0 to 255, a range from low to high). */
export function codeDraftValid(c: CodeDraft): boolean {
    const inRange = (s: string) => isWhole(s) && num(s) <= 255;
    if (!inRange(c.a)) return false;
    return c.kind !== 'range' || (inRange(c.b) && num(c.b) > num(c.a));
}

/** Exit codes that would be errors (in neither spec) and codes in both specs. */
export function codeCoverage(success: CodeDraft, failure: CodeDraft): { errors: string; overlap: string } {
    return {
        errors: codeRanges(code => !includes(success, code) && !includes(failure, code)),
        overlap: codeRanges(code => includes(success, code) && includes(failure, code)),
    };
}

/** "UTC+09:00" from an offset in seconds. */
export function formatUtcOffset(sec: number): string {
    const sign = sec < 0 ? '-' : '+';
    const abs = Math.abs(sec);
    return `UTC${sign}${pad(Math.floor(abs / 3600))}:${pad(Math.floor((abs % 3600) / 60))}`;
}

/** The host's time zone, such as "JST, UTC+09:00", or '' when the agent never reported it. */
export function hostZoneText(zone: string, offsetSec: number, known: boolean): string {
    if (!known) return '';
    return zone ? `${zone}, ${formatUtcOffset(offsetSec)}` : formatUtcOffset(offsetSec);
}

/**
 * One sentence that says when the set runs. Recurring schedules run in the
 * host's local time, which is named; a once time is shown in this browser's
 * local time.
 */
export function scheduleSummary(t: TFunction, schedule: CommandSchedule | null, hostZone: string): string {
    if (!schedule) return t('commandSets.summary.manual');
    const zone = hostZone
        ? t('commandSets.summary.hostZone', { zone: hostZone })
        : t('commandSets.summary.hostZoneUnknown');
    switch (schedule.kind) {
        case 'once': {
            const d = schedule.at ? new Date(schedule.at) : null;
            if (!d || Number.isNaN(d.getTime())) return t('commandSets.summary.onceUnset');
            return t('commandSets.summary.once', { at: toLocalInput(schedule.at).replace('T', ' ') });
        }
        case 'weekly': {
            const days = (schedule.weekdays ?? [])
                .map(w => t(`commandSets.weekdays.${w}`))
                .join(t('commandSets.summary.listSeparator'));
            return t('commandSets.summary.weekly', { days, time: schedule.time ?? '', zone });
        }
        case 'daily':
            return t('commandSets.summary.daily', { time: schedule.time ?? '', zone });
        case 'every_hours':
            return schedule.every === 1
                ? t('commandSets.summary.everyHour', { zone })
                : t('commandSets.summary.everyHours', { n: schedule.every ?? 0, zone });
        case 'every_minutes':
            return schedule.every === 1
                ? t('commandSets.summary.everyMinute', { zone })
                : t('commandSets.summary.everyMinutes', { n: schedule.every ?? 0, zone });
        case 'hourly_at':
            return t('commandSets.summary.hourlyAt', { minute: schedule.minute ?? 0, zone });
    }
}
