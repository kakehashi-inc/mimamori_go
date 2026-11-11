import type { ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from './Button';
import { CodeBlock } from './CopyButton';
import { Modal } from './Modal';
import { Alert } from './StateViews';

/** Shows a newly issued secret once, with a copy button and extra guidance. */
export function SecretValueModal({
    open,
    title,
    value,
    onClose,
    children,
}: {
    open: boolean;
    title: ReactNode;
    value: string;
    onClose: () => void;
    children?: ReactNode;
}) {
    const { t } = useTranslation();
    return (
        <Modal
            open={open}
            title={title}
            onClose={onClose}
            size='lg'
            footer={
                <Button variant='primary' onClick={onClose}>
                    {t('common.close')}
                </Button>
            }
        >
            <div className='flex flex-col gap-4'>
                <Alert tone='warning'>{t('secret.onceNote')}</Alert>
                <CodeBlock code={value} />
                {children}
            </div>
        </Modal>
    );
}
