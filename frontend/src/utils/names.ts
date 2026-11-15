// Names typed by a person (rule, template, token, display, command set and import
// names) are sent to the server as typed: the server takes the white space
// around a name away only when the name holds no control character, and
// refuses a name with a control character, also at its start or end (a tab, a
// line break, U+0085). The screens judge a typed name by the same rule, so
// that what they decide locally (whether a name is empty, unchanged or taken)
// is what the server will decide.

// Control characters as the server counts them: U+0000-U+001F and U+007F-U+009F.
const CONTROL = /\p{Cc}/u;
// White space as the server counts it (the Unicode White_Space property).
const EDGE_SPACE = /^\p{White_Space}+|\p{White_Space}+$/gu;

/** The name the server makes of a typed text: trimmed when it holds no control character, else as typed. */
export function trimName(text: string): string {
    return CONTROL.test(text) ? text : text.replace(EDGE_SPACE, '');
}
