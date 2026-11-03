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
import { api, useEventPayload, useEvents, usePolling, type ChangeEvent, type Deployment, type DeploymentListItem, type DeploymentRuntimeNode, type Instance, type RoutingPolicyOption } from "../api";
import { DeploymentPreparation, ListPreparationAction, PreparationLabel } from "./DeploymentPreparation";
import DeploymentMetricsPanel from "../components/DeploymentMetricsPanel";
import RoutingPolicyHelp from "../components/RoutingPolicyHelp";
import { effectiveDeploymentState, partlyAsleep, stateLabel } from "./state";
import DeploymentPdPanel from "./DeploymentPdPanel";
import DeploymentInstances from "./DeploymentInstances";
import SleepControls from "./DeploymentSleepControls";
import PlacementModeControl from "./DeploymentPlacementMode";
import DeploymentSchedulingPolicy from "./DeploymentSchedulingPolicy";
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
    {
      key: "served_name",
      title: t("page.column.servedName"),
      width: 220,
      // ResourceTable ellipsizes every column but actions, and that clips the
      // whole cell: with an inline copy button inside it, a long external
      // model name took the button off the edge with it, so the name could not
      // be copied at all (GWKS-145). The cell therefore clips nothing itself —
      // the name truncates on its own and the copy button is its sibling, so
      // the button keeps its space at any name length. Typography's ellipsis
      // does not engage alongside copyable, hence the two elements.
      ellipsis: false,
      render: (_: unknown, d: DeploymentListItem) => d.served_name
        ? (
          <span className="served-name-cell">
            <Typography.Text className="served-name-text" ellipsis={{ tooltip: d.served_name }}>{d.served_name}</Typography.Text>
            <Typography.Text className="served-name-copy" copyable={{ text: d.served_name }} />
          </span>
        )
        : "—",
    },
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
      // Where the replicas run now - not where the deployment was asked to run,
      // which has its own column beside it. A spread deployment folds into
      // "198.51.100.31 and 1 more" with every node in the tooltip and the drawer,
      // so no replica's node is hidden behind another's (GWKS-120).
      render: (_: unknown, d: DeploymentListItem) => <DeploymentRuntimeNodes dep={d} />,
    },
    {
      key: "targetNode",
      title: t("page.column.targetNode"),
      width: 160,
      defaultHidden: true,
      // The node the deployment was asked to run on. It stays available beside
      // the actual one rather than carrying both meanings in one field.
      render: (_: unknown, d: DeploymentListItem) => <DeploymentTargetNodes dep={d} />,
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
            <Descriptions.Item label={t("detail.node")}><DeploymentRuntimeNodeLinks dep={dep} /></Descriptions.Item>
            <Descriptions.Item label={t("detail.targetNode")}><DeploymentNodeLinks dep={dep} /></Descriptions.Item>
            {dep.placement_mode && <Descriptions.Item label={t("unitsPlacement.label")} span={2}><PlacementModeControl dep={dep} onChanged={() => void load()} /></Descriptions.Item>}
            <Descriptions.Item label={t("detail.state")}>
              {/* A partly slept deployment serves, so it stays running here too,
                  with the sleep count beside it rather than a plain badge. */}
              {b && <Space size={4}><Badge status={dep && partlyAsleep(dep) ? "processing" : b.status} text={b.text} />{dep && partlyAsleep(dep) && <Typography.Text type="secondary">{t("sleep.partial", { asleep: dep.asleep_replicas, total: dep.replicas })}</Typography.Text>}</Space>}
            </Descriptions.Item>
            <Descriptions.Item label={<Space size={0}>{t("detail.health")}<HealthCheckHelp /></Space>}><HealthCheckEditor key={`${dep.id}-${dep.health.threshold_sec}`} dep={dep} onChanged={load} /></Descriptions.Item>
            <Descriptions.Item label={<Space size={0}>{t("detail.tokenize")}<TokenizeHelp /></Space>}><TokenizeEditor dep={dep} onChanged={load} /></Descriptions.Item>
            <Descriptions.Item label={t("detail.sleepable")}>{dep.sleepable ? t("detail.yes") : t("detail.no")}</Descriptions.Item>
            {!dep.pd && (dep.nnodes ?? 1) <= 1 && <Descriptions.Item label={t("detail.placement.requirements")} span={2}><Space size={4} wrap><RequirementTags dep={dep} />{dep.placement_strategy && <Tag color="blue">{t(`detail.placement.strategy.${dep.placement_strategy}`, { defaultValue: dep.placement_strategy })}</Tag>}<EditRequirementsButton dep={dep} onChanged={() => void load()} /></Space></Descriptions.Item>}
              <Descriptions.Item label={t("detail.replicas")}><Space size={8}>{dep.ready_replicas != null && <Typography.Text type={dep.ready_replicas < dep.replicas ? "warning" : undefined}>{t("detail.readyReplicas", { ready: dep.ready_replicas, desired: dep.replicas })}</Typography.Text>}<ReplicasEditor key={`${dep.id}-${dep.replicas}`} dep={dep} onChanged={load} /></Space></Descriptions.Item>
      {dep.replicas > 1 && <Descriptions.Item label={<Space size={4}>{t("detail.routingPolicy")}<RoutingPolicyHelp policies={routingPolicies} /></Space>}><Select size="small" loading={routingBusy || routingPolicies.length === 0} value={dep.routing_policy} onChange={changeRouting} options={routingOptions} /></Descriptions.Item>}
      {dep.template_id && <Descriptions.Item label={t("detail.inferenceTemplate")}><Tag color={dep.template_deviated ? "orange" : "green"}>{dep.template_deviated ? t("detail.deviated") : t("detail.notDeviated")}</Tag>{dep.template_scenario && <Typography.Text type="secondary"> {dep.template_scenario}</Typography.Text>}</Descriptions.Item>}
          </Descriptions>

          {dep.pd && <DeploymentPdPanel dep={dep} onChanged={load} />}
          <ReplicaRetryAlert dep={dep} onChanged={load} />
          <PlacementStatusAlert dep={dep} />
          {/* The two node fields above are only readable if the drawer says
              outright that they differ, rather than leaving an operator to
              notice it and wonder which one is true (GWKS-120). */}
          {actualNodesDiffer(dep) && (
            <Alert
              type="info"
              showIcon
              role="status"
              style={{ marginBottom: 12 }}
              message={t("detail.scheduledElsewhere", {
                nodes: (dep.runtime_nodes ?? []).map((node) => node.node_name || node.node_uuid.slice(0, 8)).join(", "),
                target: deploymentTargetLabel(dep, dep.node_uuid),
              })}
            />
          )}

          <SleepControls dep={dep} onDone={load} />

          <DeploymentSchedulingPolicy dep={dep} onChanged={load} />

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

// runtimeNodesLabel names the nodes a deployment's replicas run on now. One
// node prints its name. Several fold to the first and how many nodes there are
// in all - 「198.51.100.31 等 2 台」, "198.51.100.31 and 1 more" - so the cell stays
// one line and still says the deployment is not on one machine. The full set is
// in the cell's tooltip and in the drawer, so a spread deployment can always be
// expanded; it is never printed as a single node.
//
// nodes comes from runtime_nodes, which the server derives from the instance
// rows. Nothing here falls back to the placement target: naming a machine the
// replicas are not on is exactly the bug (GWKS-120).
export function runtimeNodesLabel(
  nodes: DeploymentRuntimeNode[] | undefined,
  folded: (others: number, total: number) => string,
): string {
  if (!nodes?.length) return "—";
  const name = (node: DeploymentRuntimeNode) => node.node_name || node.node_uuid.slice(0, 8);
  const [first, ...rest] = nodes;
  return rest.length ? `${name(first)} ${folded(rest.length, nodes.length)}` : name(first);
}

// The target nodes a deployment was asked to run on: node_names rank by rank
// for a multi-node one, else its single named node. This is the placement
// target, and the drawer labels it as such rather than sharing 「计算节点」 with
// where the replicas run.
export function deploymentTargetLabel(d: Pick<Deployment, "node_name" | "node_names">, leaderUUID?: string): string {
  if ((d.node_names?.length ?? 0) > 1) return d.node_names!.join(", ");
  if ((d.node_names?.length ?? 0) === 1) return d.node_names![0];
  return d.node_name || (leaderUUID ? leaderUUID.slice(0, 8) : "—");
}

// DeploymentRuntimeNodes is the 「计算节点」 cell: the distinct nodes the
// instances are on now, folded to one line, with every node and its instance
// count in the tooltip and the whole list in the drawer.
//
// One node is printed as a link to its page. Several are printed as the folded
// text "198.51.100.31 等 2 台" ("198.51.100.31 and 1 more") inside the tooltip that
// spells out the rest, because a row of links per node would wrap the cell and
// bury the state beside it; the drawer lists one link per node.
function DeploymentRuntimeNodes({ dep }: { dep: Pick<Deployment, "runtime_nodes"> }) {
  const { t } = useTranslation("deployments");
  const nodes = dep.runtime_nodes ?? [];
  if (!nodes.length) {
    // No instance exists, so no node hosts a replica - true for a deployment
    // that was never placed and for one that has stopped. Saying so is truer
    // than printing the node that was asked for.
    return <Typography.Text type="secondary">{t("page.runtimeNodes.none")}</Typography.Text>;
  }
  const name = (node: DeploymentRuntimeNode) => node.node_name || node.node_uuid.slice(0, 8);
  if (nodes.length === 1) {
    return <Link to="/nodes" state={{ nodeUUID: nodes[0].node_uuid }}>{name(nodes[0])}</Link>;
  }
  const folded = runtimeNodesLabel(nodes, (others, total) => t("page.runtimeNodes.others", { count: others, total }));
  // The whole set in one string, so hover and a screen reader both learn every
  // node rather than only the first the cell has room for.
  const all = nodes.map((node) => `${name(node)} (${t("page.runtimeNodes.replicas", { count: node.instances })})`).join(", ");
  return (
    <Tooltip title={all}>
      <span aria-label={all}>{folded}</span>
    </Tooltip>
  );
}

// DeploymentTargetNodes is the optional 「目标节点」 cell: where the deployment
// was asked to run, which is node_name/node_names and can differ from the node
// its replicas actually run on.
function DeploymentTargetNodes({ dep }: { dep: Pick<Deployment, "node_uuid" | "node_name" | "node_names"> }) {
  return <>{deploymentTargetLabel(dep, dep.node_uuid)}</>;
}

// DeploymentRuntimeNodeLinks lists every node the deployment's replicas are on,
// one link each, so a folded cell can always be expanded to the whole set. A
// replica on a node that is not the placement target is what the drawer's
// 「已调度到其它节点」 notice points at.
export function DeploymentRuntimeNodeLinks({ dep }: { dep: Pick<Deployment, "runtime_nodes"> }) {
  const { t } = useTranslation("deployments");
  const nodes = dep.runtime_nodes ?? [];
  if (!nodes.length) return <Typography.Text type="secondary">{t("page.runtimeNodes.none")}</Typography.Text>;
  return (
    <Space size={4} wrap>
      {nodes.map((node) => (
        <Link key={node.node_uuid} to="/nodes" state={{ nodeUUID: node.node_uuid }}>
          {node.node_name || node.node_uuid.slice(0, 8)}
          {nodes.length > 1 && <Typography.Text type="secondary"> ×{node.instances}</Typography.Text>}
        </Link>
      ))}
    </Space>
  );
}

// actualNodesDiffer reports whether any replica runs on a node the deployment
// does not name as its target - the condition the drawer explains instead of
// leaving the two node fields to contradict each other.
export function actualNodesDiffer(dep: Pick<Deployment, "node_uuid" | "node_uuids" | "runtime_nodes">): boolean {
  const nodes = dep.runtime_nodes ?? [];
  if (!nodes.length) return false;
  const targets = new Set<string>(dep.node_uuids?.length ? dep.node_uuids : dep.node_uuid ? [dep.node_uuid] : []);
  if (!targets.size) return true;
  return nodes.some((node) => !targets.has(node.node_uuid));
}

// Each node the deployment runs on, linked to its node detail; a multi-node
// deployment lists its ranks in order.
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
