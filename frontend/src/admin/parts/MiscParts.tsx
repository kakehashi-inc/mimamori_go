import { useTranslation } from 'react-i18next';
import { Alert } from '../../components/StateViews';
import type { Guide } from '../../types';

// Parts placed in the history filter and the setup guide (slots history.*
// and guide.notices).

/** Option of the history category filter for the account history. */
export function AccountHistoryOption() {
    const { t } = useTranslation();
    return <option value='account'>{t('historyCategory.account')}</option>;
}

/** Option of the history category filter for the system settings history. */
export function ConfigHistoryOption() {
    const { t } = useTranslation();
    return <option value='config'>{t('historyCategory.config')}</option>;
}

/** Tells administrators to add a name to the server certificate when it has none. */
export function NoCentralHostsNotice({ guide }: { guide: Guide }) {
    const { t } = useTranslation();
    if (guide.central_hosts?.length) return null;
    return <Alert tone='warning'>{t('guide.noCentralHosts')}</Alert>;
}
