import { Typography } from "antd";
import { useCallback, useMemo } from "react";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import { api, usePolling, type MetricSeries, type MonitorSeries } from "../api";
import TimeSeriesChart, { defaultSeriesName } from "./TimeSeriesChart";
import { colors } from "../theme/tokens";

// Stage duration p95 and preemption rate come from the deployment monitor
// snapshot (GET /api/v1/monitor/deployments/{id}), which already computes
// windowed p95s and preemption rates server-side (internal/server/monitor).
// There is no separate gpuworks_infer_* metric for either number, and there
// must not be one invented here: a client-side name the agent/engine never
// emit would just be a second, unmaintained source of truth.
const RANGE = "1h";

export default function InstanceStagePanel({ instanceUuid, deploymentId }: { instanceUuid: string; deploymentId: number }) {
  const { t } = useTranslation("deployments");
  // Every open instance panel polls this endpoint independently by
  // deploymentId; the server's 10s snapshot cache (internal/server/monitor)
  // absorbs the duplicate requests from replicas of the same deployment, so
  // this does not multiply load with the number of panels open.
  const snapshot = usePolling(() => api.monitorDeployment(deploymentId, RANGE), 10000, [deploymentId]);

  const stageSeries = useMemo(
    () => filterByInstance(snapshot.data?.charts.stage_time, instanceUuid),
    [snapshot.data, instanceUuid],
  );
  const preemptSeries = useMemo(
    () => filterByInstance(snapshot.data?.charts.preemptions, instanceUuid),
    [snapshot.data, instanceUuid],
  );
  const seriesName = useCallback((labels: Record<string, string>, index: number) => stageSeriesName(labels, index, t), [t]);

  return (
    <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(320px, 1fr))", gap: 12 }}>
      <StageCard title={t("instanceStages.stageTitle")} description={t("instanceStages.stageDescription")}>
        {hasPoints(stageSeries) ? (
          <TimeSeriesChart height={190} series={stageSeries} seriesName={seriesName} unit="ms" valueFmt={(v) => v.toFixed(0)} />
        ) : (
          <NotCollected text={t("instanceStages.notCollected")} />
        )}
      </StageCard>
      <StageCard title={t("instanceStages.preemptTitle")} description={t("instanceStages.preemptDescription")}>
        {hasPoints(preemptSeries) ? (
          <TimeSeriesChart height={190} series={preemptSeries} seriesName={seriesName} unit="/s" valueFmt={(v) => v.toFixed(2)} />
        ) : (
          <NotCollected text={t("instanceStages.notCollected")} />
        )}
      </StageCard>
    </div>
  );
}

// stageSeriesName names the stage_time chart's three lines (labels.metric =
// queue_ms/prefill_ms/decode_ms, per internal/server/monitorapi's wire
// contract) through the product's own translated stage vocabulary. Matching
// "queue"/"prefill"/"decode" instead - names the wire never carried - left
// the legend reading the raw label ("metric queue_ms").
export function stageSeriesName(labels: Record<string, string>, index: number, t: TFunction<"deployments">): string {
  switch (labels.metric) {
    case "queue_ms": return t("instanceStages.queue");
    case "prefill_ms": return t("instanceStages.prefill");
    case "decode_ms": return t("instanceStages.decode");
    default: return defaultSeriesName(labels, index);
  }
}

// filterByInstance narrows a deployment-wide chart to this replica's own
// series when the server labels them by instance. A chart that carries no
// instance label at all (only ever aggregated at the deployment level) is
// shown unfiltered rather than hidden.
function filterByInstance(series: MonitorSeries[] | undefined, instanceUuid: string): MonitorSeries[] {
  const all = series ?? [];
  const labeled = all.filter((item) => item.labels?.instance != null);
  if (labeled.length === 0) return all;
  return labeled.filter((item) => item.labels.instance === instanceUuid);
}

function hasPoints(series?: MetricSeries[] | null): boolean {
  return !!series?.some((item) => item.points?.length > 0);
}

function NotCollected({ text }: { text: string }) {
  return (
    <div style={{ height: 190, display: "flex", alignItems: "center", justifyContent: "center" }}>
      <Typography.Text type="secondary">— {text}</Typography.Text>
    </div>
  );
}

function StageCard({ title, description, children }: { title: string; description: string; children: React.ReactNode }) {
  return (
    <div style={{ border: `1px solid ${colors.borderSoft}`, borderRadius: 4, padding: 16, background: colors.white }}>
      <Typography.Text strong>{title}</Typography.Text>
      <Typography.Text type="secondary" style={{ display: "block", fontSize: 12, marginBottom: 8 }}>{description}</Typography.Text>
      {children}
    </div>
  );
}
