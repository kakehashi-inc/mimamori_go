import i18n from 'i18next';
import { initReactI18next } from 'react-i18next';
import { DEFAULT_LANG, SUPPORTED_LANGS, type Lang, type LangSetting } from '../constants';
import en from './en.json';
import ja from './ja.json';

function isLang(value: unknown): value is Lang {
    return SUPPORTED_LANGS.includes(value as Lang);
}

/** The first supported language among the browser's preferred languages, otherwise English. */
export function browserLang(): Lang {
    const candidates = typeof navigator !== 'undefined' ? navigator.languages : [];
    for (const tag of candidates) {
        const base = tag.toLowerCase().split('-')[0];
        if (isLang(base)) return base;
    }
    return DEFAULT_LANG;
}

/** Resolves an account's language setting ('auto' follows the browser). */
export function resolveLang(setting: LangSetting): Lang {
    return setting === 'auto' ? browserLang() : setting;
}

// Nothing is stored in the browser: before login the browser language is
// used, after login the language setting of the account.
i18n.use(initReactI18next).init({
    resources: { ja: { translation: ja }, en: { translation: en } },
    lng: browserLang(),
    fallbackLng: DEFAULT_LANG,
    supportedLngs: [...SUPPORTED_LANGS],
    interpolation: { escapeValue: false }, // React already escapes
});

// Keep <html lang> in sync for screen readers, hyphenation and font selection.
function applyDocumentLang(lng: string) {
    document.documentElement.lang = lng;
}
applyDocumentLang(i18n.language);
i18n.on('languageChanged', applyDocumentLang);

/** Applies a language setting to the UI. */
export function applyLangSetting(setting: LangSetting): Promise<unknown> {
    return i18n.changeLanguage(resolveLang(setting));
}

export default i18n;
