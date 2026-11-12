// Typed translation keys: t('home.greeting') is checked against the texts, so
// a typo or a missing key fails "yarn lint" instead of rendering the raw key.
//
// The texts are split: i18n/*.json is bundled with the application, and
// admin/i18n/*.json is added at run time by the administrator extension
// (registerExtension). The key type is the deep merge of both Japanese files.
// This file only imports types, so the administrator texts are not bundled
// with the application; a key of the administrator file used outside admin/
// would type-check but show the raw key to accounts without the extension:
// keep administrator keys inside admin/.
import 'i18next';
import type adminJa from '../admin/i18n/ja.json';
import type ja from './ja.json';

/** Deep merge of two text trees (sections present in both are merged). */
type Merge<A, B> = {
    [K in keyof A | keyof B]: K extends keyof A
        ? K extends keyof B
            ? A[K] extends object
                ? B[K] extends object
                    ? Merge<A[K], B[K]>
                    : A[K]
                : A[K]
            : A[K]
        : K extends keyof B
          ? B[K]
          : never;
};

declare module 'i18next' {
    interface CustomTypeOptions {
        defaultNS: 'translation';
        resources: {
            translation: Merge<typeof ja, typeof adminJa>;
        };
    }
}
