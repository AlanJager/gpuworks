import type { ReactNode } from "react";
import { Badge, Tag } from "antd";
import { useTranslation } from "react-i18next";
import { Link, useNavigate } from "react-router-dom";
import type { MonitorReplica, MonitorReplicaGPU } from "../../api";
import ResourceTable, { type ResourceColumn } from "../../components/resourceTable/ResourceTable";
import Dash, { renderNumber } from "./Dash";

const STATE_BADGE: Record<string, "success" | "processing" | "default" | "error" | "warning"> = {
  running: "success",
  starting: "processing",
  stopped: "default",
  removing: "warning",
  failed: "error",
  asleep: "warning",
};

// gpuMetric renders one per-GPU metric as "GPU 0 · 91%" - a single line for
// a one-GPU replica, one stacked line per GPU otherwise - never the terse
// "0:91%" that reads as a ratio. A GPU whose value is not collected keeps
// its own dash so the other GPUs' values still line up.
function gpuMetric(gpus: MonitorReplicaGPU[] | null | undefined, pick: (gpu: MonitorReplicaGPU) => number | null, digits: number, suffix: string): ReactNode {
  if (!gpus || gpus.length === 0) return <Dash />;
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 2, whiteSpace: "nowrap" }}>
      {gpus.map((gpu) => {
        const value = pick(gpu);
        return (
          <span key={gpu.index}>
            GPU {gpu.index} · {value == null ? <Dash /> : `${value.toFixed(digits)}${suffix}`}
          </span>
        );
      })}
    </div>
  );
}

// ReplicaTable is the service page's per-instance breakdown (plan §3.2).
// There is no dedicated instance-detail route today (instances render inline
// inside DeploymentsPage's drawer, keyed by ?deployment=<id>); a row click
// opens that same drawer and additionally carries the instance uuid as a
// query param for a future deep link, per the task's fallback instruction.
export default function ReplicaTable({
  replicas,
  deploymentId,
  loading,
}: {
  replicas: MonitorReplica[];
  deploymentId: number;
  loading?: boolean;
}) {
  const { t } = useTranslation("monitoring");
  const { t: tCommon } = useTranslation("common");
  const navigate = useNavigate();

  const pct = (value: number | null) => (value == null ? <Dash /> : `${value.toFixed(0)}%`);
  const rate = (value: number | null) => (value == null ? <Dash /> : value.toFixed(0));
  const columns: ResourceColumn<MonitorReplica>[] = [
    {
      key: "instance_uuid",
      role: "name",
      title: t("deployment.replicaTable.instance"),
      width: 150,
      sortValue: (row) => row.instance_uuid,
      render: (_: unknown, row: MonitorReplica) => (
        <Link
          to={`/deployments?deployment=${deploymentId}&instance=${encodeURIComponent(row.instance_uuid)}`}
          onClick={(event) => event.stopPropagation()}
        >
          {row.instance_uuid.slice(0, 12)}
        </Link>
      ),
    },
    {
      key: "state",
      title: t("deployment.replicaTable.state"),
      width: 110,
      sortValue: (row) => row.state,
      render: (_: unknown, row: MonitorReplica) => <Badge status={STATE_BADGE[row.state] ?? "default"} text={t(`states.${row.state}`, { defaultValue: row.state })} />,
    },
    {
      key: "role",
      title: t("deployment.replicaTable.role"),
      width: 100,
      sortValue: (row) => row.role,
      // Set only for a PD deployment's replica (ADR 0028); an ordinary
      // deployment's rows carry no role and this cell stays a dash. Reuses
      // the top-level "stage" namespace's own prefill/decode labels rather
      // than duplicating a third copy of them.
      render: (_: unknown, row: MonitorReplica) => (row.role ? <Tag color="geekblue">{t(`stage.${row.role}`, { defaultValue: row.role })}</Tag> : <Dash />),
    },
    {
      key: "node",
      title: t("deployment.replicaTable.node"),
      width: 180,
      sortValue: (row) => row.node_name,
      render: (_: unknown, row: MonitorReplica) => `${row.node_name} · GPU ${(row.gpu_indexes ?? []).join(",") || "—"}`,
    },
    {
      key: "concurrency",
      title: t("deployment.replicaTable.concurrency"),
      width: 110,
      sortValue: (row) => row.running,
      render: (_: unknown, row: MonitorReplica) => (
        <>
          {renderNumber(row.running)} / {renderNumber(row.waiting)}
        </>
      ),
    },
    { key: "kv_usage_pct", title: t("deployment.replicaTable.kvUsage"), width: 110, sortValue: (row) => row.kv_usage_pct, render: (_: unknown, row: MonitorReplica) => pct(row.kv_usage_pct) },
    { key: "kv_hit_rate_pct", title: t("deployment.replicaTable.kvHitRate"), width: 110, sortValue: (row) => row.kv_hit_rate_pct, render: (_: unknown, row: MonitorReplica) => pct(row.kv_hit_rate_pct) },
    { key: "prompt_tok_s", title: t("deployment.replicaTable.prefillTokS"), width: 110, sortValue: (row) => row.prompt_tok_s, render: (_: unknown, row: MonitorReplica) => rate(row.prompt_tok_s) },
    { key: "generation_tok_s", title: t("deployment.replicaTable.decodeTokS"), width: 110, sortValue: (row) => row.generation_tok_s, render: (_: unknown, row: MonitorReplica) => rate(row.generation_tok_s) },
    { key: "util", title: t("deployment.replicaTable.util"), width: 100, render: (_: unknown, row: MonitorReplica) => gpuMetric(row.gpus, (g) => g.util_pct, 0, "%") },
    { key: "memBw", title: t("deployment.replicaTable.memBw"), width: 100, render: (_: unknown, row: MonitorReplica) => gpuMetric(row.gpus, (g) => g.mem_bw_pct, 0, "%") },
    { key: "temperature", title: t("deployment.replicaTable.temperature"), width: 100, render: (_: unknown, row: MonitorReplica) => gpuMetric(row.gpus, (g) => g.temperature_c, 0, "°C") },
    { key: "power", title: t("deployment.replicaTable.power"), width: 100, render: (_: unknown, row: MonitorReplica) => gpuMetric(row.gpus, (g) => g.power_w, 0, "W") },
  ];

  return (
    <ResourceTable<MonitorReplica>
      tableId="monitor-replicas"
      rowKey="instance_uuid"
      columns={columns}
      data={replicas}
      loading={loading}
      emptyDescription={tCommon("dataTable.empty")}
      // Mouse-only row affordance (same pattern as GPUPage/PointResultsTable):
      // keyboard/AT users reach the destination through the real <Link> in
      // the instance cell instead, so the <tr> keeps its plain row semantics.
      onRow={(row) => ({
        onClick: () => navigate(`/deployments?deployment=${deploymentId}&instance=${encodeURIComponent(row.instance_uuid)}`),
        className: "gpu-clickable-row",
        title: t("deployment.replicaTable.openInDeployments"),
      })}
    />
  );
}
