import { Route, Routes } from 'react-router-dom';
import { Layout, RequireAdmin } from './components/Layout';
import { NotFoundPage } from './components/NotFoundPage';
import { AccountsPage } from './pages/AccountsPage';
import { AgentDetailPage } from './pages/AgentDetailPage';
import { AgentsPage } from './pages/AgentsPage';
import { CertificatesPage } from './pages/CertificatesPage';
import { DetectionsPage } from './pages/DetectionsPage';
import { GuidePage } from './pages/GuidePage';
import { HistoryPage } from './pages/HistoryPage';
import { LoginPage } from './pages/LoginPage';
import { RuleDetailPage } from './pages/RuleDetailPage';
import { RuleEditPage } from './pages/RuleEditPage';
import { RuleNewPage } from './pages/RuleNewPage';
import { RulesPage } from './pages/RulesPage';
import { SettingsPage } from './pages/SettingsPage';
import { SuppressionsPage } from './pages/SuppressionsPage';
import { TemplateDetailPage } from './pages/TemplateDetailPage';
import { TemplatesPage } from './pages/TemplatesPage';
import { TokensPage } from './pages/TokensPage';

// Client-side routes. The server falls back to index.html for any path it
// does not know, so every route here also works on a full page reload.
export default function App() {
    return (
        <Routes>
            <Route path='/login' element={<LoginPage />} />
            <Route element={<Layout />}>
                <Route path='/' element={<AgentsPage />} />
                <Route path='/agents' element={<AgentsPage />} />
                <Route path='/agents/:id' element={<AgentDetailPage />} />
                <Route path='/tokens' element={<TokensPage />} />
                <Route path='/detections' element={<DetectionsPage />} />
                <Route path='/rules' element={<RulesPage />} />
                <Route path='/rules/new' element={<RuleNewPage />} />
                <Route path='/rules/:id' element={<RuleDetailPage />} />
                <Route path='/rules/:id/edit' element={<RuleEditPage />} />
                <Route path='/templates' element={<TemplatesPage />} />
                <Route path='/templates/:name' element={<TemplateDetailPage />} />
                <Route path='/suppressions' element={<SuppressionsPage />} />
                <Route path='/certificates' element={<CertificatesPage />} />
                <Route path='/history' element={<HistoryPage />} />
                <Route
                    path='/accounts'
                    element={
                        <RequireAdmin>
                            <AccountsPage />
                        </RequireAdmin>
                    }
                />
                <Route path='/settings' element={<SettingsPage />} />
                <Route path='/guide' element={<GuidePage />} />
                <Route path='*' element={<NotFoundPage />} />
            </Route>
        </Routes>
    );
}
