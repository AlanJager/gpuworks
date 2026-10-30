import { Tag } from "antd";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import type { TimelineDeployment } from "../../schedulingApi";
import ResourceTable, { type ResourceColumn } from "../../components/resourceTable/ResourceTable";
import { durationText } from "../durationText";

/** waitText words a wait in milliseconds; none is a dash. */
export function waitText(t: TFunction<"scheduling">, ms?: number): string {
  if (ms == null) return "—";
  if (ms < 1000) return t("timeline.wait.ms", { value: Math.round(ms) });
  if (ms < 120_000) return t("timeline.wait.seconds", { value: (ms / 1000).toFixed(ms < 10_000 ? 1 : 0) });
  return t("timeline.wait.minutes", { value: Math.round(ms / 60_000) });
}

/**
 * SharingTable is the deployments that share cards over the window: how long
 * they were awake, how often they changed places with others and how long the
 * requests held for their wake waited.
 */
export function SharingTable({ rows }: { rows: TimelineDeployment[] }) {
  const { t } = useTranslation("scheduling");
  const { t: tDeploy } = useTranslation("deployments");
  const num = (key: keyof TimelineDeployment) => (_: unknown, row: TimelineDeployment) => <span className="scheduling-num">{String(row[key])}</span>;
  const columns: ResourceColumn<TimelineDeployment>[] = [
    { key: "deployment", title: t("timeline.columns.deployment"), width: 200, render: (_: unknown, row) => <Link to={`/deployments?deployment=${row.deployment_id}`}>{row.deployment}</Link> },
    { key: "level", title: t("timeline.columns.level"), width: 90, render: (_: unknown, row) => <Tag>{tDeploy(`yieldPolicy.policy.name.${row.yield_policy}`)}</Tag> },
    {
      key: "awake",
      title: t("timeline.columns.awake"),
      width: 170,
      render: (_: unknown, row) => <span className="scheduling-num">{t("timeline.awakeShare", { span: durationText(t, row.awake_seconds, "elapsed"), share: Math.round(row.awake_share * 1000) / 10 })}</span>,
    },
    { key: "switches", title: t("timeline.columns.switches"), width: 90, render: num("switches") },
    { key: "yielded", title: t("timeline.columns.yielded"), width: 90, render: num("yielded") },
    { key: "yielded_to", title: t("timeline.columns.yieldedTo"), width: 90, render: num("yielded_to") },
    { key: "held", title: t("timeline.columns.held"), width: 110, render: num("held_requests") },
    { key: "p50", title: t("timeline.columns.waitP50"), width: 100, render: (_: unknown, row) => <span className="scheduling-num">{waitText(t, row.wait_p50_ms)}</span> },
    { key: "p95", title: t("timeline.columns.waitP95"), width: 100, render: (_: unknown, row) => <span className="scheduling-num">{waitText(t, row.wait_p95_ms)}</span> },
  ];
  return (
    <ResourceTable<TimelineDeployment>
      tableId="scheduling-sharing"
      columns={columns}
      data={rows}
      rowKey={(row) => String(row.deployment_id)}
      pagination={false}
      columnSettings={false}
      emptyDescription={t("timeline.sharingEmpty")}
    />
  );
}
