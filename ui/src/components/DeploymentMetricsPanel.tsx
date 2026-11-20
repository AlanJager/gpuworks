import { Alert } from "../pages/DeploymentControls";
import { Select, SelectContent, SelectTrigger, SelectItem } from "./ui/select";
import { useMemo, type ComponentProps } from "react";
import { api, usePolling, type Instance, type MetricPoint, type MetricSeries, type MonitorDeploymentSignals } from "../api";
import TimeSeriesChart, { autoValueFmt } from "./TimeSeriesChart";
import EmptyState from "./EmptyState";
import EngineMetricsNotice from "./EngineMetricsNotice";
import { RangeHeader, useMetricRange, useMetricWindow } from "./MetricWindow";
import { MetricStat } from "./InferenceMetricsPanel";
import { resolveRange, stepString, type TimeRangeSelection } from "../timeRange";
import { useTranslation } from "react-i18next";

const LIVE_STATES = new Set(["running", "starting", "optimizing"]);

// One stable string per selection so polling restarts only when the window
// really changes; a custom range never polls (pollMs 0 in resolveRange).
function selectionKey(selection: TimeRangeSelection): string {
  return selection.kind === "quick" ? selection.key : `custom-${selection.startMS}-${selection.endMS}`;
}

// The panel draws what the server resolved for the deployment (the monitor
// center's signal sources, ADR 0039): per signal, the engine's own series or,
// where the engine reports none, the gateway's. It never decides a source.
export default function DeploymentMetricsPanel({ instances, deploymentId, onSelectInstance }: { instances: Instance[]; deploymentId: number; onSelectInstance?: (uuid: string) => void }) {
  const { t } = useTranslation("deployments");
  const [selection, setSelection] = useMetricWindow();
  const liveInstances = useMemo(
    () => instances.filter((inst) => LIVE_STATES.has(inst.state)),
    [instances],
  );
  const eff = useMetricRange(selection);
  const queryKey = `${deploymentId}/${selectionKey(selection)}`;
  const metrics = usePolling(async () => {
    const { startMS, endMS, stepMS } = resolveRange(selection, Date.now());
    return { queryKey, signals: await api.monitorDeploymentSignals(deploymentId, { start: startMS, end: endMS, step: stepString(stepMS) }) };
  }, eff.pollMs, [queryKey]);
  const signals: MonitorDeploymentSignals | null = metrics.data?.queryKey === queryKey ? metrics.data.signals : null;
  const error = metrics.error;

  const rangeHeader = (
    <RangeHeader selection={selection} onChange={setSelection} beyondRetention={eff.beyondRetention}
      retentionHours={eff.retentionHours} retentionError={eff.retentionError} />
  );

  const running = byMetric(signals?.concurrency, "running");
  const waiting = byMetric(signals?.concurrency, "waiting");
  const cache = byMetric(signals?.kv, "usage_pct");
  const promptRate = byMetric(signals?.throughput, "prompt_tok_s");
  const generationRate = byMetric(signals?.throughput, "generation_tok_s");
  // A line with an instance counts while that instance is live; a line
  // without one is the gateway's, the whole deployment's. A deployment with
  // nothing live has no "now".
  const live = (series: MetricSeries[]) =>
    liveInstances.length === 0 ? [] : series.filter((item) => !item.labels.instance || liveInstances.some((inst) => inst.uuid === item.labels.instance));
  const runningNow = readingsSum(live(running));
  const waitingNow = readingsSum(live(waiting));
  const cachePeak = readingsMax(live(cache));
  const liveGeneration = readingsSum(live(generationRate));
  const promptNow = latestSum(promptRate);
  const generationNow = latestSum(generationRate);

  const concurrencySeries = [
    aggregateSeries(running, { metric: t("metrics.totalRunningRequests") }),
    aggregateSeries(waiting, { metric: t("metrics.totalWaitingRequests") }),
  ];
  const tokenSeries = [
    aggregateSeries(promptRate, { metric: "Prompt tok/s" }),
    aggregateSeries(generationRate, { metric: "Generation tok/s" }),
  ];

  const sampled = new Map<string, string>();
  for (const series of [...running, ...waiting, ...cache, ...promptRate, ...generationRate]) {
    const uuid = series.labels.instance;
    if (!uuid || !series.points.length) continue;
    const role = instances.find((inst) => inst.uuid === uuid)?.role || series.labels.role;
    sampled.set(uuid, [role && t(`detail.pd.role.${role}`, { defaultValue: role }), uuid.slice(0, 12)].filter(Boolean).join(" · "));
  }
  const historical = selection.kind === "custom" || liveInstances.length === 0;
  const empty = !hasPoints(concurrencySeries) && !hasPoints(tokenSeries) && !hasPoints(cache);
  // Only the server's sources tell which signals the engine left empty.
  const sources = Object.values(signals?.sources ?? {});
  const gatewayOnly = sources.length > 0 && sources.every((source) => source === "gateway");
  const noEngineNoTraffic = !!signals && empty && sources.length === 0 && !historical && eff.beyondRetention === false
    && liveInstances.every((inst) => inst.state === "running");
  return (
    <div className="deployment-metric-panel">
      {error && !signals ? (
        <Alert type="error" showIcon message={t("metrics.loadFailed")} description={error.message} style={{ marginBottom: 12 }} />
      ) : noEngineNoTraffic ? (
        <EngineMetricsNotice deploymentId={deploymentId} kind="noTraffic" />
      ) : empty && (
        <Alert type="info" role="status" showIcon
          message={historical && signals ? t("metrics.emptyWindow") : t("metrics.waiting")}
          description={historical && signals ? undefined : t("metrics.waitingDescription")} style={{ marginBottom: 12 }} />
      )}
      {gatewayOnly && <EngineMetricsNotice deploymentId={deploymentId} kind="gatewayOnly" />}
      <div className="deployment-metrics-summary" role="region" aria-label={t("detail.metrics")} tabIndex={0}>
        <MetricStat title={t("metrics.runningInstances")} value={liveInstances.length} />
        <MetricStat title={t("metrics.runningRequests")} value={runningNow} />
        <MetricStat title={t("metrics.waitingRequests")} value={waitingNow} />
        <MetricStat title={t("metrics.peakCache")} value={cachePeak} suffix="%" />
        <MetricStat title={t("metrics.generationThroughput")} value={liveGeneration} suffix="tok/s" precision={1} />
      </div>
      {rangeHeader}

      <div className="deployment-metric-charts">
        <ChartCard title={t("metrics.concurrency")} description={t("metrics.concurrencyDescription")}>
          <MetricPlot series={concurrencySeries} height={190} valueFmt={(v) => v.toFixed(0)} integer />
        </ChartCard>
        <ChartCard title={t("metrics.tokenThroughput")} description={t("metrics.tokenDescription", {
          prompt: promptNow?.toFixed(1) ?? "—", generation: generationNow?.toFixed(1) ?? "—",
        })}>
          <MetricPlot series={tokenSeries} height={190} unit="tok/s" valueFmt={autoValueFmt} />
        </ChartCard>
        <ChartCard title={t("metrics.instanceCache")} description={t("metrics.instanceCacheDescription")}
          action={onSelectInstance && sampled.size > 0 && (
            <Select size="sm" value="" onValueChange={onSelectInstance}>
              <SelectTrigger aria-label={t("metrics.inspectInstance")} placeholder={t("metrics.inspectInstance")} contentSized />
              <SelectContent>{Array.from(sampled, ([uuid,label])=><SelectItem key={uuid} value={uuid}>{label}</SelectItem>)}</SelectContent>
            </Select>
          )}>
          <MetricPlot series={cache} height={210} unit="%" valueFmt={v => v.toFixed(0)}
            onSeriesClick={onSelectInstance && (labels => onSelectInstance(labels.instance))} />
        </ChartCard>
      </div>
    </div>
  );
}

function byMetric(series: MetricSeries[] | undefined, metric: string): MetricSeries[] {
  return (series ?? []).filter((item) => item.labels.metric === metric).map((item) => ({ ...item, points: item.points ?? [] }));
}

function ChartCard({ title, description, action, children }: { title: string; description: string; action?: React.ReactNode; children: React.ReactNode }) {
  return (
    <div className="deployment-trend-card">
      <div className="flex flex-wrap items-center justify-between gap-2"><span className="font-semibold">{title}</span>{action}</div>
      <span className="text-fg-muted block text-label mb-2">
        {description}
      </span>
      {children}
    </div>
  );
}

function MetricPlot(chart: ComponentProps<typeof TimeSeriesChart>) {
  const { t } = useTranslation("common");
  return hasPoints(chart.series) ? <TimeSeriesChart {...chart} /> : (
    <div style={{ height: chart.height }}><EmptyState compact description={t("dataTable.empty")} /></div>
  );
}

function hasPoints(series?: MetricSeries[] | null): boolean {
  return !!series?.some((item) => item.points?.length > 0);
}

// Each line's own newest reading, summed or maxed: an instance whose last
// sample is a scrape older than another's still counts.
function readings(series: MetricSeries[]): number[] {
  return series.flatMap((item) => item.points?.slice(-1).map(([, value]) => value) ?? []);
}

function readingsSum(series: MetricSeries[]): number | undefined {
  const values = readings(series);
  return values.length ? values.reduce((sum, value) => sum + value, 0) : undefined;
}

function readingsMax(series: MetricSeries[]): number | undefined {
  const values = readings(series);
  return values.length ? Math.max(...values) : undefined;
}

// The readings that share the newest time, summed: the chart's "now".
function latestSum(series: MetricSeries[]): number | undefined {
  const values = latestValues(series);
  return values.length ? values.reduce((sum, value) => sum + value, 0) : undefined;
}

function latestValues(series: MetricSeries[]): number[] {
  const latest = series.flatMap((item) => item.points?.slice(-1) ?? []);
  if (!latest.length) return [];
  const time = Math.max(...latest.map(([at]) => at));
  return latest.filter(([at]) => at === time).map(([, value]) => value);
}

function aggregateSeries(series: MetricSeries[], labels: Record<string, string>): MetricSeries {
  const pointsByTime = new Map<number, number>();
  for (const item of series) {
    for (const point of item.points ?? []) {
      pointsByTime.set(point[0], (pointsByTime.get(point[0]) ?? 0) + point[1]);
    }
  }
  return {
    labels,
    points: Array.from(pointsByTime.entries())
      .sort(([a], [b]) => a - b)
      .map(([time, value]): MetricPoint => [time, value]),
  };
}
