import type { ComponentType } from 'react';
import { useExtensions, type SlotName, type SlotProps } from '../extensions/registry';

/** Renders the parts extensions placed in the named slot (nothing when there are none). */
export function Slot<K extends SlotName>({ name, ...props }: { name: K } & SlotProps[K]) {
    const { slots } = useExtensions();
    const parts = (slots[name] ?? []) as ComponentType<SlotProps[K]>[];
    return (
        <>
            {parts.map((Part, i) => (
                <Part key={i} {...(props as unknown as SlotProps[K])} />
            ))}
        </>
    );
}

/** Whether any part is placed in the named slot. */
export function useSlotFilled(name: SlotName): boolean {
    const { slots } = useExtensions();
    return (slots[name]?.length ?? 0) > 0;
}
