import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from './Button';

/** Copies `text` to the clipboard and briefly confirms it. */
export function CopyButton({ text, size = 'sm' }: { text: string; size?: 'sm' | 'md' }) {
    const { t } = useTranslation();
    const [copied, setCopied] = useState(false);

    async function copy() {
        try {
            await navigator.clipboard.writeText(text);
        } catch {
            // Clipboard API is unavailable on plain HTTP: fall back to a hidden textarea.
            const area = document.createElement('textarea');
            area.value = text;
            area.style.position = 'fixed';
            area.style.opacity = '0';
            document.body.appendChild(area);
            area.select();
            document.execCommand('copy');
            document.body.removeChild(area);
        }
        setCopied(true);
        window.setTimeout(() => setCopied(false), 2000);
    }

    return (
        <Button size={size} icon={copied ? 'check' : 'content_copy'} onClick={() => void copy()}>
            {copied ? t('common.copied') : t('common.copy')}
        </Button>
    );
}

/** Monospace block with a copy button, for commands and secret values. */
export function CodeBlock({ code, copy = true }: { code: string; copy?: boolean }) {
    return (
        <div className='relative rounded-md border border-line bg-canvas'>
            <pre className='overflow-x-auto px-3 py-2 pr-24 font-mono text-xs leading-relaxed whitespace-pre text-ink'>
                {code}
            </pre>
            {copy && (
                <div className='absolute top-1.5 right-1.5'>
                    <CopyButton text={code} />
                </div>
            )}
        </div>
    );
}
