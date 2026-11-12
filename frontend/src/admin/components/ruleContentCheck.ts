// Strict check of rule content typed in the JSON tab of the rule editor. The
// editor turns the JSON into a RuleContent by filling missing fields
// (normalizeRuleContent), which would silently drop unknown keys, values of
// the wrong type, unknown category or if_missing values and all but the last
// of keys written twice. This check finds every such part first, so that the
// editor can name it and refuse the text, as the server refuses it (unknown
// fields and wrong types are rejected there).

/** What a field must hold. null is accepted everywhere: the server reads it as the empty value. */
type Spec =
    | { kind: 'object'; fields: Record<string, Spec> }
    | { kind: 'list'; item: Spec }
    | { kind: 'string' }
    | { kind: 'int' }
    | { kind: 'bool' }
    | { kind: 'category' }
    | { kind: 'ifMissing' };

/** Kind of the value expected, used to pick the text of a problem. */
export type Expected = 'object' | 'list' | 'string' | 'int' | 'bool' | 'category' | 'ifMissing';

export type ContentProblem =
    { path: string; problem: 'unknown' | 'duplicate' } | { path: string; problem: 'type'; expected: Expected };

const STRING: Spec = { kind: 'string' };
const CATEGORY: Spec = { kind: 'category' };
const obj = (fields: Record<string, Spec>): Spec => ({ kind: 'object', fields });
const list = (item: Spec): Spec => ({ kind: 'list', item });
const ELEMENT = obj({ value: STRING, if_missing: { kind: 'ifMissing' } });

const RULE_CONTENT = obj({
    users: list(ELEMENT),
    exclude_interactive: { kind: 'bool' },
    check_interval_sec: { kind: 'int' },
    scan_interval_sec: { kind: 'int' },
    exec: obj({ category: CATEGORY }),
    denied: obj({ category: CATEGORY }),
    read: obj({ dirs: list(ELEMENT), category: CATEGORY }),
    network: obj({ executables: list(ELEMENT), category: CATEGORY }),
    writable: obj({ base: STRING, allowed_paths: list(STRING), category: CATEGORY }),
    exec_files: obj({
        extensions: list(STRING),
        legit: list(obj({ path: STRING, extensions: list(STRING), category: CATEGORY })),
        other_category: CATEGORY,
    }),
});

function walk(v: unknown, spec: Spec, path: string, out: ContentProblem[]): void {
    if (v === null) return;
    const wrong = () => {
        out.push({ path, problem: 'type', expected: spec.kind });
    };
    switch (spec.kind) {
        case 'object': {
            if (typeof v !== 'object' || Array.isArray(v)) {
                wrong();
                return;
            }
            for (const [k, item] of Object.entries(v as Record<string, unknown>)) {
                const p = path ? `${path}.${k}` : k;
                // Own keys only: a name such as "constructor" or "__proto__" is not a field.
                const field = Object.prototype.hasOwnProperty.call(spec.fields, k) ? spec.fields[k] : undefined;
                if (field) walk(item, field, p, out);
                else out.push({ path: p, problem: 'unknown' });
            }
            return;
        }
        case 'list':
            if (!Array.isArray(v)) {
                wrong();
                return;
            }
            v.forEach((item, i) => walk(item, spec.item, `${path}[${i}]`, out));
            return;
        case 'string':
            if (typeof v !== 'string') wrong();
            return;
        case 'int':
            // Beyond the safe range the parsed number is no longer the written one.
            if (typeof v !== 'number' || !Number.isSafeInteger(v)) wrong();
            return;
        case 'bool':
            if (typeof v !== 'boolean') wrong();
            return;
        case 'category':
            if (v !== '' && v !== 'alert' && v !== 'normal') wrong();
            return;
        case 'ifMissing':
            if (v !== '' && v !== 'ignore' && v !== 'warn') wrong();
            return;
    }
}

/** Every part of the parsed JSON that is not valid rule content (empty when all of it is). */
function contentProblems(parsed: unknown): ContentProblem[] {
    const out: ContentProblem[] = [];
    if (parsed === null || typeof parsed !== 'object' || Array.isArray(parsed)) {
        return [{ path: '', problem: 'type', expected: 'object' }];
    }
    walk(parsed, RULE_CONTENT, '', out);
    return out;
}

type Frame = { obj: true; path: string; keys: Set<string>; key: string } | { obj: false; path: string; index: number };

/**
 * Paths of the keys written more than once in one object of the JSON text
 * (the text must parse). Only the last of them would be kept.
 */
function duplicateKeys(text: string): string[] {
    const out: string[] = [];
    const stack: Frame[] = [];
    const childPath = (): string => {
        const top = stack[stack.length - 1];
        if (!top) return '';
        if (top.obj) return top.path ? `${top.path}.${top.key}` : top.key;
        return `${top.path}[${top.index}]`;
    };
    let i = 0;
    while (i < text.length) {
        const c = text[i];
        if (c === '"') {
            let j = i + 1;
            while (text[j] !== '"') j += text[j] === '\\' ? 2 : 1;
            const raw = text.slice(i, j + 1);
            i = j + 1;
            let k = i;
            while (k < text.length && /\s/.test(text[k])) k++;
            const top = stack[stack.length - 1];
            // In valid JSON a string followed by a colon is a key.
            if (top?.obj && text[k] === ':') {
                top.key = JSON.parse(raw) as string;
                if (top.keys.has(top.key)) out.push(top.path ? `${top.path}.${top.key}` : top.key);
                top.keys.add(top.key);
            }
            continue;
        }
        if (c === '{' || c === '[') {
            const path = childPath();
            stack.push(c === '{' ? { obj: true, path, keys: new Set(), key: '' } : { obj: false, path, index: 0 });
        } else if (c === '}' || c === ']') {
            stack.pop();
        } else if (c === ',') {
            const top = stack[stack.length - 1];
            if (top && !top.obj) top.index++;
        }
        i++;
    }
    return out;
}

/**
 * Every part of the JSON text of the editor that would be lost or changed by
 * taking it over: keys written twice, unknown keys, values of the wrong type
 * and unknown category or if_missing values (empty when the text is valid
 * rule content). `parsed` is JSON.parse(text).
 */
export function ruleContentProblems(text: string, parsed: unknown): ContentProblem[] {
    const dup: ContentProblem[] = duplicateKeys(text).map(path => ({ path, problem: 'duplicate' }));
    return [...dup, ...contentProblems(parsed)];
}
