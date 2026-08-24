import { BrowserRouter, Navigate, Route, Routes } from "react-router-dom";
import { SessionProvider } from "@/lib/session";
import { AppShell } from "@/components/layout/AppShell";
import { LoginPage } from "@/pages/Login/LoginPage";
import { CasesPage } from "@/pages/Cases/CasesPage";
import { CaseDetailPage } from "@/pages/Cases/CaseDetailPage";
import { AgentsPage } from "@/pages/Agents/AgentsPage";
import { AgentDetailPage } from "@/pages/Agents/AgentDetailPage";
import { RecipesPage } from "@/pages/Recipes/RecipesPage";
import { RecipeDetailPage } from "@/pages/Recipes/RecipeDetailPage";
import { RecipeBuilderPage } from "@/pages/Recipes/RecipeBuilderPage";
import { ApprovalsPage } from "@/pages/Approvals/ApprovalsPage";
import { AuditPage } from "@/pages/Audit/AuditPage";
import { ConnectorsPage } from "@/pages/Connectors/ConnectorsPage";
import { ConnectorSetupPage } from "@/pages/Connectors/ConnectorSetupPage";
import { SettingsPage } from "@/pages/Settings/SettingsPage";

export function App() {
  return (
    <BrowserRouter>
      <SessionProvider>
        <Routes>
          <Route path="/login" element={<LoginPage />} />
          <Route element={<AppShell />}>
            <Route index element={<Navigate to="/cases" replace />} />
            <Route path="/cases" element={<CasesPage />} />
            <Route path="/cases/:id" element={<CaseDetailPage />} />
            <Route path="/agents" element={<AgentsPage />} />
            <Route path="/agents/:id" element={<AgentDetailPage />} />
            <Route path="/recipes" element={<RecipesPage />} />
            <Route path="/recipes/new" element={<RecipeBuilderPage />} />
            <Route path="/recipes/:name" element={<RecipeDetailPage />} />
            <Route path="/approvals" element={<ApprovalsPage />} />
            <Route path="/audit" element={<AuditPage />} />
            <Route path="/connectors" element={<ConnectorsPage />} />
            <Route path="/connectors/:id" element={<ConnectorSetupPage />} />
            <Route path="/settings" element={<SettingsPage />} />
            <Route path="*" element={<Navigate to="/cases" replace />} />
          </Route>
        </Routes>
      </SessionProvider>
    </BrowserRouter>
  );
}
