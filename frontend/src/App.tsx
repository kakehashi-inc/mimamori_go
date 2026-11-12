import { Route, Routes } from 'react-router-dom';
import { Layout } from './components/Layout';
import { NotFoundPage } from './components/NotFoundPage';
import { useExtensions } from './extensions/registry';
import { AgentDetailPage } from './pages/AgentDetailPage';
import { AgentsPage } from './pages/AgentsPage';
import { CertificatesPage } from './pages/CertificatesPage';
import { DetectionsPage } from './pages/DetectionsPage';
import { GuidePage } from './pages/GuidePage';
import { HistoryPage } from './pages/HistoryPage';
import { LoginPage } from './pages/LoginPage';
import { ProfilePage } from './pages/ProfilePage';
import { RuleDetailPage } from './pages/RuleDetailPage';
import { RulesPage } from './pages/RulesPage';
import { SuppressionsPage } from './pages/SuppressionsPage';
import { TemplateDetailPage } from './pages/TemplateDetailPage';
import { TemplatesPage } from './pages/TemplatesPage';
import { TokensPage } from './pages/TokensPage';

// Client-side routes. The server falls back to index.html for any path it
// does not know, so every route here also works on a full page reload.
// Routes of extension modules (registered after login, before any page is
// shown) are added beside them; without the module such a URL is not found.
export default function App() {
    const { routes } = useExtensions();
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
                <Route path='/rules/:id' element={<RuleDetailPage />} />
                <Route path='/templates' element={<TemplatesPage />} />
                <Route path='/templates/:name' element={<TemplateDetailPage />} />
                <Route path='/suppressions' element={<SuppressionsPage />} />
                <Route path='/certificates' element={<CertificatesPage />} />
                <Route path='/history' element={<HistoryPage />} />
                <Route path='/profile' element={<ProfilePage />} />
                <Route path='/guide' element={<GuidePage />} />
                {routes.map(r => (
                    <Route key={r.path} path={r.path} element={r.element} />
                ))}
                <Route path='*' element={<NotFoundPage />} />
            </Route>
        </Routes>
    );
}
