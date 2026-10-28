import { playgroundRoute } from "./playground/route";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  Alert,
  Badge,
  Button,
  Card,
  Checkbox,
  Descriptions,
  Dropdown,
  Drawer,
  Input,
  Modal,
  Space,
  Select,
  Tag,
  Tooltip,
  Typography,
  message,
} from "antd";
import { ApiOutlined, CodeOutlined, CopyOutlined, DeleteOutlined, EditOutlined, ExperimentOutlined, MessageOutlined, MoreOutlined } from "@ant-design/icons";
import { api, useEventPayload, useEvents, usePolling, type ChangeEvent, type Deployment, type DeploymentListItem, type Instance, type RoutingPolicyOption } from "../api";
import { DeploymentPreparation, ListPreparationAction, PreparationLabel } from "./DeploymentPreparation";
import DeploymentMetricsPanel from "../components/DeploymentMetricsPanel";
import RoutingPolicyHelp from "../components/RoutingPolicyHelp";
import { effectiveDeploymentState, partlyAsleep, stateLabel } from "./state";
import DeploymentPdPanel from "./DeploymentPdPanel";
import DeploymentInstances from "./DeploymentInstances";
import SleepControls from "./DeploymentSleepControls";
import DeploymentNodeReplacement from "./DeploymentNodeReplacement";
import PlacementModeControl from "./DeploymentPlacementMode";
import DeploymentAutomation from "./DeploymentAutomation";
import DeploymentLifecycleActions, { ReplicaRetryAlert } from "./DeploymentLifecycleActions";
import { ReplicasEditor } from "./DeploymentReplicaActions";
import { PlacementStatusAlert, RequirementTags } from "./DeploymentPlacementStatus";
import { useAssistantSubjects } from "../assistant/AssistantContext";
import { deploymentSubjects } from "../assistant/pageSubjects";
import { EditRequirementsButton } from "./DeploymentRequirementsModal";
import ResourceTable, { storedPageSize, type ResourceColumn, type ServerSort } from "../components/resourceTable/ResourceTable";
import { formatTime } from "../i18n/datetime";
import { usePagedList } from "../pagedList";
import PageHeader from "../components/PageHeader";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { createRefreshCoalescer } from "./evaluations/evaluationRefresh";
import { localizedText } from "../i18n/localizedText";
import { apiErrorText } from "../apiError";
import { useCopy } from "../components/CopyButton";
import { HealthCheckHelp, HealthTag } from "../components/DeploymentHealth";
import { HealthCheckEditor } from "./DeploymentHealthEditor";
import { TokenizeEditor } from "./DeploymentTokenizeEditor";
import { TokenizeHelp } from "../components/DeploymentTokenize";

const DEPLOYMENT_PAGE_SIZE = 20;
const DEPLOYMENT_TABLE_ID = "deployments";
const DEPLOYMENT_LIST_STATES = ["running", "sleeping", "asleep", "offline", "starting", "pulling", "pending", "failed", "stopped", "deleting"];

export function deploymentTerminalInstances(deployment: Deployment): Instance[] {
  return (deployment.instances || []).filter((instance) => ["starting", "optimizing", "running"].includes(instance.state) && Boolean(instance.node_uuid && instance.container_id));
}

export function containerTerminalURL(instance: Instance): string {
  return `/nodes/${encodeURIComponent(instance.node_uuid)}/terminal?target=container&ref=${encodeURIComponent(instance.container_id || "")}`;
}

// The list carries ready/desired totals for ordinary and PD deployments.
// Missing readiness stays unknown rather than looking like a ready count.
function replicaCount(d: DeploymentListItem): string {
  const desired = d.replicas ?? 0;
  return `${d.ready_replicas ?? "—"}/${desired}`;
}

// asleepActionHint is the tooltip on an action the deployment cannot take yet.
// A sleep order explains itself: the runtime is fine, and requests are refused
// until someone wakes the deployment, which is not what "not running yet" says.
function asleepActionHint(d: DeploymentListItem, t: (key: string) => string): string {
  switch (effectiveDeploymentState(d)) {
    case "asleep":
    case "sleeping":
      return t("page.asleepUnavailable");
    case "running":
      return "";
    default:
      return t("page.runtimeUnavailable");
  }
}

export default function DeploymentsPage() {
  const { t, i18n } = useTranslation("deployments");
  const { t: tCommon } = useTranslation("common");
  const [copy, copyHolder] = useCopy();
  const navigate = useNavigate();
  const [searchParams, setSearchParams] = useSearchParams();
  // The server pages, searches and filters: at thousands of deployments the
  // page used to fetch and render every one of them every few seconds.
  const [search, setSearch] = useState("");
  const [states, setStates] = useState<string[]>([]);
  const [sort, setSort] = useState<ServerSort | null>(null);
  const { data, refetch, refresh, loading, error, updatedAt, pagination } = usePagedList(api.deploymentsPage, {
    filters: { q: search, state: states, ...(sort ? { sort: `${sort.order === "desc" ? "-" : ""}${sort.key}` } : {}) },
    pageSize: storedPageSize(DEPLOYMENT_TABLE_ID, DEPLOYMENT_PAGE_SIZE),
    intervalMs: 8000,
  });
  const filtered = search.trim() !== "" || states.length > 0;
  const routingPolicies = usePolling(api.routingPolicies, 0);
  const [detailId, setDetailId] = useState<number | null>(null);
  const routingOptions = useMemo(
    () => (routingPolicies.data?.items ?? []).map((policy) => ({ value: policy.value, label: localizedText(policy.label, i18n.language) })),
    [routingPolicies.data, i18n.language],
  );
  // Image pulls report progress frequently. SSE updates refresh the existing
  // rows at most once per second so an event stream cannot keep the initial
  // request in a perpetual trailing-refresh loop.
  const refreshRef = useRef(refresh);
  refreshRef.current = refresh;
  const eventRefresh = useMemo(() => createRefreshCoalescer(() => refreshRef.current(), 1000), []);
  useEffect(() => () => eventRefresh.dispose(), [eventRefresh]);
  // No column of the list shows byte counters, and with a fleet downloading
  // there is always a progress notice in flight: reloading every deployment for
  // each would never stop. The open drawer below still follows progress, for the
  // one deployment it shows.
  const refreshUnlessProgress = (event: ChangeEvent) => { if (event.reason !== "progress") eventRefresh.trigger(); };
  useEvents("deployment", eventRefresh.trigger);
  useEventPayload("instance", refreshUnlessProgress);
  useEventPayload("modelfile", refreshUnlessProgress);
  useEventPayload("node", (event) => { if (event.reason?.startsWith("state:")) eventRefresh.trigger(); });
  useEffect(() => {
    const requested = Number(searchParams.get("deployment"));
    if (Number.isInteger(requested) && requested > 0) setDetailId(requested);
  }, [searchParams]);
  const openDetail = (id: number) => { setDetailId(id); setSearchParams({ deployment: String(id) }); };
  const closeDetail = () => { setDetailId(null); setSearchParams({}); };
  const clone = async (dep: DeploymentListItem) => { try { await api.cloneDeployment(dep.id); message.success(t("page.cloneCreated")); refetch(); } catch (e) { message.error(t("page.cloneFailed", { detail: apiErrorText(tCommon, e) })); } };
  const benchmark = (dep: DeploymentListItem) => navigate(`/evaluations?create=saturation&deployment_id=${dep.id}`);
  const openTerminal = async (deployment: DeploymentListItem) => {
    const tab = window.open("about:blank", "_blank");
    if (tab) tab.opener = null;
    try {
      const detail = await api.deployment(deployment.id);
      const instance = deploymentTerminalInstances(detail)[0];
      if (!instance) throw new Error(t("instanceTerminal.unavailable"));
      const target = containerTerminalURL(instance);
      if (tab) tab.location.replace(target);
      else window.location.assign(target);
    } catch (error) {
      tab?.close();
      message.error(t("instanceTerminal.openFailed", { detail: (error as Error).message }));
    }
  };
  const [deleting, setDeleting] = useState<DeploymentListItem | null>(null);
  const copyCurl = async (dep: DeploymentListItem) => { await copy(deploymentCurl(window.location.origin, dep), { success: t("page.curlCopied") }); };

  const columns: ResourceColumn<DeploymentListItem>[] = [
    {
      key: "name",
      role: "name",
      title: t("page.column.name"),
      width: 220,
      serverSort: true,
      render: (_: unknown, d: DeploymentListItem) => <Button type="link" onClick={() => openDetail(d.id)}>{d.name}</Button>,
    },
    { key: "served_name", title: t("page.column.servedName"), width: 220, render: (_: unknown, d: DeploymentListItem) => d.served_name ? <Typography.Text copyable>{d.served_name}</Typography.Text> : "—" },
    { key: "model", title: t("page.column.model"), width: 220, serverSort: true, render: (_: unknown, d: DeploymentListItem) => d.model_name || "—" },
    {
      key: "runtime",
      title: t("page.column.runtime"),
      width: 220,
      render: (_: unknown, d: DeploymentListItem) => <><span>{d.engine || "—"}</span><Typography.Text type="secondary" className="resource-table-sub">{d.image || t("page.autoImage")}</Typography.Text></>,
    },
    {
      key: "node",
      title: t("page.column.node"),
      width: 160,
      // The uuid identifies the node; the name is what an operator recognizes.
      render: (_: unknown, d: DeploymentListItem) => deploymentNodesLabel(d, d.node_uuid),
    },
    {
      key: "state",
      title: t("page.column.state"),
      width: 120,
      serverSort: true,
      filters: DEPLOYMENT_LIST_STATES.map((state) => ({ value: state, text: stateLabel(state, tCommon).text })),
      filteredValue: states,
      render: (_: unknown, deployment: DeploymentListItem) => {
        const state = effectiveDeploymentState(deployment);
        const b = stateLabel(state, tCommon);
        // A partly slept deployment still serves, so it keeps the running
        // text; the count beside it says how much of it is awake, which a
        // plain green badge would hide.
        const partial = partlyAsleep(deployment);
        // A failed model preparation is the reason the row is not running, so
        // the retry is offered here, where that state is read - not only in
        // the drawer behind it (GWKS-143).
        return <>
          <Badge status={partial ? "processing" : b.status} text={b.text} />
          {partial && <Typography.Text type="secondary" className="resource-table-sub">{t("sleep.partial", { asleep: deployment.asleep_replicas, total: deployment.replicas ?? 0 })}</Typography.Text>}
          {deployment.preparation && <Typography.Text type="secondary" className="resource-table-sub"><PreparationLabel item={deployment.preparation} /></Typography.Text>}
          {deployment.preparation && <ListPreparationAction item={deployment.preparation} onChanged={refetch} />}
        </>;
      },
    },
    { key: "health", title: <Space size={0}>{t("page.column.health")}<HealthCheckHelp /></Space>, width: 140, render: (_: unknown, d: DeploymentListItem) => <HealthTag health={d.health} /> },
    // A list row carries the replica counts; the detail lists the instances.
    { key: "replicas", title: t("page.column.instances"), width: 160, align: "right", serverSort: true, render: (_: unknown, d: DeploymentListItem) => replicaCount(d) },
    { key: "created_at", title: t("page.column.createdAt"), width: 180, serverSort: true, render: (_: unknown, d: DeploymentListItem) => formatTime(d.created_at) },
    {
      key: "action",
      role: "actions",
      width: 120,
      render: (_: unknown, d: DeploymentListItem) => {
    return <Space size={4}>
      <Tooltip title={effectiveDeploymentState(d) === "running" ? `${t("page.openPlayground")} · ${(d.operations || []).map((operation) => operation.type).join(", ")}` : asleepActionHint(d, t)}><Button disabled={effectiveDeploymentState(d) !== "running" || !d.operations?.length} type="text" size="small" icon={<MessageOutlined />} onClick={() => d.operations?.[0] && navigate(playgroundRoute(d.operations[0], d.served_name || d.model_name))} aria-label={t("page.openPlayground")} /></Tooltip>
      <Tooltip title={effectiveDeploymentState(d) === "running" ? t("page.copyCurl") : asleepActionHint(d, t)}><Button disabled={effectiveDeploymentState(d) !== "running"} type="text" size="small" icon={<ApiOutlined />} onClick={() => void copyCurl(d)} aria-label={t("page.copyCurl")} /></Tooltip>
      <Dropdown trigger={["click"]} menu={{ items: [
      // A sleeping engine's container is still there to inspect; that is how
      // an operator confirms the sleep state, so the terminal stays open.
      { key: "terminal", icon: <CodeOutlined />, disabled: !["starting", "optimizing", "running", "sleeping", "asleep"].includes(effectiveDeploymentState(d)), label: t("instanceTerminal.open") },
      { key: "edit", icon: <EditOutlined />, disabled: d.desired_state === "deleting", label: t("page.edit") },
      { key: "clone", icon: <CopyOutlined />, label: t("page.clone") },
      { key: "cloneModify", icon: <CopyOutlined />, label: t("page.cloneModify") },
      { key: "benchmark", icon: <ExperimentOutlined />, disabled: effectiveDeploymentState(d) !== "running", label: t("page.benchmark") },
      { type: "divider" },
      { key: "delete", icon: <DeleteOutlined />, danger: true, label: t("delete.button") },
      ], onClick: ({ key }) => { if (key === "terminal") void openTerminal(d); else if (key === "edit") navigate(`/deploy?edit=${d.id}`); else if (key === "clone") void clone(d); else if (key === "cloneModify") navigate(`/deploy?from=${d.id}`); else if (key === "benchmark") void benchmark(d); else if (key === "delete") setDeleting(d); } }}><Tooltip title={t("page.more")}><Button type="text" size="small" icon={<MoreOutlined />} aria-label={t("page.more")} /></Tooltip></Dropdown>
        </Space>;
    },
    },
  ];

  return (
    <>
      {copyHolder}
      <PageHeader title={t("page.title")} description={t("page.description")} extra={<Typography.Text type="secondary">{t("page.count", { count: data?.total ?? 0 })}</Typography.Text>} />
      <Card>
        <ResourceTable<DeploymentListItem>
          tableId={DEPLOYMENT_TABLE_ID}
          rowKey="id"
          columns={columns}
          data={data?.items ?? []}
          loading={loading}
          error={error}
          onRefresh={refetch}
          updatedAt={updatedAt}
          pagination={pagination}
          sort={sort}
          onSortChange={setSort}
          onFiltersChange={(filters) => setStates(filters.state ?? [])}
          toolbar={
            <Input.Search
              allowClear
              style={{ width: 280 }}
              placeholder={t("page.search")}
              aria-label={t("page.search")}
              onSearch={(value) => setSearch(value.trim())}
            />
          }
          emptyDescription={t(filtered ? "page.emptyFiltered" : "page.empty")}
          emptyKind="deployments"
          emptyAction={filtered ? undefined : <Button type="primary" onClick={() => location.assign("/deploy")}>{t("page.create")}</Button>}
        />
      </Card>
      <DeleteDeploymentModal dep={deleting} onClose={() => setDeleting(null)} onDeleted={refetch} />
      <DeploymentDetail
        id={detailId}
        focusInstance={searchParams.get("instance")}
        routingPolicies={routingPolicies.data?.items ?? []}
        routingOptions={routingOptions}
        onClose={closeDetail}
        onStopped={() => {
          closeDetail();
          refetch();
        }}
      />
    </>
  );
}

// The detail drawer's delete button; the list offers the same modal from the
// row's "more" menu.
function DeleteDeploymentButton({ dep, onDeleted }: { dep: Pick<Deployment, "id" | "name">; onDeleted: () => void }) {
  const { t } = useTranslation("deployments");
  const [open, setOpen] = useState(false);
  return (
    <>
      <Button danger icon={<DeleteOutlined />} onClick={() => setOpen(true)}>
        {t("delete.button")}
      </Button>
      <DeleteDeploymentModal dep={open ? dep : null} onClose={() => setOpen(false)} onDeleted={onDeleted} />
    </>
  );
}

function DeleteDeploymentModal({
  dep,
  onClose,
  onDeleted,
}: {
  dep: Pick<Deployment, "id" | "name"> | null;
  onClose: () => void;
  onDeleted: () => void;
}) {
  const { t } = useTranslation("deployments");
  const { t: tCommon } = useTranslation("common");
  const [force, setForce] = useState(false);
  const [deleting, setDeleting] = useState(false);

  const close = () => {
    if (deleting) return;
    setForce(false);
    onClose();
  };

  const remove = async () => {
    if (!dep) return;
    setDeleting(true);
    try {
      await api.deleteDeployment(dep.id, force);
      message.success(force ? t("delete.forced") : t("delete.started"));
      setForce(false);
      onClose();
      onDeleted();
    } catch (e) {
      message.error(t("delete.failed", { detail: apiErrorText(tCommon, e) }));
    } finally {
      setDeleting(false);
    }
  };

  return (
    <Modal
      title={t("delete.title", { name: dep?.name ?? "" })}
      open={dep != null}
      onCancel={close}
      onOk={remove}
      okText={force ? t("delete.forceButton") : t("delete.button")}
      cancelText={t("delete.cancel")}
      okButtonProps={{ danger: true, loading: deleting }}
      cancelButtonProps={{ disabled: deleting }}
      maskClosable={!deleting}
      closable={!deleting}
    >
      <Typography.Paragraph>
        {t("delete.description")}
      </Typography.Paragraph>
      <Checkbox checked={force} onChange={(event) => setForce(event.target.checked)}>
        {t("delete.force")}
      </Checkbox>
      {force && (
        <Alert
          style={{ marginTop: 16 }}
          type="warning"
          showIcon
          message={t("delete.forceWarning")}
          description={t("delete.forceDescription")}
        />
      )}
    </Modal>
  );
}

function DeploymentDetail({
  id,
  routingPolicies,
  routingOptions,
  focusInstance,
  onClose,
  onStopped,
}: {
  id: number | null;
  focusInstance?: string | null;
  routingPolicies: RoutingPolicyOption[];
  routingOptions: Array<{ value: RoutingPolicyOption["value"]; label: string }>;
  onClose: () => void;
  onStopped: () => void;
}) {
  const { t } = useTranslation("deployments");
  const { t: tCommon } = useTranslation("common");
  const [dep, setDep] = useState<Deployment | null>(null);
  const [routingBusy, setRoutingBusy] = useState(false);
  const { t: tAI } = useTranslation("ai");
  useAssistantSubjects(dep && id != null ? deploymentSubjects(dep, tAI) : null);
  const currentIDRef = useRef(id);
  currentIDRef.current = id;

  const load = useCallback(async () => {
    if (id == null) return;
    try {
      const next = await api.deployment(id);
      if (currentIDRef.current === id) setDep(next);
    } catch {
      // The next event or explicit action retries transient detail failures.
    }
  }, [id]);
  const eventRefresh = useMemo(() => createRefreshCoalescer(load, 1000), [load]);

  useEffect(() => {
    setDep(null);
    eventRefresh.trigger();
    return () => eventRefresh.dispose();
  }, [eventRefresh]);
  useEvents("instance", eventRefresh.trigger);
  useEvents("deployment", eventRefresh.trigger);
  useEvents("modelfile", eventRefresh.trigger);
  useEventPayload("node", (event) => { if (event.reason?.startsWith("state:")) eventRefresh.trigger(); });

  const changeRouting = async (routing_policy: string) => {
    if (!dep || dep.replicas <= 1) return;
    const policy = routingPolicies.find((item) => item.value === routing_policy);
    if (!policy) return;
    setRoutingBusy(true);
    try { await api.setDeploymentRouting(dep.id, policy.value); await load(); message.success(t("detail.routingUpdated")); }
    catch (e) { message.error(t("detail.routingFailed", { detail: apiErrorText(tCommon, e) })); }
    finally { setRoutingBusy(false); }
  };

  const b = dep ? stateLabel(effectiveDeploymentState(dep), tCommon) : null;

  return (
    <Drawer
      title={dep?.name || t("detail.title")}
      open={id != null}
      onClose={onClose}
      width={720}
      extra={
        <Space>
          {dep && <DeploymentLifecycleActions dep={dep} onChanged={load} />}
          {dep && <DeleteDeploymentButton dep={dep} onDeleted={onStopped} />}
        </Space>
      }
    >
      {dep && (
        <>
          <Descriptions column={2} bordered size="small">
            <Descriptions.Item label={t("detail.model")}><Link to={`/models?detail=${dep.model_id}`}>{dep.model_name || dep.model_id}</Link></Descriptions.Item>
            <Descriptions.Item label={t("detail.servedName")}>{dep.served_name || "—"}</Descriptions.Item>
            <Descriptions.Item label={t("detail.node")}><DeploymentNodeLinks dep={dep} /></Descriptions.Item>
            {dep.placement_mode && <Descriptions.Item label={t("unitsPlacement.label")} span={2}><PlacementModeControl dep={dep} onChanged={() => void load()} /></Descriptions.Item>}
            {!dep.pd && (dep.nnodes ?? 1) <= 1 && <Descriptions.Item label={t("detail.placement.requirements")} span={2}><Space size={4} wrap><RequirementTags dep={dep} />{dep.placement_strategy && <Tag color="blue">{t(`detail.placement.strategy.${dep.placement_strategy}`, { defaultValue: dep.placement_strategy })}</Tag>}<EditRequirementsButton dep={dep} onChanged={() => void load()} /></Space></Descriptions.Item>}
            <Descriptions.Item label={t("detail.state")}>
              {/* A partly slept deployment serves, so it stays running here too,
                  with the sleep count beside it rather than a plain badge. */}
              {b && <Space size={4}><Badge status={dep && partlyAsleep(dep) ? "processing" : b.status} text={b.text} />{dep && partlyAsleep(dep) && <Typography.Text type="secondary">{t("sleep.partial", { asleep: dep.asleep_replicas, total: dep.replicas })}</Typography.Text>}</Space>}
            </Descriptions.Item>
            <Descriptions.Item label={<Space size={0}>{t("detail.health")}<HealthCheckHelp /></Space>}><HealthCheckEditor key={`${dep.id}-${dep.health.threshold_sec}`} dep={dep} onChanged={load} /></Descriptions.Item>
            <Descriptions.Item label={<Space size={0}>{t("detail.tokenize")}<TokenizeHelp /></Space>}><TokenizeEditor dep={dep} onChanged={load} /></Descriptions.Item>
            <Descriptions.Item label={t("detail.sleepable")}>{dep.sleepable ? t("detail.yes") : t("detail.no")}</Descriptions.Item>
              <Descriptions.Item label={t("detail.replicas")}><Space size={8}>{dep.ready_replicas != null && <Typography.Text type={dep.ready_replicas < dep.replicas ? "warning" : undefined}>{t("detail.readyReplicas", { ready: dep.ready_replicas, desired: dep.replicas })}</Typography.Text>}<ReplicasEditor key={`${dep.id}-${dep.replicas}`} dep={dep} onChanged={load} /></Space></Descriptions.Item>
      {dep.replicas > 1 && <Descriptions.Item label={<Space size={4}>{t("detail.routingPolicy")}<RoutingPolicyHelp policies={routingPolicies} /></Space>}><Select size="small" loading={routingBusy || routingPolicies.length === 0} value={dep.routing_policy} onChange={changeRouting} options={routingOptions} /></Descriptions.Item>}
      {dep.template_id && <Descriptions.Item label={t("detail.inferenceTemplate")}><Tag color={dep.template_deviated ? "orange" : "green"}>{dep.template_deviated ? t("detail.deviated") : t("detail.notDeviated")}</Tag>{dep.template_scenario && <Typography.Text type="secondary"> {dep.template_scenario}</Typography.Text>}</Descriptions.Item>}
          </Descriptions>

          {dep.pd && <DeploymentPdPanel dep={dep} onChanged={load} />}
          <ReplicaRetryAlert dep={dep} onChanged={load} />
          <PlacementStatusAlert dep={dep} />

          <SleepControls dep={dep} onDone={load} />

          <DeploymentAutomation dep={dep} onChanged={load} />
          <DeploymentNodeReplacement dep={dep} onChanged={load} />

          <DeploymentPreparation rows={dep.model_progress} onChanged={load} />

          <Typography.Title level={5} style={{ marginTop: 20 }}>
            {t("detail.metrics")}
          </Typography.Title>
          <DeploymentMetricsPanel instances={dep.instances ?? []} deploymentId={dep.id} />

          <DeploymentInstances dep={dep} focus={focusInstance} onChanged={load} />
        </>
      )}
    </Drawer>
  );
}

// A multi-node deployment runs on every rank's node; name them all, in rank
// order, instead of only the leader the row is keyed by.
export function deploymentNodesLabel(d: Pick<Deployment, "node_name" | "node_names" | "runtime_node_names">, leaderUUID?: string): string {
  if ((d.runtime_node_names?.length ?? 0) > 0) return d.runtime_node_names!.join(", ");
  if ((d.node_names?.length ?? 0) > 1) return d.node_names!.join(", ");
  return d.node_name || (leaderUUID ? leaderUUID.slice(0, 8) : "—");
}

// Each node the deployment runs on, linked to its node detail; a multi-node
// deployment lists its ranks in order, as deploymentNodesLabel does.
function DeploymentNodeLinks({ dep }: { dep: Pick<Deployment, "node_uuid" | "node_name" | "node_uuids" | "node_names"> }) {
  const uuids = dep.node_uuids?.length ? dep.node_uuids : dep.node_uuid ? [dep.node_uuid] : [];
  if (uuids.length === 0) return <>—</>;
  const names = uuids.length > 1 ? dep.node_names ?? [] : [dep.node_name ?? dep.node_names?.[0]];
  return (
    <>
      {uuids.map((uuid, i) => <span key={`${i}-${uuid}`}>{i > 0 && ", "}<Link to="/nodes" state={{ nodeUUID: uuid }}>{names[i] || uuid.slice(0, 8)}</Link></span>)}
    </>
  );
}

// The copied request names the gateway's key header: every /v1 route needs
// an API key, and a curl without one reads as if none were required.
export function deploymentCurl(origin: string, dep: Pick<DeploymentListItem, "served_name" | "model_name">): string {
  return `curl '${origin}/v1/chat/completions' -H 'Content-Type: application/json' -H "Authorization: Bearer <key>" -d '{"model":"${dep.served_name || dep.model_name}","messages":[{"role":"user","content":"hello"}]}'`;
}
