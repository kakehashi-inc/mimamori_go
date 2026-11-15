// The number rule of the program for the numbers a person types (system
// specification, chapter 5): a whole number is written with the digits 0 to
// 9 only and read as a decimal number ("010" is ten); a sign, a space, a
// point, an exponent or any other character makes the text no number. The
// typed text is judged as it is; nothing in it is changed.

const DIGITS = /^[0-9]+$/;

/** The value of a typed whole number, or null when the text is not one (an empty text included). */
export function wholeNumber(text: string): number | null {
    return DIGITS.test(text) ? Number(text) : null;
}

/** Whether a typed text is a whole number from min to max. */
export function wholeNumberIn(text: string, min: number, max: number): boolean {
    const n = wholeNumber(text);
    return n !== null && n >= min && n <= max;
}
