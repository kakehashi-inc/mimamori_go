import type { TFunction } from 'i18next';
import { ApiError } from '../api/client';
import { errorMessage } from './format';

// Codes of the server's errors for account fields (see the API specification
// in Documents/) that have a friendlier text of their own here.
const EMAIL_INVALID = [
    'account.email_required',
    'account.email_too_long',
    'account.email_chars',
    'account.email_invalid',
];
const DISPLAY_NAME_INVALID = [
    'account.display_name_required',
    'account.display_name_too_long',
    'account.display_name_control',
];

function errorCode(e: ApiError): string {
    return e.i18n?.code ?? '';
}

/** The server could not mail a verification code (503; a busy server also answers 503). */
export function isCodeMailFailed(e: unknown): boolean {
    return e instanceof ApiError && e.status === 503 && errorCode(e) === 'auth.code_mail_failed';
}

/** A user-facing message for an error of an account or profile request. */
export function accountErrorMessage(t: TFunction, e: unknown): string {
    if (e instanceof ApiError) {
        const code = errorCode(e);
        if (e.status === 400 && EMAIL_INVALID.includes(code)) return t('accountErrors.emailInvalid');
        if (e.status === 400 && DISPLAY_NAME_INVALID.includes(code)) return t('accountErrors.displayNameInvalid');
        if (e.status === 400 && code === 'account.email_unchanged') return t('accountErrors.emailUnchanged');
        if (e.status === 400 && code === 'account.current_password_wrong') {
            return t('accountErrors.wrongCurrentPassword');
        }
        if (e.status === 409 && code === 'account.email_duplicate') return t('accountErrors.emailDuplicate');
        if (isCodeMailFailed(e)) return t('code.mailFailed');
    }
    return errorMessage(t, e);
}
