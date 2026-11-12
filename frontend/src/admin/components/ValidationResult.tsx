import { useTranslation } from 'react-i18next';
import { Alert } from '../../components/StateViews';

/** Result of POST /rules/validate: null when not checked yet. */
export function ValidationResult({ errors }: { errors: string[] | null }) {
    const { t } = useTranslation();
    if (errors === null) return null;
    if (errors.length === 0) return <Alert tone='success'>{t('ruleEditor.valid')}</Alert>;
    return (
        <Alert tone='danger' title={t('ruleEditor.invalid')}>
            <ul className='list-disc pl-5'>
                {errors.map((e, i) => (
                    <li key={i}>{e}</li>
                ))}
            </ul>
        </Alert>
    );
}
