import type { TFunction } from "i18next";
import { defaultSeriesName } from "../../components/TimeSeriesChart";
// Instance uuids are 32 hex chars; the first 8 identify a replica the same
// way the replica table's link and the log stream's chips do.
export const INSTANCE_SHORT_LEN = 8;

// serviceSeriesName names one series of a service-page chart for its legend
// and hover tooltip: the localized metric label (metrics.<labels.metric>,
// e.g. "Running", "Prompt tok/s", "TTFT p95"), followed by " · <short
// instance id>" when the series is drawn per replica (concurrency/kv/
// throughput carry labels.instance; latency/stage_time/preemptions do not).
// Without this, TimeSeriesChart's default name would collapse running and
// waiting (same instance label) into indistinguishable legend entries.
export function serviceSeriesName(t: TFunction, labels: Record<string, string>, index: number): string {
  const metric = labels.metric;
  const label = metric ? t(`metrics.${metric}`, { defaultValue: metric }) : t("metrics.series", { index: index + 1 });
  const instance = labels.instance;
  return instance ? `${label} · ${instance.slice(0, INSTANCE_SHORT_LEN)}` : label;
}

// explorerSeriesName names one series of the metric explorer: the chart's
// default name, with a node or deployment the server marked deleted
// (node_deleted / deployment_deleted) named "<name> (deleted)", since its
// samples outlive it.
export function explorerSeriesName(t: TFunction, labels: Record<string, string>, index: number): string {
  let named = labels;
  if (labels.node_deleted === "true" && labels.node_name) named = { ...named, node_name: t("requests.deletedName", { name: labels.node_name }) };
  if (labels.deployment_deleted === "true" && labels.deployment_name) named = { ...named, deployment_name: t("requests.deletedName", { name: labels.deployment_name }) };
  return defaultSeriesName(named, index);
}
