import type { TFunction } from "i18next";
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
