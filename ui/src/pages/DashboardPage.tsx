import { useLayoutEffect, useRef, useState, type ReactNode } from "react";
import { Alert, Button, Card, Dropdown, Pagination, Select, Tag, Tooltip, Typography } from "antd";
import { ArrowRightOutlined, CloudOutlined, CloudServerOutlined, ControlOutlined, DatabaseOutlined, InfoCircleOutlined, RocketOutlined } from "@ant-design/icons";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { api, useEvents, usePolling, type CapacityGroup, type CloudProvider, type DashboardSummary, type GPUListResponse, type Health, type List, type ModelListItem, type ModelPage, type MonitorOverview, type NodePage, type Storage, type StorageNodeUsage } from "../api";
import { analyticsApi, type UsageAnalytics } from "../analyticsApi";
import { fmtCelsius, fmtCompact, fmtInt, fmtPct, fmtWatts } from "./analytics/format";
import { storageTypeInfo } from "../storageTypes";
import PageHeader from "../components/PageHeader";
import { storageDetailPath } from "./storage/storageLinks";

const GIB = 1024 ** 3;
const DAY = 24 * 60 * 60 * 1000;
const ATTENTION_DETAIL_LIMIT = 5;
const DISTRIBUTION_PAGE_SIZE = 8;
const STORAGE_PAGE_SIZE = 2;
const fmt = (value?: number) => value == null ? "—" : Intl.NumberFormat().format(value);
const gib = (value?: number) => value == null ? "—" : `${(value / GIB).toFixed(1)} GiB`;

type ModelOverview = { total: number; ready: number; preparing: number; failed: number; items: ModelListItem[] };
type StorageRow = { id: number; name: string; total?: number; available?: number; problems: number };
type StorageOverview = { rows: StorageRow[]; problems: { id: number; name: string; count: number }[]; failedDetails: number };
type DistributionRow = { name: string; vramGiB?: number };
type Issue = { kind: string; name: string; reason: string; to: string };
type AttentionGroup = { label: string; count: number; to: string };
type AttentionEntry = { to: string; title: string; content: ReactNode };

async function readModels(): Promise<ModelOverview> {
  const first = await api.modelsPage({ page: 1, page_size: 100 });
  const items = [...first.items];
  for (let page = 2; items.length < first.total && page <= 100; page++) {
    const next: ModelPage = await api.modelsPage({ page, page_size: 100 });
    if (!next.items.length) break;
    items.push(...next.items);
  }
  const complete = items.length >= first.total;
  return {
    total: first.total,
    ready: complete ? items.filter((model) => (model.presence?.ready_nodes ?? 0) + (model.presence?.ready_storages ?? 0) > 0).length : -1,
    preparing: complete ? items.filter((model) => (model.presence?.in_flight ?? 0) > 0).length : -1,
    failed: complete ? items.filter((model) => (model.presence?.failed ?? 0) > 0).length : -1,
    items,
  };
}

async function readStorageOverview(storages: Storage[], usage: StorageNodeUsage[]): Promise<StorageOverview> {
  const sharedStorages = storages.filter((storage) => storageTypeInfo(storage.type).shared);
  const results = await Promise.allSettled(sharedStorages.map((storage) => api.storage(storage.id, true)));
  const details = results.filter((result): result is PromiseFulfilledResult<Storage> => result.status === "fulfilled").map((result) => result.value);
  const usageByNode = new Map(usage.map((node) => [node.node_uuid, node]));
  const rows: StorageRow[] = [];
  const problems = storages.flatMap((storage) => {
    const count = (storage.nodes?.problems ?? []).filter((node) => node.state !== "offline").length;
    return count ? [{ id: storage.id, name: storage.name, count }] : [];
  });
  for (const storage of details) {
    const nodes = storage.nodes.all ?? [];
    const measured = nodes.flatMap((node) => {
      const fresh = usageByNode.get(node.node_uuid)?.path_usage?.find((path) => path.kind === "storage" && path.storage_id === storage.id);
      const report = fresh && fresh.total_bytes > 0 ? fresh : node;
      return report.total_bytes != null && report.total_bytes > 0 && report.available_bytes != null
        ? [{ total: report.total_bytes, available: report.available_bytes }]
        : [];
    });
    const shared = measured[0];
    rows.push({
      id: storage.id,
      name: storage.name,
      total: storage.total_bytes || shared?.total,
      available: storage.total_bytes ? storage.available_bytes : shared?.available,
      problems: problems.find((problem) => problem.id === storage.id)?.count ?? 0,
    });
  }
  return { rows, problems, failedDetails: results.length - details.length };
}

function physicalCapacityRows(gpus: GPUListResponse | null, unknownLabel: string): DistributionRow[] {
  if (!gpus) return [];
  const capacity = new Map<string, { bytes: number; known: boolean }>();
  for (const gpu of gpus.items.filter((card) => !card.ownership)) {
    const name = gpu.name || unknownLabel;
    const row = capacity.get(name) ?? { bytes: 0, known: true };
    row.known = row.known && Number.isFinite(gpu.memory_total_bytes) && gpu.memory_total_bytes > 0;
    row.bytes += gpu.memory_total_bytes || 0;
    capacity.set(name, row);
  }
  return [...capacity].map(([name, row]) => ({ name, vramGiB: row.known ? row.bytes / GIB : undefined })).sort((a, b) => (b.vramGiB ?? -1) - (a.vramGiB ?? -1) || a.name.localeCompare(b.name));
}

export default function DashboardPage() {
  const { t, i18n } = useTranslation("dashboard");
  const language = i18n.resolvedLanguage ?? "en";
  const [pickedLabelKey, setPickedLabelKey] = useState<string>();
  const [storagePage, setStoragePage] = useState(1);
  const health = usePolling<Health>(api.health, 10000);
  const summary = usePolling<DashboardSummary>(() => api.dashboardSummary(pickedLabelKey), 10000, [pickedLabelKey]);
  const providers = usePolling<List<CloudProvider>>(api.providers, 30000);
  const gpus = usePolling<GPUListResponse>(api.gpus, 60000);
  const models = usePolling<ModelOverview>(readModels, 60000);
  const offlineNodes = usePolling<NodePage>(() => api.nodesPage({ page: 1, page_size: 20, state: ["offline"] }), 30000);
  const monitor = usePolling<MonitorOverview>(() => api.monitorOverview("6h"), 30000);
  const storages = usePolling<List<Storage>>(api.storages, 30000);
  const storageUsage = usePolling<List<StorageNodeUsage>>(api.storageUsage, 30000);
  const storageIds = storages.data?.items.map((storage) => storage.id).join(",") ?? "";
  const storage = usePolling<StorageOverview>(() => readStorageOverview(storages.data?.items ?? [], storageUsage.data?.items ?? []), 30000, [storageIds, storageUsage.updatedAt]);
  const usage = usePolling<UsageAnalytics>(() => {
    const to = Date.now();
    return analyticsApi.usage({ from: to - DAY, to, granularity: "hour" });
  }, 30000);
  useEvents("node", () => { health.refetch(); summary.refetch(); gpus.refetch(); offlineNodes.refetch(); storageUsage.refetch(); });
  useEvents("deployment", () => { summary.refetch(); monitor.refetch(); });
  useEvents("provider", () => providers.refetch());

  const fleet = summary.data;
  const cloudList = providers.data?.items ?? [];
  const publicClouds = cloudList.filter((cloud) => cloud.kind !== "zstack");
  const privateClouds = cloudList.filter((cloud) => cloud.kind === "zstack");
  const invalidClouds = cloudList.filter((cloud) => cloud.state === "invalid");
  const gpuStates = gpus.data?.items.reduce((counts, gpu) => {
    if (gpu.allocation_state === "available" || gpu.allocation_state === "partial") counts.allocatable++;
    else if (gpu.allocation_state === "allocated") counts.allocated++;
    else if (gpu.allocation_state === "unusable") counts.unusable++;
    else counts.unknown++;
    return counts;
  }, { allocatable: 0, allocated: 0, unusable: 0, unknown: 0 });
  const otherNodes = fleet ? Math.max(0, fleet.nodes.total - fleet.nodes.online - fleet.nodes.connecting - fleet.nodes.offline) : undefined;
  const otherServices = fleet ? Math.max(0, fleet.deployments.total - fleet.deployments.running - fleet.deployments.warming) : undefined;
  const replicaSignal = monitor.data?.signals?.find((signal) => signal.key === "ready_replicas");
  const readyReplicas = replicaSignal?.collected ? replicaSignal.value : undefined;
  const desiredReplicas = replicaSignal?.collected ? replicaSignal.denominator : undefined;
  const requestRate = monitor.data?.signals?.find((signal) => signal.key === "request_rate");
  const errorRate = monitor.data?.signals?.find((signal) => signal.key === "error_rate");
  const ttft = monitor.data?.signals?.find((signal) => signal.key === "ttft");
  const waiting = monitor.data?.signals?.find((signal) => signal.key === "waiting");
  const monitorStatus = monitor.error ? t("overview.monitorUnavailable") : !monitor.data ? t("overview.loading") : monitor.data.stale ? t("overview.monitorStale") : requestRate?.detail?.no_recent_requests === true ? t("overview.monitorIdle") : t("overview.monitorWindow");
  const failedServices = (monitor.data?.services ?? []).filter((service) => service.state === "running" && ["failed", "timeout", "no_replica"].includes(service.health?.status));
  const unusableGpus = (gpus.data?.items ?? []).filter((gpu) => gpu.allocation_state === "unusable");
  const failedModels = (models.data?.items ?? []).filter((model) => (model.presence?.failed ?? 0) > 0);
  const modelIssuesIncomplete = models.data?.failed === -1;
  const failedModelCount = modelIssuesIncomplete ? failedModels.length : models.data?.failed ?? 0;
  const controlStatus = health.error ? undefined : health.data?.status;
  const currentGpu = summary.error ? undefined : fleet?.current_metrics;
  const gpuPower = currentGpu?.power_watts;
  const gpuPowerValue = gpuPower == null ? "—" : gpuPower >= 1000 ? `${(gpuPower / 1000).toFixed(2)} kW` : fmtWatts(gpuPower);
  const totals = usage.data?.totals;
  const totalTokens = totals ? totals.prompt_tokens + totals.completion_tokens : undefined;
  const usageLink = usage.data ? `/analytics/usage?range=custom&from=${usage.data.range.from_ms}&to=${usage.data.range.to_ms}&gran=${usage.data.range.granularity}` : "/analytics/usage";
  const storageIncomplete = !!(storages.error || storage.error || storage.data?.failedDetails);
  const storageRows = storage.data?.rows ?? [];
  const currentStoragePage = Math.min(storagePage, Math.max(1, Math.ceil(storageRows.length / STORAGE_PAGE_SIZE)));
  const attentionGroups: AttentionGroup[] = [
    { label: t("overview.offlineNodesGroup", { count: fmt(fleet?.nodes.offline ?? offlineNodes.data?.total ?? 0) }), count: fleet?.nodes.offline ?? offlineNodes.data?.total ?? 0, to: "/nodes" },
    { label: t("overview.unusableGpusGroup", { count: fmt(gpus.data?.overview.allocation?.unusable ?? unusableGpus.length) }), count: gpus.data?.overview.allocation?.unusable ?? unusableGpus.length, to: "/gpus" },
    { label: t("overview.failedServicesGroup", { count: fmt(failedServices.length) }), count: failedServices.length, to: "/monitor" },
    { label: t(modelIssuesIncomplete ? "overview.failedModelsGroupPartial" : "overview.failedModelsGroup", { count: fmt(failedModelCount) }), count: failedModelCount, to: "/models" },
    { label: t("overview.problemStoragesGroup", { count: fmt(storage.data?.problems.length ?? 0) }), count: storage.data?.problems.length ?? 0, to: "/storage" },
    { label: t("overview.invalidPublicCloudsGroup", { count: fmt(invalidClouds.filter((cloud) => cloud.kind !== "zstack").length) }), count: invalidClouds.filter((cloud) => cloud.kind !== "zstack").length, to: "/providers" },
    { label: t("overview.invalidPrivateCloudsGroup", { count: fmt(invalidClouds.filter((cloud) => cloud.kind === "zstack").length) }), count: invalidClouds.filter((cloud) => cloud.kind === "zstack").length, to: "/zstack" },
  ].filter((group) => group.count > 0);
  const attentionCount = attentionGroups.reduce((sum, group) => sum + group.count, 0);
  const issues: Issue[] = attentionCount <= ATTENTION_DETAIL_LIMIT ? [
    ...(offlineNodes.data?.items ?? []).map((node) => ({ kind: t("overview.issueNode"), name: node.name || node.hostname, reason: t("overview.offline"), to: "/nodes" })),
    ...unusableGpus.map((gpu) => ({ kind: "GPU", name: gpu.name, reason: t("overview.unavailableGpu"), to: `/gpus?gpu=${encodeURIComponent(gpu.uuid)}` })),
    ...failedServices.map((service) => ({ kind: t("overview.issueService"), name: service.name, reason: t("overview.probeFailed"), to: `/monitor/d/${service.deployment_id}` })),
    ...failedModels.map((model) => ({ kind: t("overview.models"), name: model.name, reason: t("overview.fileFailed"), to: `/models?detail=${model.id}` })),
    ...(storage.data?.problems ?? []).map((problem) => ({ kind: t("overview.storageTitle"), name: problem.name, reason: t("overview.storageProblem", { count: problem.count }), to: storageDetailPath(problem.id, "nodes") })),
    ...invalidClouds.map((cloud) => ({ kind: t("overview.clouds"), name: cloud.name, reason: t("overview.cloudProblem"), to: cloud.kind === "zstack" ? "/zstack" : "/providers" })),
  ] : [];
  const groupedAttention = attentionCount > ATTENTION_DETAIL_LIMIT || issues.length !== attentionCount || modelIssuesIncomplete;
  const attentionEntries = groupedAttention
    ? attentionGroups.map((group) => ({ to: group.to, title: group.label, content: <strong>{group.label}</strong> }))
    : issues.map((issue) => ({ to: issue.to, title: `${issue.kind} · ${issue.name} · ${issue.reason}`, content: <><span>{issue.kind}</span><strong>{issue.name}</strong><span>{issue.reason}</span></> }));
  const partial = [summary, providers, gpus, models, offlineNodes, monitor, storages, storageUsage, storage, usage].some((query) => !!query.error) || !!storage.data?.failedDetails || modelIssuesIncomplete;
  const vendorRows = (fleet?.by_vendor ?? []).filter((row) => row.gpus > 0).map((row: CapacityGroup) => ({ name: row.key, vramGiB: row.vram_gib > 0 ? row.vram_gib : undefined })).sort((a, b) => (b.vramGiB ?? -1) - (a.vramGiB ?? -1));
  const labelRows = (fleet?.by_label ?? []).filter((row) => row.gpus > 0).map((row: CapacityGroup) => ({ name: row.unlabeled ? t("capacity.unlabeled") : row.key.replace("=", ":"), vramGiB: row.vram_gib > 0 ? row.vram_gib : undefined })).sort((a, b) => (b.vramGiB ?? -1) - (a.vramGiB ?? -1));
  const ownedGpus = gpus.data?.items.filter((gpu) => !gpu.ownership);
  const modelVramKnown = ownedGpus?.every((gpu) => Number.isFinite(gpu.memory_total_bytes) && gpu.memory_total_bytes > 0);
  const modelVramGiB = modelVramKnown ? ownedGpus!.reduce((sum, gpu) => sum + gpu.memory_total_bytes / GIB, 0) : undefined;

  return <div className="dashboard-overview">
    <PageHeader className="page-header-wrap" title={t("title")} description={t("overview.description")} breadcrumb={[t("title")]} />
    {attentionEntries.length > 0 && <AttentionBanner entries={attentionEntries} grouped={groupedAttention} count={groupedAttention ? attentionGroups.length : attentionCount} />}
    {partial && <Alert type="warning" showIcon message={t("overview.unavailable")} className="dashboard-overview-alert" />}
    <section className="dashboard-overview-summary" aria-label={t("overview.platformSummary")}>
      <ResourceCard icon={<CloudServerOutlined />} title={t("overview.nodes")} value={fmt(fleet?.nodes.total)} unit={t("overview.nodeUnit")} to="/nodes" facts={<><StatusCount count={fleet?.nodes.online} label={t("overview.online", { count: fmt(fleet?.nodes.online) })} tone="success" /><StatusCount count={fleet?.nodes.connecting} label={t("overview.connecting", { count: fmt(fleet?.nodes.connecting) })} tone="warning" /><StatusCount count={fleet?.nodes.offline} label={t("overview.offlineCount", { count: fmt(fleet?.nodes.offline) })} tone="danger" /><StatusCount count={otherNodes} label={t("overview.otherStates", { count: fmt(otherNodes) })} tone="neutral" /></>} />
      <ResourceCard icon={<ControlOutlined />} title={t("overview.gpuResources")} value={fmt(gpus.data?.overview.total)} unit={t("overview.gpuUnit")} to="/gpus" facts={<><StatusCount count={gpuStates?.allocatable} label={t("overview.allocatable", { count: fmt(gpuStates?.allocatable) })} tone="success" /><StatusCount count={gpuStates?.allocated} label={t("overview.allocated", { count: fmt(gpuStates?.allocated) })} tone="info" /><StatusCount count={gpuStates?.unusable} label={t("overview.unavailableGpuCount", { count: fmt(gpuStates?.unusable) })} tone="danger" /><StatusCount count={gpuStates?.unknown} label={t("overview.unknownGpuCount", { count: fmt(gpuStates?.unknown) })} tone="neutral" /></>} />
      <ResourceCard icon={<RocketOutlined />} title={t("overview.deployments")} value={fmt(fleet?.deployments.total)} unit={t("overview.deploymentUnit")} to="/deployments" detail={fleet ? t("overview.sleepableCount", { count: fmt(fleet.deployments.sleepable) }) : undefined} facts={<><StatusCount count={fleet?.deployments.running} label={t("overview.running", { count: fmt(fleet?.deployments.running) })} tone="success" /><StatusCount count={fleet?.deployments.warming} label={t("overview.warming", { count: fmt(fleet?.deployments.warming) })} tone="warning" /><StatusCount count={otherServices} label={t("overview.otherStates", { count: fmt(otherServices) })} tone="neutral" /></>} />
      <ResourceCard icon={<CloudOutlined />} title={t("overview.clouds")} value={fmt(providers.data?.total)} unit={t("overview.cloudUnit")} entry={<CloudModuleArrow />} facts={<><StatusCount count={providers.data ? publicClouds.length : undefined} label={t("overview.publicClouds", { count: publicClouds.length })} /><StatusCount count={providers.data ? privateClouds.length : undefined} label={t("overview.privateClouds", { count: privateClouds.length })} /></>} />
    </section>
    <Card size="small" className="dashboard-overview-posture" aria-label={t("situation.title")}><strong>{t("situation.title")}</strong><span>{t("platform.version", { version: health.error ? "—" : health.data?.version || "—" })}</span><span>{t("overview.managementService")} <b className={controlStatus === "ok" ? "dashboard-overview-healthy" : undefined}>{controlStatus === "ok" ? t("overview.normal") : controlStatus ?? "—"}</b></span><span>{t("situation.activeAgentSessions", { count: fmt(health.error ? undefined : health.data?.sessions) })}</span></Card>
    <div className="dashboard-overview-grid dashboard-overview-inference">
      <Card size="small" className="dashboard-overview-panel dashboard-overview-usage"><PanelHead title={t("overview.usageTitle")} extra={<><span className="dashboard-overview-count">{t("overview.usageRange")}</span><ModuleArrow to={usageLink} label={t("overview.viewUsage")} /></>} />
        <div className="dashboard-overview-metrics">
          <Metric label={t("overview.totalTokens")} value={fmtCompact(totalTokens, language)} exact={fmtInt(totalTokens)} hint={totals?.no_usage ? t("overview.unreportedUsageHint", { count: fmt(totals.no_usage) }) : ""} tip={t("overview.totalTokensTip")} />
          <Metric label={t("overview.inputTokens")} value={fmtCompact(totals?.prompt_tokens, language)} exact={fmtInt(totals?.prompt_tokens)} tip={t("overview.inputTokensTip")} />
          <Metric label={t("overview.outputTokens")} value={fmtCompact(totals?.completion_tokens, language)} exact={fmtInt(totals?.completion_tokens)} tip={t("overview.outputTokensTip")} />
        </div>
      </Card>
      <Card size="small" className="dashboard-overview-panel dashboard-overview-monitor"><PanelHead title={t("overview.monitorSummary")} extra={<><Tooltip title={t("overview.replicaCountTip")}><span className="dashboard-overview-replica-count">{t("overview.readyReplicasLabel")} <strong>{fmt(readyReplicas)}/{fmt(desiredReplicas)}</strong></span></Tooltip><span className="dashboard-overview-count">{monitorStatus}</span><ModuleArrow to="/monitor?range=6h" label={t("overview.viewMonitor")} /></>} />
        <div className="dashboard-overview-metrics">
          <Metric label={t("overview.requestRate")} value={requestRate?.collected && requestRate.value != null ? `${requestRate.value.toFixed(1)} req/s` : "—"} tip={t("overview.requestRateTip")} />
          <Metric label={t("overview.errorRate")} value={errorRate?.collected ? fmtPct(errorRate.value) : "—"} tip={t("overview.errorRateTip")} />
          <Metric label={t("overview.ttftP95")} value={ttft?.collected && ttft.value != null ? `${ttft.value.toFixed(1)} ms` : "—"} tip={t("overview.ttftP95Tip")} />
          <Metric label={t("overview.waitingP95")} value={waiting?.collected && waiting.value != null ? waiting.value.toFixed(1) : "—"} tip={t("overview.waitingP95Tip")} />
        </div>
      </Card>
    </div>
    <div className="dashboard-overview-grid dashboard-overview-composition">
      <Card size="small" className="dashboard-overview-panel"><PanelHead title={t("overview.gpuComposition")} extra={<ModuleArrow to="/gpus" label={t("overview.viewGpu")} />} />
        <div className="dashboard-overview-composition-grid">
          <Distribution title={t("overview.byVendor")} rows={vendorRows} totalVramGiB={fleet?.nodes.vram_gib} empty={t("overview.noGpu")} />
          <Distribution title={t("overview.byModel")} rows={physicalCapacityRows(gpus.data, t("overview.unknownGpu"))} totalVramGiB={modelVramGiB} empty={t("overview.noGpu")} paginated />
        </div>
      </Card>
      <Card size="small" className="dashboard-overview-panel"><PanelHead title={t("overview.labelDistribution")} extra={<>{(fleet?.label_keys.length ?? 0) > 1 && <Select size="small" variant="borderless" className="dashboard-overview-label-key" value={fleet?.label_key || undefined} aria-label={t("capacity.labelKeyAria")} onChange={setPickedLabelKey} options={(fleet?.label_keys ?? []).map((key) => ({ label: key, value: key }))} />}<ModuleArrow to="/nodes" label={t("overview.viewNodes")} /></>} />
        <div className="dashboard-overview-panel-body"><Distribution key={fleet?.label_key} rows={labelRows} totalVramGiB={fleet?.nodes.vram_gib} empty={fleet?.nodes.gpus && !fleet.label_keys.length ? t("overview.noLabels") : t("overview.noGpu")} paginated /></div>
      </Card>
    </div>
    <div className="dashboard-overview-grid dashboard-overview-capacity">
      <Link to="/analytics/gpus" className="dashboard-overview-gpu-link" aria-label={t("overview.viewGpuUsage")}>
        <Card size="small" className="dashboard-overview-panel dashboard-overview-gpu-usage">
          <PanelHead title={t("overview.gpuUsageTitle")} extra={<><span className="dashboard-overview-count">{t("overview.currentSample")}</span><span className="dashboard-overview-module-arrow" aria-hidden="true"><ArrowRightOutlined /></span></>} />
          <div className="dashboard-overview-metrics dashboard-overview-gpu-metrics">
            <Metric label={t("overview.averageGpuUtilization")} value={fmtPct(currentGpu?.utilization_pct)} />
            <Metric label={t("overview.maximumGpuTemperature")} value={fmtCelsius(currentGpu?.temperature_c)} />
            <Metric label={t("overview.totalGpuPower")} value={gpuPowerValue} />
          </div>
        </Card>
      </Link>
      <Card size="small" className="dashboard-overview-panel"><PanelHead title={t("overview.storageCapacity")} extra={<><span className="dashboard-overview-count">{t("overview.storageCount", { count: storages.data?.items.filter((item) => storageTypeInfo(item.type).shared).length ?? 0 })}</span><ModuleArrow to="/storage" label={t("overview.viewStorage")} /></>} />
        <div className="dashboard-overview-panel-body">{storageRows.length ? <><div className={storageRows.length > STORAGE_PAGE_SIZE ? "dashboard-overview-storage-page" : undefined}>{storageRows.slice((currentStoragePage - 1) * STORAGE_PAGE_SIZE, currentStoragePage * STORAGE_PAGE_SIZE).map((row) => <StorageCapacityRow key={row.id} row={row} />)}</div>{storageRows.length > STORAGE_PAGE_SIZE && <Pagination className="dashboard-overview-storage-pagination" size="small" current={currentStoragePage} pageSize={STORAGE_PAGE_SIZE} total={storageRows.length} showSizeChanger={false} onChange={setStoragePage} />}</> : <EmptyState title={storageIncomplete ? t("overview.unavailable") : t("overview.noStorage")} detail={storageIncomplete ? "" : t("overview.noStorageHint")} />}</div>
      </Card>
    </div>
  </div>;
}

function PanelHead({ title, extra }: { title: string; extra?: ReactNode }) { return <div className="dashboard-overview-panel-head"><h2>{title}</h2><div>{extra}</div></div>; }
function AttentionBanner({ entries, grouped, count }: { entries: AttentionEntry[]; grouped: boolean; count: number }) {
  const { t } = useTranslation("dashboard");
  const contentRef = useRef<HTMLDivElement>(null);
  const [visibleCount, setVisibleCount] = useState(2);
  const entryTitles = entries.map((entry) => entry.title).join("\n");
  useLayoutEffect(() => {
    const content = contentRef.current;
    if (!content) return;
    const update = () => {
      if (!content.clientWidth) return;
      const headingWidth = content.querySelector<HTMLElement>(".dashboard-overview-attention-heading")?.offsetWidth ?? 0;
      const entryWidths = [...content.querySelectorAll<HTMLElement>(".dashboard-overview-attention-measure-entry")].map((element) => element.offsetWidth);
      const moreWidths = [...content.querySelectorAll<HTMLElement>(".dashboard-overview-attention-measure-more")].map((element) => element.offsetWidth);
      for (let visible = entryWidths.length; visible >= 0; visible--) {
        const itemsWidth = entryWidths.slice(0, visible).reduce((sum, width) => sum + width, 0) + Math.max(0, visible - 1) * 8;
        const hidden = entryWidths.length - visible;
        const required = headingWidth + (visible ? 12 + itemsWidth : 0) + (hidden ? 12 + moreWidths[hidden - 1] : 0);
        if (required <= content.clientWidth) {
          setVisibleCount(Math.max(1, visible));
          return;
        }
      }
      setVisibleCount(1);
    };
    update();
    const observer = typeof ResizeObserver === "undefined" ? undefined : new ResizeObserver(update);
    observer?.observe(content);
    window.addEventListener("resize", update);
    return () => { observer?.disconnect(); window.removeEventListener("resize", update); };
  }, [entryTitles, grouped, count]);
  const shown = Math.min(visibleCount, entries.length);
  const hidden = entries.slice(shown);
  const moreLabel = (amount: number) => t(grouped ? "overview.moreCategories" : "overview.moreIssues", { count: amount });
  return <Alert type="warning" showIcon className="dashboard-overview-attention" message={<div ref={contentRef} className="dashboard-overview-attention-content">
    <span className="dashboard-overview-attention-heading">{t("overview.attention")}{" "}<Tag color="warning" className="dashboard-overview-attention-count">{grouped ? t("overview.attentionCategories", { count }) : count}</Tag></span>
    <div className="dashboard-overview-attention-items">{entries.slice(0, shown).map((entry, index) => <Link key={index} to={entry.to} title={entry.title} className="dashboard-overview-attention-link">{entry.content}</Link>)}</div>
    {hidden.length > 0 && <Dropdown trigger={["click"]} menu={{ items: hidden.map((entry, index) => ({ key: index, label: <Link to={entry.to}>{entry.title}</Link> })) }}><Button type="link" size="small" className="dashboard-overview-attention-more">{moreLabel(hidden.length)}</Button></Dropdown>}
    <div className="dashboard-overview-attention-measure" aria-hidden="true">{entries.map((entry, index) => <span key={index} className="dashboard-overview-attention-measure-entry">{entry.content}</span>)}{entries.map((_, index) => <Button key={index} type="link" size="small" tabIndex={-1} className="dashboard-overview-attention-more dashboard-overview-attention-measure-more">{moreLabel(index + 1)}</Button>)}</div>
  </div>} />;
}
function EmptyState({ title, detail }: { title: string; detail: string }) { return <div className="dashboard-overview-empty"><strong>{title}</strong><span>{detail}</span></div>; }
function StatusCount({ count, label, tone }: { count?: number; label: string; tone?: "success" | "warning" | "danger" | "info" | "neutral" }) { return count != null && count > 0 ? <span className={tone ? `dashboard-overview-status dashboard-overview-status-${tone}` : undefined}>{label}</span> : null; }
function ModuleArrow({ to, label }: { to: string; label: string }) { return <Link to={to} className="dashboard-overview-module-arrow" aria-label={label} title={label}><ArrowRightOutlined /></Link>; }
function CloudModuleArrow() {
  const { t } = useTranslation("dashboard");
  return <Dropdown trigger={["click"]} menu={{ items: [
    { key: "public", label: <Link to="/providers">{t("overview.publicCloud")}</Link> },
    { key: "private", label: <Link to="/zstack">{t("overview.privateCloud")}</Link> },
  ] }}><Button type="text" size="small" className="dashboard-overview-module-arrow" icon={<ArrowRightOutlined />} aria-label={t("overview.clouds")} title={t("overview.clouds")} /></Dropdown>;
}
function ResourceCard({ icon, title, value, unit, to, entry, detail, facts }: { icon: ReactNode; title: string; value: string; unit: string; to?: string; entry?: ReactNode; detail?: string; facts: ReactNode }) {
  return <Card size="small" className="dashboard-overview-resource"><div className="dashboard-overview-resource-title">{icon}<span>{title}</span>{to ? <ModuleArrow to={to} label={title} /> : entry}</div><div className="dashboard-overview-resource-value">{value}<small>{unit}</small>{detail && <span className="dashboard-overview-resource-detail">{detail}</span>}</div><div className="dashboard-overview-resource-facts">{facts}</div></Card>;
}
function Metric({ label, value, exact, hint, tip }: { label: string; value: string; exact?: string; hint?: string; tip?: string }) {
  return <div className="dashboard-overview-metric"><span>{label}{tip && <Tooltip title={tip}><InfoCircleOutlined className="dashboard-overview-metric-tip" tabIndex={0} role="img" aria-label={tip} /></Tooltip>}</span><strong title={exact}>{value}</strong>{hint && <small>{hint}</small>}</div>;
}
function Distribution({ title, rows, totalVramGiB, empty, paginated = false }: { title?: string; rows: DistributionRow[]; totalVramGiB?: number; empty: string; paginated?: boolean }) {
  const [page, setPage] = useState(1);
  const currentPage = Math.min(page, Math.max(1, Math.ceil(rows.length / DISTRIBUTION_PAGE_SIZE)));
  const visibleRows = paginated ? rows.slice((currentPage - 1) * DISTRIBUTION_PAGE_SIZE, currentPage * DISTRIBUTION_PAGE_SIZE) : rows;
  return <div className="dashboard-overview-distribution">
    <h3>{title}</h3>
    {rows.length ? <div className={paginated && rows.length > DISTRIBUTION_PAGE_SIZE ? "dashboard-overview-distribution-rows-paged" : undefined}>{visibleRows.map((row) => <div key={row.name} className="dashboard-overview-distribution-row"><span title={row.name}>{row.name}</span><span>{row.vramGiB == null ? "—" : `${row.vramGiB.toFixed(1)} GiB`}</span>{row.vramGiB != null && totalVramGiB != null && totalVramGiB > 0 && <i><b style={{ width: `${Math.min(100, row.vramGiB / totalVramGiB * 100)}%` }} /></i>}</div>)}</div> : <Typography.Text type="secondary">{empty}</Typography.Text>}
    {paginated && rows.length > DISTRIBUTION_PAGE_SIZE && <Pagination className="dashboard-overview-distribution-pagination" size="small" current={currentPage} pageSize={DISTRIBUTION_PAGE_SIZE} total={rows.length} showSizeChanger={false} onChange={setPage} />}
  </div>;
}
function StorageCapacityRow({ row }: { row: StorageRow }) {
  const { t } = useTranslation("dashboard");
  const used = row.total != null && row.available != null ? Math.max(0, row.total - row.available) : undefined;
  const percent = used != null && row.total ? Math.min(100, used / row.total * 100) : 0;
  return <div className="dashboard-overview-storage-row"><DatabaseOutlined /><div><Link to={storageDetailPath(row.id)}>{row.name}</Link>{row.problems > 0 && <small>{t("overview.storageProblem", { count: row.problems })}</small>}</div><div className="dashboard-overview-storage-capacity"><span>{used == null ? t("overview.capacityUnknown") : t("overview.usedStorage", { used: gib(used), total: gib(row.total) })}</span>{row.available != null && <span>{t("overview.availableStorage", { size: gib(row.available) })}</span>}</div>{used != null && <i className="dashboard-overview-storage-progress"><b style={{ width: `${percent}%` }} /></i>}</div>;
}
