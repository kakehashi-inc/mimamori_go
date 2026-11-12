import type { TFunction } from 'i18next';
import { ApiError } from '../api/client';
import { errorMessage } from './format';

// Fixed error texts of the server for account fields (see the API
// specification in Documents/), shown translated instead of in English.
const EMAIL_INVALID = [
    'an e-mail address is required',
    'the e-mail address must be at most 254 characters',
    'the e-mail address must not contain spaces, control characters or " < > ,',
    'the e-mail address is not valid',
];
const DISPLAY_NAME_INVALID = [
    'a display name is required',
    'the display name must be at most 64 characters',
    'the display name must not contain control characters',
];
const DUPLICATE_EMAIL = 'an account with this e-mail address already exists';
const EMAIL_UNCHANGED = 'the e-mail address is unchanged';
const CURRENT_PASSWORD_WRONG = 'the current password is wrong';
const CODE_MAIL_FAILED = 'the verification code could not be sent; contact an administrator';

/** The server could not mail a verification code (503; a busy server also answers 503). */
export function isCodeMailFailed(e: unknown): boolean {
    return e instanceof ApiError && e.status === 503 && e.message === CODE_MAIL_FAILED;
}

/** A user-facing message for an error of an account or profile request. */
export function accountErrorMessage(t: TFunction, e: unknown): string {
    if (e instanceof ApiError) {
        if (e.status === 400 && EMAIL_INVALID.includes(e.message)) return t('accountErrors.emailInvalid');
        if (e.status === 400 && DISPLAY_NAME_INVALID.includes(e.message)) return t('accountErrors.displayNameInvalid');
        if (e.status === 400 && e.message === EMAIL_UNCHANGED) return t('accountErrors.emailUnchanged');
        if (e.status === 400 && e.message === CURRENT_PASSWORD_WRONG) return t('accountErrors.wrongCurrentPassword');
        if (e.status === 409 && e.message === DUPLICATE_EMAIL) return t('accountErrors.emailDuplicate');
        if (isCodeMailFailed(e)) return t('code.mailFailed');
    }
    return errorMessage(t, e);
}
