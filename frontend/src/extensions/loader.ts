import { listUiModules } from '../api/client';
import type { Role } from '../types';
import { hasExtensions } from './registry';

// Module the development server loads instead of asking the server: in
// development (yarn dev) no built files exist. The path is a variable so that
// neither TypeScript nor the bundler follows it, and the branch is removed from
// production builds.
const DEV_MODULE = '/src/admin/entry.tsx';

let loaded: Promise<void> | null = null;

/** Accepts only absolute paths on this server. */
function isSameOriginPath(url: unknown): url is string {
    return typeof url === 'string' && url.startsWith('/') && !url.startsWith('//');
}

async function load(role: Role): Promise<void> {
    let urls: string[];
    if (import.meta.env.DEV) {
        urls = role === 'admin' ? [DEV_MODULE] : [];
    } else {
        const list = await listUiModules();
        urls = (list ?? []).filter(isSameOriginPath);
    }
    // Each module registers itself (registry.ts) when it is evaluated.
    for (const url of urls) {
        await import(/* @vite-ignore */ url);
    }
}

/**
 * Loads the extension modules of the signed-in account once per page load.
 * A failure is logged and the application runs without them.
 */
export function loadExtensions(role: Role): Promise<void> {
    if (!loaded) {
        loaded = load(role).catch(e => {
            console.error('extension modules could not be loaded', e);
        });
    }
    return loaded;
}

/**
 * Called when the session ends. Loaded modules cannot be unloaded from a page,
 * so when any was loaded the page is replaced by a fresh login page; returns
 * true in that case.
 */
export function leaveExtensions(): boolean {
    if (!loaded && !hasExtensions()) return false;
    if (!hasExtensions()) {
        loaded = null;
        return false;
    }
    window.location.replace('/login');
    return true;
}
