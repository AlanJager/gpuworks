import React, { useState } from "react";
import { createRoot } from "react-dom/client";
import { ConfigProvider, theme } from "antd";
import { StyleProvider } from "@ant-design/cssinjs";
import { MemoryRouter } from "react-router-dom";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import { api, type Deployment } from "../../src/api";
import { deploymentScheduleApi } from "../../src/deploymentScheduleApi";
import { themeConfig } from "../../src/theme/tokens";
import DetailDrawer from "../../src/components/detail/DetailDrawer";
import DeploymentDetailOverview from "../../src/pages/DeploymentDetailOverview";
import { Button } from "../../src/components/ui/button";
import { Tabs, TabsList, TabItem, TabPanel } from "../../src/components/ui/tabs";
import zhCommon from "../../src/i18n/locales/zh/common.json";
import enCommon from "../../src/i18n/locales/en/common.json";
import zhDeployments from "../../src/i18n/locales/zh/deployments.json";
import enDeployments from "../../src/i18n/locales/en/deployments.json";
import zhDeploy from "../../src/i18n/locales/zh/deploy.json";
import enDeploy from "../../src/i18n/locales/en/deploy.json";
import zhNodes from "../../src/i18n/locales/zh/nodes.json";
import enNodes from "../../src/i18n/locales/en/nodes.json";

const params = new URLSearchParams(location.search || location.hash.slice(1));
const dark = params.get("theme") === "dark";
document.documentElement.classList.toggle("dark", dark);
document.documentElement.dataset.theme = dark ? "dark" : "light";
const i18n = createInstance();
void i18n.use(initReactI18next).init({ lng: params.get("lang") || "zh", fallbackLng: "zh",
  resources: { zh: { common: zhCommon, deployments: zhDeployments, deploy: zhDeploy, nodes: zhNodes },
    en: { common: enCommon, deployments: enDeployments, deploy: enDeploy, nodes: enNodes } },
  defaultNS: "common", interpolation: { escapeValue: false } });

const image = "gpuworks/gpuworks-runner:cuda12.9-vllm0.31.0-r2";
const node = "gpu-target";
const cases = ["syncing", "pulling", "unknown", "missing", "zero", "finishing", "present", "failed", "canceled", "unavailable", "waiting", "ambiguous"];
let scene = params.get("scene") || "syncing";
let recovered = false;
let bytes = 3 * 1024 ** 3;
api.imageVersions = async () => ({ items: [{ id: 23, series_id: 4, image_ref: image, size_bytes: 20 * 1024 ** 3 }] }) as never;
api.image = async () => {
  if (scene === "unavailable" && !recovered) { recovered = true; throw new Error("Connection unavailable"); }
  const row = { node_uuid: "target-node", node_name: node, state: scene === "unknown" || scene === "zero" || scene === "finishing" ? "pulling"
    : scene === "canceled" ? "failed" : scene === "unavailable" || scene === "missing" ? "syncing" : scene,
    size_bytes: scene === "missing" ? undefined : scene === "unknown" ? 0 : 8 * 1024 ** 3,
    transferred_bytes: scene === "missing" ? undefined : scene === "zero" ? 0 : scene === "present" || scene === "finishing" ? 8 * 1024 ** 3 : bytes,
    rate_bps: scene === "missing" ? undefined : 12 * 1024 ** 2, updated_at: new Date().toISOString(),
    transfer_error_code: scene === "canceled" ? "PEER_TRANSFER_CANCELED" : undefined,
    message: scene === "failed" ? "Connection to source node interrupted" : undefined };
  return { id: 4, versions: [{ id: 23, nodes: scene === "waiting" ? [] : [row,
    { ...row, node_uuid: "other-node", node_name: scene === "ambiguous" ? node : "another-node", transferred_bytes: 7 * 1024 ** 3 }] }] } as never;
};
api.me = async () => ({ id: 1, username: "preview", role: "viewer" });
deploymentScheduleApi.list = async () => ({ items: [] });
api.deploymentCandidates = async () => ({ items: [], nodes: 0, total: 0, offset: 0, limit: 50, counts: {} }) as never;
const deployment = { id: 625, name: "mineru2-5-2509-1-2b-ms", model_id: 1,
  model_name: "OpenDataLab/MinerU2.5-2509-1.2B", served_name: "MinerU2.5-2509-1.2B", engine: "vllm", image,
  node_uuid: "target-node", node_name: node, runtime_nodes: [], node_requirements: { "gpuworks.ai/vendor": ["nvidia"] },
  desired_state: "active", runtime_state: "pending", state: "pending", replicas: 1, ready_replicas: 0,
  sleepable: false, health: { status: "off", threshold_sec: 60 }, instances: [], model_progress: [{
    node_uuid: "target-node", node_name: node, model_name: "OpenDataLab/MinerU2.5-2509-1.2B", state: "downloading",
    size_bytes: 953.7 * 1024 ** 2, downloaded: 400.5 * 1024 ** 2, rate_bps: 0,
    preparation: { phase: "downloading", total_known: true },
  }],
  placement_status: { unscheduled: 1, reason: { code: "image_delivery_waiting", params: { image, node } } },
} as unknown as Deployment;
const originalModel = structuredClone(deployment.model_progress![0]);
const originalPlacement = structuredClone(deployment.placement_status);
if (params.has("resources")) deployment.placement_status = undefined;

function Preview() {
  const [revision, setRevision] = useState(0);
  const [, refreshResource] = useState(0);
  const [language, setLanguage] = useState(i18n.language);
  React.useEffect(() => {
    Object.assign(window, { advanceResourceStep: (step: string) => {
      const ready = ["model-ready", "model-done", "ready", "starting"].includes(step);
      scene = ["image-done", "ready", "starting"].includes(step) ? "present" : "syncing";
      deployment.model_progress = [{ ...originalModel, state: ready ? "present" : "downloading",
        downloaded: ready ? originalModel.size_bytes : originalModel.downloaded, rate_bps: ready ? 0 : 1024,
        preparation: { phase: ready ? "ready" : "downloading", total_known: true, idle_seconds: 0 } }];
      deployment.placement_status = ["parallel", "model-done"].includes(step) ? originalPlacement : undefined;
      deployment.state = step === "starting" ? "starting" : "pending";
      deployment.instances = step === "starting" ? [{ uuid: "new-instance", state: "starting" }] as Deployment["instances"] : [];
      refreshResource(value => value + 1);
    } });
  }, []);
  const labels = language === "zh"
    ? ["节点间同步", "下载镜像", "总量未知", "同步中未上报进度", "尚未传输", "传输完毕待就绪", "镜像已就绪", "传输失败", "传输取消", "读取失败／重试", "尚未上报", "同名节点"]
    : ["Peer sync", "Download", "Unknown total", "Sync without byte telemetry", "Zero bytes", "Bytes complete", "Image ready", "Transfer failed", "Canceled", "Load failure / retry", "No report", "Duplicate names"];
  return <>
    <div style={{ position: "fixed", zIndex: 2000, bottom: 8, left: 8, width: 160, display: "flex", flexDirection: "column", gap: 8, padding: 8, background: "var(--surface-overlay)", borderRadius: "var(--radius-lg)" }}>
      <label>{language === "zh" ? "模拟状态 " : "Fixture "}<select aria-label="模拟状态" value={scene} onChange={e => { scene = e.target.value; recovered = false; setRevision(x => x + 1); }}>
        {cases.map((value, index) => <option key={value} value={value}>{labels[index]}</option>)}
      </select></label>
      <select aria-label="语言" value={language} onChange={e => { void i18n.changeLanguage(e.target.value); setLanguage(e.target.value); }}><option value="zh">中文</option><option value="en">English</option></select>
    </div>
    <DetailDrawer open onClose={() => {}} title={deployment.name} width="min(832px, calc(100vw - 32px))"
      className="deployment-detail-drawer" extra={<Button size="sm" variant="tertiary" disabled>{language === "zh" ? "预览" : "Preview"}</Button>}>
      <div className="deployment-detail-content" key={revision}>
        <Tabs className="deployment-inspection-tabs deployment-detail-tabs" defaultValue="overview" variant="underline">
          <TabsList className="deployment-top-tabs m-0 max-w-full">
            <TabItem value="overview" label={language === "zh" ? "部署详情" : "Deployment details"} />
            <TabItem value="instances" label={language === "zh" ? "实例详情" : "Instance details"} />
          </TabsList>
          <TabPanel value="overview" className="pt-3"><DeploymentDetailOverview dep={deployment} onChanged={async () => {}} routingPolicies={[]} routingOptions={[]} /></TabPanel>
          <TabPanel value="instances">{language === "zh" ? "暂无实例" : "No instances"}</TabPanel>
        </Tabs>
      </div>
    </DetailDrawer>
  </>;
}
const root = createRoot(document.getElementById("root")!);
root.render(<StyleProvider layer><I18nextProvider i18n={i18n}><ConfigProvider theme={{ ...themeConfig, algorithm: dark ? theme.darkAlgorithm : theme.defaultAlgorithm }}>
  <MemoryRouter><Preview /></MemoryRouter>
</ConfigProvider></I18nextProvider></StyleProvider>);
Object.assign(window, { setTransferredBytes: (next: number) => { bytes = next; } });
