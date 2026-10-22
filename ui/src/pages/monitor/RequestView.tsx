import { useCallback, useEffect, useMemo, useState } from "react";
import { Alert, Button, Select, Space, Switch, Typography } from "antd";
import { useTranslation } from "react-i18next";
import { api, usePolling, type MonitorRange, type MonitorReplica } from "../../api";
import type { MonitorRequest, MonitorRequests } from "../../monitorRequestsApi";
import { colors } from "../../theme/tokens";
import RequestDetailDrawer from "./RequestDetailDrawer";
import RequestHistory from "./RequestHistory";
import RequestRow, { LEGEND_ORDER, SEGMENT_STYLE, segmentLabel } from "./RequestRow";

// Rows shown before "show all", and the most "show all" lists.
const DEFAULT_ROWS = 20;
const MAX_ROWS = 200;
const LIVE_POLL_MS = 2000;

export type RequestViewKind = "live" | "history";

// ReplicaLabels names the deployment's replicas by instance uuid: their
// position in the replica list and the node they run on.
export type ReplicaLabels = Map<string, { n: number; node: string }>;

export function replicaLabels(replicas: MonitorReplica[] | undefined): ReplicaLabels {
  const out: ReplicaLabels = new Map();
  (replicas ?? []).forEach((r, i) => out.set(r.instance_uuid, { n: i + 1, node: r.node_name }));
  return out;
}

// RequestView lists a deployment's requests one per row: those in flight now
// (refreshed every two seconds, pausable) or those finished, a page at a
// time with filters (RequestHistory); the page's tabs pick which. Both
// narrow to one API key and one replica, and each row says which replica
// served it.
export default function RequestView({
  view,
  deploymentId,
  range,
  replicas: replicaList,
  pd = false,
}: {
  view: RequestViewKind;
  deploymentId: number;
  range: MonitorRange;
  replicas?: MonitorReplica[];
  // pd says the deployment is PD disaggregated (the legend names its stage).
  pd?: boolean;
}) {
  const { t } = useTranslation("monitoring");
  const [key, setKey] = useState<number>(0);
  const [instance, setInstance] = useState<string>("");
  const replicas = useMemo(() => replicaLabels(replicaList), [replicaList]);
  const replicaOptions = useMemo(
    () => [
      { value: "", label: t("requests.allReplicas") },
      ...[...replicas.entries()].map(([uuid, r]) => ({ value: uuid, label: t("requests.replica", { n: r.n, node: r.node }) })),
    ],
    [replicas, t],
  );
  // Keys seen in any answer, so the filter can offer them by name.
  const [keys, setKeys] = useState<Map<number, string>>(new Map());
  const learnKeys = useCallback((requests: MonitorRequest[]) => {
    setKeys((prev) => {
      let next: Map<number, string> | null = null;
      for (const r of requests) {
        if (!r.api_key_id || prev.get(r.api_key_id) === r.api_key_name) continue;
        next ??= new Map(prev);
        next.set(r.api_key_id, r.api_key_name);
      }
      return next ?? prev;
    });
  }, []);

  const keyOptions = useMemo(
    () => [
      { value: 0, label: t("requests.allKeys") },
      ...[...keys.entries()].sort((a, b) => a[1].localeCompare(b[1])).map(([id, name]) => ({ value: id, label: name || `#${id}` })),
    ],
    [keys, t],
  );

  return (
    <div>
      <div style={{ display: "flex", justifyContent: "flex-end", marginBottom: 8 }}>
        <Space size={6}>
          {replicas.size > 1 && (
            <Select
              size="small"
              aria-label={t("requests.replicaFilter")}
              value={instance}
              onChange={setInstance}
              options={replicaOptions}
              style={{ minWidth: 120 }}
              popupMatchSelectWidth={false}
            />
          )}
          <Select
            size="small"
            aria-label={t("requests.keyFilter")}
            value={key}
            onChange={setKey}
            options={keyOptions}
            style={{ minWidth: 140 }}
            popupMatchSelectWidth={false}
          />
        </Space>
      </div>
      {view === "live" ? (
        <LiveRequests deploymentId={deploymentId} apiKey={key} instance={instance} replicas={replicas} onRows={learnKeys} />
      ) : (
        <RequestHistory deploymentId={deploymentId} range={range} apiKey={key} instance={instance} replicas={replicas} onRows={learnKeys} />
      )}
      <Legend pd={pd} />
    </div>
  );
}

function LiveRequests({
  deploymentId,
  apiKey,
  instance,
  replicas,
  onRows,
}: {
  deploymentId: number;
  apiKey: number;
  instance: string;
  replicas: ReplicaLabels;
  onRows: (r: MonitorRequest[]) => void;
}) {
  const { t } = useTranslation("monitoring");
  const { t: tCommon } = useTranslation("common");
  const [paused, setPaused] = useState(false);
  const [all, setAll] = useState(false);
  const limit = all ? MAX_ROWS : DEFAULT_ROWS;
  const fetcher = useCallback(
    () => api.monitorRequests(deploymentId, { view: "live", limit, key: apiKey, instance }),
    [deploymentId, limit, apiKey, instance],
  );
  const { data, error, loading, refetch } = usePolling<MonitorRequests>(fetcher, paused ? 0 : LIVE_POLL_MS, [deploymentId, limit, apiKey, instance, paused]);
  useEffect(() => {
    if (data) onRows(data.requests);
  }, [data, onRows]);

  const counts = data?.counts;
  const inFlight = counts ? counts.waiting + counts.thinking + counts.answering : 0;
  return (
    <div>
      <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center", gap: 8, flexWrap: "wrap", marginBottom: 8 }}>
        <Space size={14} wrap aria-label={t("requests.counts")}>
          <Count color={SEGMENT_STYLE.waiting} label={t("requests.phase.waiting")} n={counts?.waiting} />
          <Count color={SEGMENT_STYLE.thinking} label={t("requests.phase.thinking")} n={counts?.thinking} />
          <Count color={SEGMENT_STYLE.answering} label={t("requests.phase.answering")} n={counts?.answering} />
        </Space>
        <Space size={8}>
          <Switch size="small" checked={!paused} onChange={(on) => setPaused(!on)} aria-label={t("requests.autoRefresh")} />
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>
            {paused ? t("requests.paused") : t("requests.refreshing")}
          </Typography.Text>
        </Space>
      </div>
      {error && (
        <Alert
          type="error"
          showIcon
          message={t("requests.loadFailed")}
          description={error.message}
          action={<Button size="small" onClick={() => refetch()}>{tCommon("actions.retry")}</Button>}
          style={{ marginBottom: 8 }}
        />
      )}
      {loading && !data && <Placeholder text={t("requests.loading")} />}
      {data && data.requests.length === 0 && <Placeholder text={apiKey || instance ? t("requests.liveEmptyFiltered") : t("requests.liveEmpty")} />}
      {data && data.requests.length > 0 && (
        <>
          <RequestList requests={data.requests} label={t("requests.live")} replicas={replicas} />
          {inFlight > data.requests.length && !all && (
            <Button size="small" type="link" onClick={() => setAll(true)}>
              {t("requests.showAll", { count: Math.min(inFlight, MAX_ROWS) })}
            </Button>
          )}
          {all && (
            <Button size="small" type="link" onClick={() => setAll(false)}>
              {t("requests.showFewer", { count: DEFAULT_ROWS })}
            </Button>
          )}
        </>
      )}
    </div>
  );
}

// RequestList is one request per row; a row opens the request's details.
// deploymentNames, when given, names each request's deployment in the
// details (a list across deployments).
export function RequestList({
  requests,
  label,
  replicas,
  deploymentNames,
}: {
  requests: MonitorRequest[];
  label: string;
  replicas: ReplicaLabels;
  deploymentNames?: Map<number, string>;
}) {
  const [open, setOpen] = useState<MonitorRequest | null>(null);
  return (
    <>
      <div role="list" aria-label={label} style={{ display: "flex", flexDirection: "column", gap: 2 }}>
        {requests.map((r) => (
          <RequestRow
            key={r.id}
            request={r}
            replica={replicas.get(r.instance_uuid)}
            place={deploymentNames ? deploymentNames.get(r.deployment_id) || `#${r.deployment_id}` : undefined}
            onOpen={setOpen}
          />
        ))}
      </div>
      <RequestDetailDrawer
        request={open}
        replica={open ? replicas.get(open.instance_uuid) : undefined}
        deploymentName={open && deploymentNames ? (deploymentNames.get(open.deployment_id) ?? "") : undefined}
        onClose={() => setOpen(null)}
      />
    </>
  );
}

export function Placeholder({ text }: { text: string }) {
  return (
    <Typography.Text type="secondary" style={{ display: "flex", minHeight: 96, alignItems: "center", justifyContent: "center", textAlign: "center" }}>
      {text}
    </Typography.Text>
  );
}

function Count({ color, label, n }: { color: string; label: string; n: number | undefined }) {
  return (
    <span style={{ display: "inline-flex", alignItems: "center", gap: 6, fontSize: 12 }}>
      <span style={{ width: 10, height: 10, borderRadius: 2, background: color }} />
      <span style={{ color: colors.muted }}>{label}</span>
      <strong style={{ fontVariantNumeric: "tabular-nums", color: colors.ink }}>{n ?? "—"}</strong>
    </span>
  );
}

// Legend names the bar's colours; a PD deployment's legend has its prefill
// stage and names the decode instance's queue and prefill as PD has them.
function Legend({ pd }: { pd: boolean }) {
  const { t } = useTranslation("monitoring");
  return (
    <div style={{ display: "flex", gap: 14, flexWrap: "wrap", marginTop: 10 }} aria-label={t("requests.legend")}>
      {LEGEND_ORDER.filter((kind) => pd || kind !== "pd_prefill").map((kind) => (
        <span key={kind} style={{ display: "inline-flex", alignItems: "center", gap: 5, fontSize: 12, color: colors.muted }}>
          <span style={{ width: 10, height: 10, borderRadius: 2, background: SEGMENT_STYLE[kind], display: "inline-block" }} />
          {segmentLabel(kind, pd, t)}
        </span>
      ))}
    </div>
  );
}
