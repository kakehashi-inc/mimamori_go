// Splitting of pasted text into the commands of a command set, the way bash
// reads text typed line by line: a line that bash would continue on the next
// line (a backslash at its end, an open quote, parenthesis, brace group,
// compound command or here-document, a function header without its body, or
// a trailing |, || or &&) stays one command with the following lines; every
// other line is one command. Blank lines and comment lines between commands
// are skipped.
//
// The scanner makes one pass over the characters. Its state is a stack of
// frames (shell code, or a kind of quote or expansion), the here-documents
// whose bodies start at the next line end, and per code frame the word being
// read, the open compound commands and whether a command can begin there.

/** A compound command that is still open in a code frame. */
type Block =
    | { kind: 'if' | 'loop' | 'group' }
    /**
     * case: 'word' before "in", 'pattern' before ")", 'body' after it.
     * patternStart: the next word is the first of a pattern list, where
     * "esac" ends the case ("a|esac)" and "(esac)" are patterns).
     */
    | { kind: 'case'; phase: 'word' | 'pattern' | 'body'; patternStart: boolean };

/** Shell code: the text itself, ( ... ), $( ... ) or <( ... ), and ` ... `. */
interface CodeFrame {
    kind: 'top' | 'subshell' | 'cmdsub' | 'backtick';
    blocks: Block[];
    /** The word being read (null between words); '\0' marks quotes, escapes and expansions. */
    word: string | null;
    /** The previous word ("" after an operator), to read "for ((" as arithmetic. */
    lastWord: string;
    /** A command (or a reserved word) can begin at the next word. */
    canBegin: boolean;
    /**
     * The command goes on after a line end: the last token was |, |&, || or
     * &&, or a function header ("f()", "function f") waits for its body.
     */
    needCommand: boolean;
    /** The next word is the target of a redirection. */
    redirect: boolean;
    /** The next word is the name after "function". */
    funcName: boolean;
    /** Nothing was read in the frame yet: "f()" is followed by a function body. */
    empty: boolean;
    /** A subshell right after a word that is not an assignment: "f()" is a function header, "a=()" is not. */
    afterName: boolean;
}

/** Quotes and expansions: '...', $'...', "...", ${...} and $((...)) or ((...)). */
interface QuoteFrame {
    kind: 'single' | 'ansi' | 'double' | 'param' | 'arith';
    /** Open parentheses inside an arithmetic expression. */
    depth: number;
}

type Frame = CodeFrame | QuoteFrame;

interface HereDoc {
    delimiter: string;
    /** <<- : leading tabs of the end line are ignored. */
    stripTabs: boolean;
    /** The word was quoted: a backslash at the end of a body line does not join the next line. */
    quoted: boolean;
}

const OPERATOR_CHARS = ';&|<>()';

function codeFrame(kind: CodeFrame['kind']): CodeFrame {
    return {
        kind,
        blocks: [],
        word: null,
        lastWord: '',
        canBegin: true,
        needCommand: false,
        redirect: false,
        funcName: false,
        empty: true,
        afterName: false,
    };
}

function isCode(f: Frame): f is CodeFrame {
    return f.kind === 'top' || f.kind === 'subshell' || f.kind === 'cmdsub' || f.kind === 'backtick';
}

function isBlank(c: string): boolean {
    return c === ' ' || c === '\t';
}

/** NAME=, NAME+= or NAME[...]= at the start of a word: a command can still follow. */
function isAssignment(word: string): boolean {
    return /^[A-Za-z_][A-Za-z0-9_]*(\[[^\]]*\])?\+?=/.test(word);
}

/** The line ends with a backslash that is not itself escaped. */
function endsWithEscape(line: string): boolean {
    let n = 0;
    while (n < line.length && line[line.length - 1 - n] === '\\') n++;
    return n % 2 === 1;
}

class Scanner {
    private readonly src: string;
    private i = 0;
    private stack: Frame[] = [codeFrame('top')];
    private hereDocs: HereDoc[] = [];
    /** Where the current command begins. */
    private start = 0;
    /** The current command has something other than blanks and comments. */
    private content = false;
    /** Where the last backslash that continues a line in shell code is. */
    private continuation = -1;
    readonly commands: string[] = [];

    constructor(text: string) {
        this.src = text.replace(/\r\n?/g, '\n');
    }

    run(): string[] {
        while (this.i < this.src.length) {
            const f = this.top();
            if (isCode(f)) this.stepCode(f);
            else this.stepQuoted(f);
        }
        // The end of the text: what is left is the last command, open or not.
        const f = this.top();
        if (isCode(f)) this.endWord(f);
        this.emit(this.src.length);
        return this.commands;
    }

    private top(): Frame {
        return this.stack[this.stack.length - 1];
    }

    private at(offset: number): string {
        return this.src[this.i + offset] ?? '';
    }

    /** Ends the command at end: blanks and line ends around it are removed. */
    private emit(end: number) {
        const src = this.src;
        let a = this.start;
        let b = end;
        while (a < b && (isBlank(src[a]) || src[a] === '\n')) a++;
        while (b > a && (isBlank(src[b - 1]) || src[b - 1] === '\n')) b--;
        // "a \" before a blank line: the line end stays, so that bash -c also reads it as a continuation.
        if (b - 1 === this.continuation && b < end) b++;
        if (this.content && a < b) this.commands.push(src.slice(a, b));
        this.start = end + 1;
        this.content = false;
        this.stack = [codeFrame('top')];
    }

    /** Enters a quote or expansion that is part of the current word. */
    private push(frame: Frame, skip: number) {
        const f = this.top();
        if (isCode(f)) {
            f.word = (f.word ?? '') + '\0';
            f.empty = false;
        }
        this.stack.push(frame);
        this.i += skip;
    }

    /** $( $(( ${ $' $" : enters the expansion and returns true, or returns false. */
    private dollar(inCode: boolean): boolean {
        const next = this.at(1);
        if (next === '(' && this.at(2) === '(') this.push({ kind: 'arith', depth: 0 }, 3);
        else if (next === '(') this.push(codeFrame('cmdsub'), 2);
        else if (next === '{') this.push({ kind: 'param', depth: 0 }, 2);
        else if (next === "'" && inCode) this.push({ kind: 'ansi', depth: 0 }, 2);
        else if (next === '"' && inCode) this.push({ kind: 'double', depth: 0 }, 2);
        else return false;
        return true;
    }

    private stepQuoted(f: QuoteFrame) {
        const c = this.at(0);
        if (f.kind === 'single') {
            if (c === "'") this.stack.pop();
            this.i++;
            return;
        }
        if (c === '\\') {
            this.i += 2;
            return;
        }
        if (f.kind === 'ansi') {
            if (c === "'") this.stack.pop();
            this.i++;
            return;
        }
        const close = { double: '"', param: '}', arith: ')' }[f.kind];
        if (f.kind === 'arith' && c === '(') {
            f.depth++;
            this.i++;
        } else if (f.kind === 'arith' && c === ')' && f.depth > 0) {
            f.depth--;
            this.i++;
        } else if (c === close) {
            this.stack.pop();
            // "))" ends an arithmetic expression.
            this.i += f.kind === 'arith' && this.at(1) === ')' ? 2 : 1;
        } else if (c === '`') {
            this.push(codeFrame('backtick'), 1);
        } else if (c === '$' && this.dollar(false)) {
            // entered
        } else if (c === '"' && f.kind !== 'double') {
            this.push({ kind: 'double', depth: 0 }, 1);
        } else if (c === "'" && f.kind !== 'double') {
            this.push({ kind: 'single', depth: 0 }, 1);
        } else {
            this.i++;
        }
    }

    private stepCode(f: CodeFrame) {
        const c = this.at(0);
        if (c === '\n') {
            this.lineEnd(f);
            return;
        }
        if (isBlank(c)) {
            this.endWord(f);
            this.i++;
            return;
        }
        if (c === '#' && f.word === null) {
            // A comment runs to the line end; a backslash in it continues nothing.
            while (this.i < this.src.length && this.at(0) !== '\n') this.i++;
            return;
        }
        this.content = true;
        if (c === '\\') {
            if (this.at(1) === '\n') {
                this.continuation = this.i;
                this.i += 2; // line continuation
            } else {
                f.word = (f.word ?? '') + '\0';
                f.empty = false;
                this.i += 2;
            }
            return;
        }
        if (c === "'") return this.push({ kind: 'single', depth: 0 }, 1);
        if (c === '"') return this.push({ kind: 'double', depth: 0 }, 1);
        if (c === '`') {
            if (f.kind === 'backtick') {
                this.endWord(f);
                this.stack.pop();
                this.i++;
            } else {
                this.push(codeFrame('backtick'), 1);
            }
            return;
        }
        if (c === '$' && this.dollar(true)) return;
        if (OPERATOR_CHARS.includes(c)) {
            this.endWord(f);
            if (c !== ')') f.empty = false;
            this.operator(f, c);
            return;
        }
        f.word = (f.word ?? '') + c;
        f.empty = false;
        this.i++;
    }

    /** A line end in shell code: here-document bodies, then the end of the command or a separator. */
    private lineEnd(f: CodeFrame) {
        this.endWord(f);
        if (this.hereDocs.length > 0) this.readHereDocBodies();
        const complete = this.stack.length === 1 && f.blocks.length === 0 && !f.needCommand;
        if (complete) {
            this.emit(this.i);
        } else {
            const block = f.blocks[f.blocks.length - 1];
            if (block?.kind !== 'case' || block.phase === 'body') f.canBegin = true;
            f.redirect = false;
        }
        this.i++;
    }

    /** Skips the here-document bodies after the line end at i; i ends at the line end of the last end line. */
    private readHereDocBodies() {
        let pos = this.i;
        const src = this.src;
        for (const doc of this.hereDocs) {
            for (;;) {
                if (pos >= src.length) break;
                const lineStart = pos + 1;
                let lineEnd = src.indexOf('\n', lineStart);
                if (lineEnd < 0) lineEnd = src.length;
                let line = src.slice(lineStart, lineEnd);
                if (doc.stripTabs) line = line.replace(/^\t+/, '');
                // Unquoted word: an unescaped backslash at the line end joins the next line.
                // Once the joined line is longer than the end word, it is no longer built.
                let part = line;
                while (!doc.quoted && lineEnd < src.length && endsWithEscape(part)) {
                    const next = src.indexOf('\n', lineEnd + 1);
                    const nextEnd = next < 0 ? src.length : next;
                    part = src.slice(lineEnd + 1, nextEnd);
                    if (line.length <= doc.delimiter.length + 1) line = line.slice(0, -1) + part;
                    lineEnd = nextEnd;
                }
                pos = lineEnd;
                if (line === doc.delimiter) break;
            }
        }
        // When the text ended inside a body, pos is its end and the rest is the last command.
        this.hereDocs = [];
        this.i = pos;
    }

    private operator(f: CodeFrame, c: string) {
        const block = f.blocks[f.blocks.length - 1];
        const casePattern = block?.kind === 'case' && block.phase === 'pattern';
        const next = this.at(1);
        const lastWord = f.lastWord;
        f.lastWord = '';
        switch (c) {
            case ';':
                if (next === ';' || next === '&') {
                    // ;; ;& ;;& end a case item.
                    this.i += next === ';' && this.at(2) === '&' ? 3 : 2;
                    if (block?.kind === 'case') {
                        block.phase = 'pattern';
                        block.patternStart = true;
                    }
                    f.canBegin = false;
                } else {
                    this.i++;
                    f.canBegin = true;
                }
                f.needCommand = false;
                return;
            case '&':
                if (next === '&') {
                    this.i += 2;
                    f.canBegin = true;
                    f.needCommand = true;
                } else if (next === '>') {
                    this.i += this.at(2) === '>' ? 3 : 2;
                    f.redirect = true;
                } else {
                    this.i++;
                    f.canBegin = true;
                    f.needCommand = false;
                }
                return;
            case '|':
                this.i += next === '|' || next === '&' ? 2 : 1;
                if (casePattern && next !== '|') {
                    // a|b) separates patterns
                    if (block?.kind === 'case') block.patternStart = false;
                    return;
                }
                f.canBegin = true;
                f.needCommand = true;
                return;
            case '<':
            case '>':
                return this.redirection(f, c, next);
            case '(':
                return this.openParen(f, block, next, lastWord);
            case ')':
                return this.closeParen(f, casePattern, block);
        }
    }

    private redirection(f: CodeFrame, c: string, next: string) {
        if (next === '(') {
            // <( ... ) and >( ... ): process substitution, part of a word.
            this.push(codeFrame('cmdsub'), 2);
            return;
        }
        if (c === '<' && next === '<' && this.at(2) !== '<') {
            this.i += 2;
            const stripTabs = this.at(0) === '-';
            if (stripTabs) this.i++;
            this.readHereDocWord(stripTabs);
            return;
        }
        // <<< (a here-string), >> >| >& <& <> and plain < >; the next word is the target.
        const twoChars = next === '>' || next === '|' || next === '&';
        this.i += c === '<' && next === '<' ? 3 : twoChars ? 2 : 1;
        f.redirect = true;
    }

    /** Reads the word after << and remembers its end line (quotes removed). */
    private readHereDocWord(stripTabs: boolean) {
        const src = this.src;
        while (isBlank(this.at(0))) this.i++;
        let delimiter = '';
        let quoted = false;
        while (this.i < src.length) {
            const c = this.at(0);
            if (isBlank(c) || c === '\n' || OPERATOR_CHARS.includes(c)) break;
            if (c === "'" || c === '"' || c === '\\') quoted = true;
            if (c === "'") {
                let end = src.indexOf("'", this.i + 1);
                if (end < 0) end = src.length;
                delimiter += src.slice(this.i + 1, end);
                this.i = end + 1;
            } else if (c === '"') {
                this.i++;
                while (this.i < src.length && this.at(0) !== '"') {
                    if (this.at(0) === '\\') this.i++;
                    delimiter += this.at(0);
                    this.i++;
                }
                this.i++;
            } else if (c === '\\') {
                delimiter += this.at(1);
                this.i += 2;
            } else {
                delimiter += c;
                this.i++;
            }
        }
        if (delimiter !== '') this.hereDocs.push({ delimiter, stripTabs, quoted });
    }

    private openParen(f: CodeFrame, block: Block | undefined, next: string, lastWord: string) {
        if (block?.kind === 'case' && block.phase === 'pattern') {
            this.i++; // "(a|b)": the optional parenthesis before a case pattern
            block.patternStart = false;
            return;
        }
        f.needCommand = false;
        if (next === '(' && (f.canBegin || lastWord === 'for')) {
            this.push({ kind: 'arith', depth: 0 }, 2); // (( ... )) and "for (( ... ))"
        } else {
            this.i++;
            const sub = codeFrame('subshell');
            sub.afterName = lastWord !== '' && !isAssignment(lastWord);
            this.stack.push(sub);
        }
    }

    private closeParen(f: CodeFrame, casePattern: boolean, block: Block | undefined) {
        this.i++;
        if (casePattern && block?.kind === 'case') {
            block.phase = 'body';
            f.canBegin = true;
            return;
        }
        if (f.kind !== 'subshell' && f.kind !== 'cmdsub') return; // a stray ")"
        this.stack.pop();
        const parent = this.top();
        if (f.kind === 'subshell' && isCode(parent)) {
            // "name()" is followed by the function body, such as "{", also on a later line.
            parent.canBegin = f.empty;
            if (f.empty && f.afterName) parent.needCommand = true;
        }
    }

    /** Ends the word being read; reserved words are recognized where a command can begin. */
    private endWord(f: CodeFrame) {
        if (f.word === null) return;
        const word = f.word;
        f.word = null;
        f.lastWord = word;
        if (f.redirect) {
            f.redirect = false;
            return;
        }
        f.needCommand = false;
        const block = f.blocks[f.blocks.length - 1];
        if (block?.kind === 'case' && block.phase !== 'body') {
            if (block.phase === 'word' && word === 'in') {
                block.phase = 'pattern';
                block.patternStart = true;
            } else if (block.phase === 'pattern') {
                if (word === 'esac' && block.patternStart) f.blocks.pop();
                else block.patternStart = false;
            }
            return;
        }
        if (f.funcName) {
            // "function f": the body follows, also on a later line.
            f.funcName = false;
            f.canBegin = true;
            f.needCommand = true;
            return;
        }
        if (!f.canBegin) return;
        f.canBegin = false;
        const close = (kind: Block['kind']) => {
            if (block?.kind === kind) f.blocks.pop();
        };
        switch (word) {
            case 'if':
                f.blocks.push({ kind: 'if' });
                f.canBegin = true;
                break;
            case 'while':
            case 'until':
                f.blocks.push({ kind: 'loop' });
                f.canBegin = true;
                break;
            case 'for':
            case 'select':
                f.blocks.push({ kind: 'loop' });
                break;
            case 'case':
                f.blocks.push({ kind: 'case', phase: 'word', patternStart: false });
                break;
            case '{':
                f.blocks.push({ kind: 'group' });
                f.canBegin = true;
                break;
            case 'then':
            case 'else':
            case 'elif':
            case 'do':
            case '!':
            case 'time':
                f.canBegin = true;
                break;
            case 'fi':
                close('if');
                break;
            case 'done':
                close('loop');
                break;
            case 'esac':
                close('case');
                break;
            case '}':
                close('group');
                break;
            case 'function':
                f.funcName = true;
                break;
            default:
                if (isAssignment(word)) f.canBegin = true;
        }
    }
}

/**
 * Splits pasted text into commands: lines that bash would continue stay one
 * command with their line breaks as pasted, every other line is one command,
 * and blank lines and comment lines between commands are skipped. Blanks
 * around each command are removed, except the line end after a final
 * continuing backslash. When the text ends while something is
 * still open, the remaining lines are the last command.
 */
export function splitShellCommands(text: string): string[] {
    return new Scanner(text).run();
}
