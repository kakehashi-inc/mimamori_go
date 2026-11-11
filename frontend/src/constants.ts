// Tunable frontend settings collected in one place so they are easy to find.

// --- Language ---

// Languages the UI ships translations for (see src/i18n/). Each account has
// one of them as its display language; English is the default.
export const SUPPORTED_LANGS = ['en', 'ja'] as const;
export type Lang = (typeof SUPPORTED_LANGS)[number];
export const DEFAULT_LANG: Lang = 'en';

// localStorage key that remembers the language last used in this browser, so
// the login screen opens in it. After login the account's language applies.
export const LANG_STORAGE_KEY = 'mimamori.lang';

// --- Accounts ---

// Minimum password length enforced by the server.
export const MIN_PASSWORD_LENGTH = 10;
