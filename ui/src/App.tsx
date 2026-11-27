import { FeedbackHost } from "./ui-kit/feedback";
import { Button } from "@/components/ui/button";
import { Tooltip } from "@/components/ui/tooltip";
import { SidebarProvider } from "@/components/ui/sidebar";
import AppSidebar from "./app/AppSidebar";
import ConsoleBanners from "./app/ConsoleBanners";
import ConsoleManifestWait from "./app/ConsoleManifestWait";
import { lazyPage } from "./app/lazyPage";
import { ConsoleIcon } from "./ui-kit/consoleIcons";
import { Spinner } from "./ui-kit/Spinner";

const AssistantIcon = ConsoleIcon.assistant;
import UpdateNotice from "./components/UpdateNotice";
import NoticeBell from "./components/NoticeBell";
import AssistantPanel from "./assistant/AssistantPanel";
import { AssistantProvider, useAssistant } from "./assistant/AssistantContext";
import { Navigate, Route, Routes, useLocation } from "react-router-dom";
import { Suspense, useState } from "react";
import { useTranslation } from "react-i18next";
import SessionBoundary from "./components/SessionBoundary";
import ClusterUpgradeBoundary from "./components/ClusterUpgradeScreen";
import UserMenu from "./components/UserMenu";
import { StorageDetailRedirect } from "./pages/storage/storageLinks";
import { NetworkDetailRedirect } from "./pages/networks/networkLinks";
import AppErrorBoundary from "./components/AppErrorBoundary";
import { signOutPlayground } from "./pages/playground/conversations/useConversations";
import LanguageSwitcher from "./components/LanguageSwitcher";
import { BrandMark, useBranding, useDocumentBranding } from "./branding";
import { extensionRoutes } from "./extensions";
import { ExtensionHeaderItems, ExtensionRoutePage } from "./extensions/shell";
import "./pages/evaluations/agenticWorkload.css";
import "./pages/inferenceTemplateFlat.css";
import "./pages/networks/topology/networkTopology.css";
import "./pages/inferenceTemplateOrganizationCatalog.css";
import "./components/detail/detailDrawer.css";
import "./pages/nodes/nodeDetail.css";
import "./pages/dashboard.css";
// Shared by the deploy page and the PD scale dialog of a deployment detail,
// which can open before the lazily loaded deploy page.
import "./pages/deploy/deployNodeCard.css";
import "./pages/scheduling/scheduling.css";
import "./pages/batches/batches.css";
import "./pages/scheduling/heatmap.css";
import "./pages/playground/playground.css";

// Each page is its own chunk: the shell paints and the first page asks for
// its data without downloading and parsing every other page with it.
const NodesPage = lazyPage(() => import("./pages/NodesPage"));
const DashboardPage = lazyPage(() => import("./pages/DashboardPage"));
const MonitorPage = lazyPage(() => import("./pages/monitor/MonitorPage"));
const ServiceMonitorPage = lazyPage(() => import("./pages/monitor/ServiceMonitorPage"));
const AnalyticsPage = lazyPage(() => import("./pages/analytics/AnalyticsPage"));
const ModelsPage = lazyPage(() => import("./pages/ModelsPage"));
const InferenceTemplatesPage = lazyPage(() => import("./pages/InferenceTemplatesPage"));
const InferenceTemplateDetailPage = lazyPage(() => import("./pages/InferenceTemplateDetailPage"));
const DeployPage = lazyPage(() => import("./pages/DeployPage"));
const DeploymentsPage = lazyPage(() => import("./pages/DeploymentsPage"));
const SchedulingPage = lazyPage(() => import("./pages/SchedulingPage"));
const BatchesPage = lazyPage(() => import("./pages/BatchesPage"));
const EvaluationsPage = lazyPage(() => import("./pages/EvaluationsPage"));
const EvaluationDetailPage = lazyPage(() => import("./pages/evaluations/EvaluationDetailPage"));
const EvaluationComparePage = lazyPage(() => import("./pages/evaluations/EvaluationComparePage"));
const APIKeysPage = lazyPage(() => import("./pages/APIKeysPage"));
const CloudProvidersPage = lazyPage(() => import("./pages/CloudProvidersPage"));
const SettingsPage = lazyPage(() => import("./pages/SettingsPage"));
const LogsPage = lazyPage(() => import("./pages/LogsPage"));
const NotFoundPage = lazyPage(() => import("./pages/NotFoundPage"));
const ImagesPage = lazyPage(() => import("./pages/ImagesPage"));
const StoragePage = lazyPage(() => import("./pages/StoragePage"));
const NetworksPage = lazyPage(() => import("./pages/networks/NetworksPage"));
const NetworkTopologyGuide = lazyPage(() => import("./pages/networks/topology/NetworkTopologyGuide"));
const GPUPage = lazyPage(() => import("./pages/GPUPage"));
const TerminalPage = lazyPage(() => import("./pages/TerminalPage"));
const PlaygroundPage = lazyPage(() => import("./pages/playground/PlaygroundPage"));


export default function App() {
  return (
    <ClusterUpgradeBoundary>
      <FeedbackHost />
      <SessionBoundary onLogout={signOutPlayground}>
        {(logout) => <AppErrorBoundary>
          <Routes>
            <Route path="/login" element={<Navigate to="/dashboard" replace />} />
            {/* The terminal uses the full viewport and shares the session gate. */}
            <Route path="/nodes/:uuid/terminal" element={<Suspense fallback={<PageLoading />}><TerminalPage /></Suspense>} />
            <Route path="*" element={<AuthenticatedShell onLogout={logout} />} />
          </Routes>
        </AppErrorBoundary>}
      </SessionBoundary>
    </ClusterUpgradeBoundary>
  );
}

/** Sends the old /ai-store/ai-model address to /models, keeping its query (?detail=). */
function ModelsAlias() {
  const { search } = useLocation();
  return <Navigate replace to={`/models${search}`} />;
}

function AuthenticatedShell({ onLogout }: { onLogout: () => Promise<void> }) {
  const branding = useBranding();
  useDocumentBranding(branding);
  const [navCollapsed, setNavCollapsed] = useState(readNavCollapsed);
  const setNavOpen = (open: boolean) => {
    setNavCollapsed(!open);
    writeNavCollapsed(!open);
  };

  return (
    <AssistantProvider>
    <div className="app-shell">
      <header className="app-header">
        <div className="app-header-start">
          <Brand title={branding.consoleTitle} />
        </div>
        <div className="app-header-actions">
          <UpdateNotice />
          <NoticeBell />
          <ExtensionHeaderItems />
          <AssistantHeaderButton />
          <LanguageSwitcher light />
          <UserMenu onLogout={onLogout} />
        </div>
      </header>
      <SidebarProvider open={!navCollapsed} onOpenChange={setNavOpen} breakpointBehavior="collapse">
      <div className="app-body">
        <AppSidebar />
        <main className="app-content"><div className="app-page">
          <ConsoleBanners />
          <ConsoleManifestWait>
          <Suspense fallback={<PageLoading />}>
          <Routes>
            <Route path="/" element={<Navigate to="/dashboard" replace />} />
            <Route path="/login" element={<Navigate to="/dashboard" replace />} />
            <Route path="/dashboard" element={<DashboardPage />} />
            <Route path="/monitor" element={<MonitorPage />} />
            <Route path="/monitor/d/:id" element={<ServiceMonitorPage />} />
            <Route path="/analytics/:tab?" element={<AnalyticsPage />} />
            <Route path="/nodes" element={<NodesPage />} />
            <Route path="/gpus" element={<GPUPage />} />
            <Route path="/providers" element={<CloudProvidersPage />} />
            {/* The private cloud page became the external platforms list, opened on private clouds. */}
            <Route path="/zstack" element={<Navigate to="/providers?category=private_cloud" replace />} />
            {/* The Kubernetes page became part of the external platforms list, opened on what connects Kubernetes. */}
            <Route path="/kubernetes" element={<Navigate to="/providers?resource=kubernetes" replace />} />
            <Route path="/models" element={<ModelsPage />} />
			<Route path="/inference-templates" element={<InferenceTemplatesPage />} />
			<Route path="/inference-templates/*" element={<InferenceTemplateDetailPage />} />
            <Route path="/images" element={<ImagesPage />} />
            <Route path="/storage" element={<StoragePage />} />
            <Route path="/storage/:id" element={<StorageDetailRedirect />} />
            <Route path="/networks" element={<NetworksPage />} />
            <Route path="/networks/guide" element={<NetworkTopologyGuide />} />
            <Route path="/networks/:id" element={<NetworkDetailRedirect />} />
            {/* The old models address: same page, so the menu and its breadcrumb follow /models. */}
            <Route path="/ai-store/ai-model" element={<ModelsAlias />} />
            <Route path="/deploy" element={<DeployPage />} />
            <Route path="/deployments" element={<DeploymentsPage />} />
            <Route path="/scheduling" element={<SchedulingPage />} />
            <Route path="/batches" element={<BatchesPage />} />
            <Route path="/playground/*" element={<PlaygroundPage />} />
            <Route path="/evaluations" element={<EvaluationsPage />} />
			<Route path="/evaluations/compare" element={<EvaluationComparePage />} />
			<Route path="/evaluations/:id" element={<EvaluationDetailPage />} />
            <Route path="/apikeys" element={<APIKeysPage />} />
            <Route path="/logs" element={<LogsPage />} />
            <Route path="/settings" element={<SettingsPage />} />
            {/* Console extensions' pages (ui/src/extensions); none in the community build. */}
            {extensionRoutes().map((route) => (
              <Route key={route.path} path={route.path} element={<ExtensionRoutePage route={route} notFound={<NotFoundPage />} />} />
            ))}
            <Route path="*" element={<NotFoundPage />} />
          </Routes>
          </Suspense>
          </ConsoleManifestWait>
        </div></main>
      </div>
      </SidebarProvider>
      <AssistantPanel />
    </div>
    </AssistantProvider>
  );
}

// PageLoading stands in the content area while a page's chunk arrives, as an
// extension's page does while its module loads.
function PageLoading() {
  const { t } = useTranslation("common");
  return <Spinner label={t("consoleExtension.loading")} />;
}

// AssistantHeaderButton opens the resident assistant from any page (also ⌘I / Ctrl+I).
function AssistantHeaderButton() {
  const { t } = useTranslation("ai");
  const { open, setOpen } = useAssistant();
  return (
    <Tooltip content={t("panel.headerTooltip")}>
      {/* The one header button with a label and a hairline border (ui-guidelines 9.1b). */}
      <Button variant="secondary" size="md" leadingIcon={AssistantIcon} onClick={() => setOpen(!open)} aria-label={t("title")} aria-pressed={open}>
        <span className="app-header-user">{t("title")}</span>
      </Button>
    </Tooltip>
  );
}

// Brand is the logo, followed by the top bar title when one is set.
function Brand({ title = "" }: { title?: string }) {
  const { productName, logo } = useBranding();
  return (
    <div className="app-brand">
      <BrandMark logo={logo} label={productName} />
      {title && <span className="app-brand-sep" aria-hidden="true" />}
      {title && <span className="app-brand-name">{title}</span>}
    </div>
  );
}

// The collapsed menu is a per-browser preference, like the language.
const navCollapsedKey = "gpuworks.navCollapsed";

function readNavCollapsed() {
  try {
    return localStorage.getItem(navCollapsedKey) === "1";
  } catch {
    return false;
  }
}

function writeNavCollapsed(collapsed: boolean) {
  try {
    localStorage.setItem(navCollapsedKey, collapsed ? "1" : "0");
  } catch {
    // Storage blocked: the choice lasts until reload.
  }
}
