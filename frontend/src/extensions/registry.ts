import { useSyncExternalStore, type ComponentType, type ReactNode } from 'react';
import i18n from '../i18n/i18n';
import type { Detection, Guide, RuleDetail, Suppression, TemplateDetail, TemplateSummary, Token } from '../types';

// Extension points of the application. An extension module (loaded after
// login, see loader.ts) registers routes, navigation items, parts placed in
// named slots of the pages, and texts. The application itself knows only the
// slot names and the props each slot passes; what is placed there is decided
// by the extension.

/** Props of a slot that passes nothing. */
export type NoProps = Record<never, never>;

/** Props passed to the parts of each slot. */
export interface SlotProps {
    'tokens.headerActions': { reload: () => void };
    'tokens.headEnd': NoProps;
    'tokens.rowEnd': { token: Token; reload: () => void };
    'detections.toolbar': {
        items: Detection[];
        selected: Set<string>;
        setSelected: (s: Set<string>) => void;
        reload: () => void;
    };
    'detections.headStart': {
        items: Detection[];
        selected: Set<string>;
        setSelected: (s: Set<string>) => void;
    };
    'detections.rowStart': {
        detection: Detection;
        selected: Set<string>;
        setSelected: (s: Set<string>) => void;
    };
    'detections.headEnd': NoProps;
    'detections.rowEnd': { detection: Detection; reload: () => void };
    'rules.headerActions': NoProps;
    'ruleDetail.headerActions': { rule: RuleDetail; reload: () => void };
    'templates.headerActions': { templates: TemplateSummary[] };
    'templateDetail.headerActions': { template: TemplateDetail; editing: boolean; setEditing: (v: boolean) => void };
    'templateDetail.notices': { template: TemplateDetail };
    'templateDetail.editor': { template: TemplateDetail; onSaved: (t: TemplateDetail) => void; onCancel: () => void };
    'suppressions.headerActions': { reload: () => void };
    'suppressions.headEnd': NoProps;
    'suppressions.rowEnd': { suppression: Suppression; reload: () => void };
    'history.categoryOptionsStart': NoProps;
    'history.categoryOptionsEnd': NoProps;
    'guide.notices': { guide: Guide };
}

export type SlotName = keyof SlotProps;

export interface ExtensionRoute {
    path: string;
    element: ReactNode;
}

export interface ExtensionNavItem {
    to: string;
    icon: string;
    /** Label in the current language (called on every render). */
    label: () => string;
    /** Position among the navigation items (the application's own items use 10, 20, ...). */
    order: number;
}

export type ExtensionSlots = { [K in SlotName]?: ComponentType<SlotProps[K]>[] };

export interface Extension {
    routes?: ExtensionRoute[];
    navItems?: ExtensionNavItem[];
    slots?: ExtensionSlots;
    /** Texts per language, merged into the 'translation' namespace. */
    texts?: Record<string, object>;
}

interface Snapshot {
    routes: ExtensionRoute[];
    navItems: ExtensionNavItem[];
    slots: ExtensionSlots;
}

const EMPTY: Snapshot = { routes: [], navItems: [], slots: {} };
let snapshot: Snapshot = EMPTY;
const listeners = new Set<() => void>();

function emit() {
    for (const l of listeners) l();
}

/** Adds an extension. Its texts are added before anything that uses them is shown. */
export function registerExtension(ext: Extension): void {
    for (const [lng, texts] of Object.entries(ext.texts ?? {})) {
        i18n.addResourceBundle(lng, 'translation', texts, true, false);
    }
    const slots: ExtensionSlots = { ...snapshot.slots };
    for (const [name, parts] of Object.entries(ext.slots ?? {}) as [SlotName, ComponentType<never>[]][]) {
        slots[name] = [...((slots[name] as ComponentType<never>[] | undefined) ?? []), ...parts] as never;
    }
    snapshot = {
        routes: [...snapshot.routes, ...(ext.routes ?? [])],
        navItems: [...snapshot.navItems, ...(ext.navItems ?? [])],
        slots,
    };
    emit();
}

/** Whether any extension is registered. */
export function hasExtensions(): boolean {
    return snapshot !== EMPTY;
}

function subscribe(l: () => void): () => void {
    listeners.add(l);
    return () => listeners.delete(l);
}

function getSnapshot(): Snapshot {
    return snapshot;
}

/** The registered routes, navigation items and slot parts; re-renders on change. */
export function useExtensions(): Snapshot {
    return useSyncExternalStore(subscribe, getSnapshot);
}
