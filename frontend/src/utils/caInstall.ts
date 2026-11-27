/**
 * Install procedures of a CA certificate that differ by OS and browser.
 * Firefox on a computer keeps its own certificate store on every OS, so it
 * has one procedure; on iOS and iPadOS every browser uses the system's
 * trust, so they share one.
 */
export type InstallGuide =
    'windows' | 'macos' | 'linux' | 'chromeos' | 'firefox' | 'ios' | 'android' | 'androidFirefox';

/** Every procedure, in the order they are listed. */
export const ALL_GUIDES: InstallGuide[] = [
    'windows',
    'macos',
    'linux',
    'chromeos',
    'firefox',
    'ios',
    'android',
    'androidFirefox',
];

export interface DetectedEnv {
    /**
     * The procedures that may apply to this browser: one when the OS and the
     * browser are known; macOS and iOS for Safari reporting a Mac, which
     * iPadOS also does; empty when nothing is known.
     */
    guides: InstallGuide[];
    /** iOS or iPadOS in a browser other than Safari, which cannot install profiles. */
    iosNotSafari: boolean;
}

/**
 * Detects the install procedure from a User-Agent string. Touch support is
 * not used: Macs with touch screens would look like iPads.
 */
export function detectInstallEnv(ua: string): DetectedEnv {
    const iosBrowser = /CriOS|FxiOS|EdgiOS/.test(ua);
    if (iosBrowser || /iPhone|iPad|iPod/.test(ua)) {
        const safari = !iosBrowser && /Version\/[\d.]+.*Safari\//.test(ua);
        return { guides: ['ios'], iosNotSafari: !safari };
    }
    if (/Android/.test(ua)) {
        return { guides: [/Firefox\//.test(ua) ? 'androidFirefox' : 'android'], iosNotSafari: false };
    }
    if (/CrOS/.test(ua)) return { guides: ['chromeos'], iosNotSafari: false };
    if (/Firefox\//.test(ua)) return { guides: ['firefox'], iosNotSafari: false };
    if (/Windows/.test(ua)) return { guides: ['windows'], iosNotSafari: false };
    if (/Macintosh|Mac OS X/.test(ua)) {
        const safari = /Safari\//.test(ua) && !/Chrome\/|Chromium\/|Edg\//.test(ua);
        return { guides: safari ? ['macos', 'ios'] : ['macos'], iosNotSafari: false };
    }
    if (/Linux|X11/.test(ua)) return { guides: ['linux'], iosNotSafari: false };
    return { guides: [], iosNotSafari: false };
}
