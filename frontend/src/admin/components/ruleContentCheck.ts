// Strict check of rule content typed in the JSON tab of the rule editor. The
// editor turns the JSON into a RuleContent by filling missing fields
// (normalizeRuleContent), which would silently drop unknown keys, values of
// the wrong type and unknown category or if_missing values. This check finds
// every such part first, so that the editor can name it and refuse the text,
// as the server refuses it (unknown fields and wrong types are rejected there).

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
    { path: string; problem: 'unknown' } | { path: string; problem: 'type'; expected: Expected };

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
                const field = spec.fields[k];
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
            if (typeof v !== 'number' || !Number.isInteger(v)) wrong();
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
export function ruleContentProblems(parsed: unknown): ContentProblem[] {
    const out: ContentProblem[] = [];
    if (parsed === null || typeof parsed !== 'object' || Array.isArray(parsed)) {
        return [{ path: '', problem: 'type', expected: 'object' }];
    }
    walk(parsed, RULE_CONTENT, '', out);
    return out;
}
