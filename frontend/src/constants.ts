// Tunable frontend settings collected in one place so they are easy to find.

// --- Language ---

// Languages the UI ships translations for (see src/i18n/). English is the
// fallback when the browser language is not supported.
export const SUPPORTED_LANGS = ['en', 'ja'] as const;
export type Lang = (typeof SUPPORTED_LANGS)[number];
export const DEFAULT_LANG: Lang = 'en';

// Display language settings stored on each account. 'auto' (the default)
// follows the language of the browser.
export const LANG_SETTINGS = ['auto', 'en', 'ja'] as const;
export type LangSetting = (typeof LANG_SETTINGS)[number];

// --- Accounts ---

// Minimum password length enforced by the server.
export const MIN_PASSWORD_LENGTH = 10;

// Maximum length of a display name enforced by the server (characters, after trimming).
export const MAX_DISPLAY_NAME_LENGTH = 64;
