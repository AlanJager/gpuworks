import { Layout, Menu, Space, Typography, Button, Tooltip, Drawer, Input, Modal, Empty } from "antd";
import UpdateNotice from "./components/UpdateNotice";
import NoticeBell from "./components/NoticeBell";
import AssistantPanel from "./assistant/AssistantPanel";
import { AssistantProvider, useAssistant } from "./assistant/AssistantContext";
import { Link, Navigate, Route, Routes, useLocation, useNavigate } from "react-router-dom";
import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import {
  CloudServerOutlined, DashboardOutlined, AppstoreOutlined, RocketOutlined,
  UnorderedListOutlined, KeyOutlined, CloudOutlined, SettingOutlined, FileSearchOutlined,
  LogoutOutlined, SearchOutlined, ApartmentOutlined,
  MenuOutlined, ControlOutlined, ApiOutlined,
  ExperimentOutlined, HeartOutlined, RobotOutlined,
} from "@ant-design/icons";
import NodesPage from "./pages/NodesPage";
import DashboardPage from "./pages/DashboardPage";
import MonitorPage from "./pages/monitor/MonitorPage";
import ServiceMonitorPage from "./pages/monitor/ServiceMonitorPage";
import ModelsPage from "./pages/ModelsPage";
import InferenceTemplatesPage from "./pages/InferenceTemplatesPage";
import InferenceTemplateDetailPage from "./pages/InferenceTemplateDetailPage";
import DeployPage from "./pages/DeployPage";
import DeploymentsPage from "./pages/DeploymentsPage";
import EvaluationsPage from "./pages/EvaluationsPage";
import EvaluationDetailPage from "./pages/evaluations/EvaluationDetailPage";
import EvaluationComparePage from "./pages/evaluations/EvaluationComparePage";
import APIKeysPage from "./pages/APIKeysPage";
import CloudProvidersPage from "./pages/CloudProvidersPage";
import SettingsPage from "./pages/SettingsPage";
import LogsPage from "./pages/LogsPage";
import SessionBoundary from "./components/SessionBoundary";
import NotFoundPage from "./pages/NotFoundPage";
import ImagesPage from "./pages/ImagesPage";
import StoragePage from "./pages/StoragePage";
import StorageDetailPage from "./pages/storage/StorageDetailPage";
import NetworksPage from "./pages/networks/NetworksPage";
import NetworkDetailPage from "./pages/networks/NetworkDetailPage";
import GPUPage from "./pages/GPUPage";
import TerminalPage from "./pages/TerminalPage";
import AppErrorBoundary from "./components/AppErrorBoundary";
import PlaygroundPage from "./pages/playground/PlaygroundPage";
import { signOutPlayground } from "./pages/playground/conversations/useConversations";
import LanguageSwitcher from "./components/LanguageSwitcher";
import { BrandMark, useBranding, useDocumentBranding } from "./branding";
import "./pages/evaluations/agenticWorkload.css";
import "./pages/inferenceTemplateFlat.css";
import "./pages/networks/topology/networkTopology.css";
import "./pages/inferenceTemplateOrganizationCatalog.css";
import "./pages/nodes/nodeDetail.css";
import "./pages/playground/playground.css";

const { Header, Sider, Content } = Layout;

const navGroups = [
  { labelKey: "navigation.groups.overview", items: [
    { key: "/dashboard", icon: <DashboardOutlined />, labelKey: "navigation.dashboard" },
    { key: "/monitor", icon: <HeartOutlined />, labelKey: "navigation.monitor" },
  ] },
  { labelKey: "navigation.groups.resources", items: [
    { key: "/providers", icon: <CloudOutlined />, labelKey: "navigation.publicCloud" },
    { key: "/zstack", icon: <ApartmentOutlined />, labelKey: "navigation.privateCloud" },
    { key: "/nodes", icon: <CloudServerOutlined />, labelKey: "navigation.nodes" },
    { key: "/gpus", icon: <ControlOutlined />, labelKey: "navigation.gpus" },
    { key: "/models", icon: <AppstoreOutlined />, labelKey: "navigation.models" },
	{ key: "/inference-templates", icon: <AppstoreOutlined />, labelKey: "navigation.inferenceTemplates" },
    { key: "/images", icon: <CloudOutlined />, labelKey: "navigation.images" },
    { key: "/storage", icon: <CloudServerOutlined />, labelKey: "navigation.storage" },
    { key: "/networks", icon: <ApiOutlined />, labelKey: "navigation.networks" },
  ] },
  { labelKey: "navigation.groups.inference", items: [
    { key: "/deploy", icon: <RocketOutlined />, labelKey: "navigation.deploy" },
    { key: "/deployments", icon: <UnorderedListOutlined />, labelKey: "navigation.deployments" },
    { key: "/playground/chat", icon: <ExperimentOutlined />, labelKey: "navigation.playground" },
    { key: "/evaluations", icon: <FileSearchOutlined />, labelKey: "navigation.evaluations" },
  ] },
  { labelKey: "navigation.groups.platform", items: [
    { key: "/apikeys", icon: <KeyOutlined />, labelKey: "navigation.apiKeys" },
    { key: "/logs", icon: <FileSearchOutlined />, labelKey: "navigation.logs" },
    { key: "/settings", icon: <SettingOutlined />, labelKey: "navigation.settings" },
  ] },
];

export default function App() {
  return (
    <SessionBoundary onLogout={signOutPlayground}>
      {(logout) => <AppErrorBoundary>
        <Routes>
          <Route path="/login" element={<Navigate to="/dashboard" replace />} />
          {/* The terminal uses the full viewport and shares the session gate. */}
          <Route path="/nodes/:uuid/terminal" element={<TerminalPage />} />
          <Route path="*" element={<AuthenticatedShell onLogout={logout} />} />
        </Routes>
      </AppErrorBoundary>}
    </SessionBoundary>
  );
}

function AuthenticatedShell({ onLogout }: { onLogout: () => Promise<void> }) {
  const { t } = useTranslation();
  const branding = useBranding();
  useDocumentBranding(branding);
  const loc = useLocation();
  const navigate = useNavigate();
  const [mobileNavOpen, setMobileNavOpen] = useState(false);
  const [searchOpen, setSearchOpen] = useState(false);
  const [search, setSearch] = useState("");
  const selected = "/" + (loc.pathname.split("/")[1] || "dashboard");

  const menuItems = navGroups.map((group) => ({
    type: "group" as const,
    label: t(group.labelKey),
    children: group.items.map((item) => ({ ...item, label: <Link to={item.key}>{t(item.labelKey)}</Link> })),
  }));
  const searchableItems = navGroups
    .flatMap((group) => group.items.map((item) => ({ ...item, label: t(item.labelKey), group: t(group.labelKey) })))
    .filter((item) => !search.trim() || `${item.label} ${item.group}`.toLowerCase().includes(search.trim().toLowerCase()));

  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      const target = event.target as HTMLElement | null;
      const editable = target?.matches("input, textarea, select, [contenteditable='true']");
      if ((event.key === "/" && !editable) || ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "k")) {
        event.preventDefault();
        setSearchOpen(true);
      }
    };
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, []);

  const openDestination = (path: string) => {
    navigate(path);
    setSearchOpen(false);
    setMobileNavOpen(false);
    setSearch("");
  };

  return (
    <AssistantProvider>
    <Layout className="app-shell">
      <Sider theme="light" width={208} className="app-sider desktop-sider">
        <Brand />
        <Menu mode="inline" selectedKeys={[selected]} items={menuItems} className="app-nav" />
      </Sider>
      <Layout>
        <Header className="app-header">
          <Space size={8}>
            <Tooltip title={t("header.openNavigation")}>
              <Button className="mobile-menu-button" type="text" icon={<MenuOutlined />} onClick={() => setMobileNavOpen(true)} aria-label={t("header.openNavigation")} />
            </Tooltip>
            <Typography.Text strong className="app-header-title">{branding.consoleTitle}</Typography.Text>
            <Tooltip title={t("header.search")}>
              <Button type="text" icon={<SearchOutlined />} onClick={() => setSearchOpen(true)} aria-label={t("header.search")} />
            </Tooltip>
          </Space>
          <Space>
            <UpdateNotice />
            <NoticeBell />
            <AssistantHeaderButton />
            <LanguageSwitcher light />
            <Typography.Text className="app-header-user" style={{ color: "rgba(255,255,255,.82)" }}>{t("header.administrator")}</Typography.Text>
            <Tooltip title={t("header.logout")}><Button type="text" icon={<LogoutOutlined />} onClick={onLogout} aria-label={t("header.logout")} /></Tooltip>
          </Space>
        </Header>
        <Content className="app-content">
          <Routes>
            <Route path="/" element={<Navigate to="/dashboard" replace />} />
            <Route path="/login" element={<Navigate to="/dashboard" replace />} />
            <Route path="/dashboard" element={<DashboardPage />} />
            <Route path="/monitor" element={<MonitorPage />} />
            <Route path="/monitor/d/:id" element={<ServiceMonitorPage />} />
            <Route path="/nodes" element={<NodesPage />} />
            <Route path="/gpus" element={<GPUPage />} />
            <Route path="/providers" element={<CloudProvidersPage scope="public" />} />
            <Route path="/zstack" element={<CloudProvidersPage scope="private" />} />
            <Route path="/models" element={<ModelsPage />} />
			<Route path="/inference-templates" element={<InferenceTemplatesPage />} />
			<Route path="/inference-templates/*" element={<InferenceTemplateDetailPage />} />
            <Route path="/images" element={<ImagesPage />} />
            <Route path="/storage" element={<StoragePage />} />
            <Route path="/storage/:id" element={<StorageDetailPage />} />
            <Route path="/networks" element={<NetworksPage />} />
            <Route path="/networks/:id" element={<NetworkDetailPage />} />
            <Route path="/ai-store/ai-model" element={<ModelsPage />} />
            <Route path="/deploy" element={<DeployPage />} />
            <Route path="/deployments" element={<DeploymentsPage />} />
            <Route path="/playground/*" element={<PlaygroundPage />} />
            <Route path="/evaluations" element={<EvaluationsPage />} />
			<Route path="/evaluations/compare" element={<EvaluationComparePage />} />
			<Route path="/evaluations/:id" element={<EvaluationDetailPage />} />
            <Route path="/apikeys" element={<APIKeysPage />} />
            <Route path="/logs" element={<LogsPage />} />
            <Route path="/settings" element={<SettingsPage />} />
            <Route path="*" element={<NotFoundPage />} />
          </Routes>
        </Content>
      </Layout>
      <Drawer
        title={null}
        placement="left"
        width={260}
        open={mobileNavOpen}
        onClose={() => setMobileNavOpen(false)}
        styles={{ body: { padding: 0 } }}
      >
        <Brand />
        <Menu
          mode="inline"
          selectedKeys={[selected]}
          items={menuItems}
          className="app-nav"
          onClick={() => setMobileNavOpen(false)}
        />
      </Drawer>
      <Modal
        className="global-search"
        title={t("search.title")}
        open={searchOpen}
        onCancel={() => { setSearchOpen(false); setSearch(""); }}
        footer={null}
        width={560}
        afterOpenChange={(open) => { if (!open) setSearch(""); }}
      >
        <Input
          autoFocus
          allowClear
          prefix={<SearchOutlined />}
          className="global-search-input"
          placeholder={t("search.placeholder")}
          value={search}
          onChange={(event) => setSearch(event.target.value)}
          onPressEnter={() => searchableItems[0] && openDestination(searchableItems[0].key)}
        />
        <div className="global-search-results">
          {searchableItems.map((item) => (
            <button key={item.key} type="button" className="global-search-item" onClick={() => openDestination(item.key)}>
              <span className="global-search-item-icon">{item.icon}</span>
              <span>{item.label}</span>
              <span className="global-search-item-group">{item.group}</span>
            </button>
          ))}
          {searchableItems.length === 0 && <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description={t("search.empty")} />}
        </div>
      </Modal>
      <AssistantPanel />
    </Layout>
    </AssistantProvider>
  );
}

// AssistantHeaderButton opens the resident assistant from any page (also ⌘I / Ctrl+I).
function AssistantHeaderButton() {
  const { t } = useTranslation("ai");
  const { open, setOpen } = useAssistant();
  return (
    <Tooltip title={t("panel.headerTooltip")}>
      <Button type="text" icon={<RobotOutlined />} onClick={() => setOpen(!open)} aria-label={t("title")} style={{ color: "rgba(255,255,255,.88)" }}>
        <span className="app-header-user">{t("title")}</span>
      </Button>
    </Tooltip>
  );
}

function Brand() {
  const { productName, logo } = useBranding();
  return (
    <div className="app-brand">
      <BrandMark logo={logo} />
      <span className="app-brand-name">{productName}</span>
    </div>
  );
}
