// Administrator extension. This entry is built separately from the
// application (vite.config.ts); the server lists its URL only to
// administrators (GET /api/v1/ui/modules) and serves its files only to them.
// Evaluating it registers everything administrators have in addition to what
// every account sees: pages, navigation items, parts placed in the slots of
// the shared pages, and their texts.
import { registerExtension } from '../extensions/registry';
import i18n from '../i18n/i18n';
import en from './i18n/en.json';
import ja from './i18n/ja.json';
import { AccountsPage } from './pages/AccountsPage';
import { ConfigPage } from './pages/ConfigPage';
import { RuleEditPage } from './pages/RuleEditPage';
import { RuleNewPage } from './pages/RuleNewPage';
import { AckAction, EmptyHead, SelectAllHead, SelectRow, ToSuppressionAction } from './parts/DetectionParts';
import { AccountHistoryOption, ConfigHistoryOption, NoCentralHostsNotice } from './parts/MiscParts';
import { CreateRuleAction, RuleActions } from './parts/RuleParts';
import { CreateSuppressionAction, DeleteSuppressionAction, SuppressionActionsHead } from './parts/SuppressionParts';
import { BuiltinTemplateNotice, CreateTemplateAction, TemplateActions, TemplateEditor } from './parts/TemplateParts';
import { IssueTokenAction, TokenActionsHead, TokenRowActions } from './parts/TokenParts';

registerExtension({
    texts: { en, ja },
    routes: [
        { path: '/accounts', element: <AccountsPage /> },
        { path: '/config', element: <ConfigPage /> },
        { path: '/rules/new', element: <RuleNewPage /> },
        { path: '/rules/:id/edit', element: <RuleEditPage /> },
    ],
    navItems: [
        { to: '/accounts', icon: 'manage_accounts', label: () => i18n.t('nav.accounts'), order: 90 },
        { to: '/config', icon: 'tune', label: () => i18n.t('nav.config'), order: 100 },
    ],
    slots: {
        'tokens.headerActions': [IssueTokenAction],
        'tokens.headEnd': [TokenActionsHead],
        'tokens.rowEnd': [TokenRowActions],
        'detections.toolbar': [AckAction],
        'detections.headStart': [SelectAllHead],
        'detections.rowStart': [SelectRow],
        'detections.headEnd': [EmptyHead],
        'detections.rowEnd': [ToSuppressionAction],
        'rules.headerActions': [CreateRuleAction],
        'ruleDetail.headerActions': [RuleActions],
        'templates.headerActions': [CreateTemplateAction],
        'templateDetail.headerActions': [TemplateActions],
        'templateDetail.notices': [BuiltinTemplateNotice],
        'templateDetail.panel': [TemplateEditor],
        'suppressions.headerActions': [CreateSuppressionAction],
        'suppressions.headEnd': [SuppressionActionsHead],
        'suppressions.rowEnd': [DeleteSuppressionAction],
        'history.categoryOptionsStart': [AccountHistoryOption],
        'history.categoryOptionsEnd': [ConfigHistoryOption],
        'guide.notices': [NoCentralHostsNotice],
    },
});
