import { Alert, Card, Col, Row, Statistic, Typography } from "antd";
import { useMemo, useState, type ReactNode } from "react";
import { api, usePolling, type Instance, type MetricPoint, type MetricSeries } from "../api";
import TimeSeriesChart, { autoValueFmt } from "./TimeSeriesChart";
import GatewayMetricsNotice from "./GatewayMetricsNotice";
import TimeRangePicker from "./TimeRangePicker";
import { percentScaled } from "./InferenceMetricsPanel";
import { colors } from "../theme/tokens";
import { RETENTION_WARNING_DAYS, resolveRange, stepString, type TimeRangeSelection } from "../timeRange";
import { useTranslation } from "react-i18next";

const LIVE_STATES = new Set(["running", "starting", "optimizing"]);

const METRICS = {
  running: "gpuworks_infer_requests_running",
  waiting: "gpuworks_infer_requests_waiting",
  cache: "gpuworks_infer_gpu_cache_usage_percent",
  promptTokens: "gpuworks_infer_prompt_tokens_total",
  generationTokens: "gpuworks_infer_generation_tokens_total",
} as const;

type MetricKey = keyof typeof METRICS;
type MetricData = Record<MetricKey, MetricSeries[]>;

const EMPTY_DATA: MetricData = {
  running: [],
  waiting: [],
  cache: [],
  promptTokens: [],
  generationTokens: [],
};

// One stable string per selection so polling restarts only when the window
// really changes; a custom range never polls (pollMs 0 in resolveRange).
function selectionKey(selection: TimeRangeSelection): string {
  return selection.kind === "quick" ? selection.key : `custom-${selection.startMS}-${selection.endMS}`;
}

export default function DeploymentMetricsPanel({ instances, deploymentId }: { instances: Instance[]; deploymentId?: number }) {
  const { t } = useTranslation("deployments");
  const [selection, setSelection] = useState<TimeRangeSelection>({ kind: "quick", key: "15m" });
  const liveInstances = useMemo(
    () => instances.filter((inst) => LIVE_STATES.has(inst.state)),
    [instances],
  );
  const liveKey = useMemo(() => liveInstances.map((inst) => inst.uuid).sort().join(","), [liveInstances]);
  const eff = useMemo(() => resolveRange(selection, Date.now()), [selection]);
  const effKey = selectionKey(selection);
  const metrics = usePolling<MetricData>(
    () => loadMetricData(liveInstances, selection),
    eff.pollMs,
    // pollMs must be a dep or a widened window would keep the old cadence.
    [liveKey, effKey, eff.pollMs],
  );
  const data = metrics.data ?? EMPTY_DATA;

  if (instances.length === 0) {
    return <Alert type="info" showIcon message={t("metrics.noInstances")} description={t("metrics.noInstancesDescription")} />;
  }

  if (liveInstances.length === 0) {
    return (
      <Alert
        type="info"
        showIcon
        message={t("metrics.noRunning")}
        description={t("metrics.noRunningDescription")}
      />
    );
  }

  const promptRate = rateSeries(data.promptTokens);
  const generationRate = rateSeries(data.generationTokens);
  const runningNow = latestSum(data.running);
  const waitingNow = latestSum(data.waiting);
  const cachePeak = latestMax(data.cache);
  const promptNow = latestSum(promptRate);
  const generationNow = latestSum(generationRate);
  const hasAnyMetric = (Object.keys(data) as MetricKey[]).some((key) => hasPoints(data[key]));

  const concurrencySeries = [
    aggregateSeries(data.running, { metric: t("metrics.runningRequests") }),
    aggregateSeries(data.waiting, { metric: t("metrics.waitingRequests") }),
  ];
  const tokenSeries = [
    aggregateSeries(promptRate, { metric: "Prompt tok/s" }),
    aggregateSeries(generationRate, { metric: "Generation tok/s" }),
  ];

  return (
    <div>
      {/* Stat cards are live readings, not filtered by the window, so the
          range control sits between them and the charts it actually drives. */}
      {metrics.error && !hasAnyMetric ? (
        <Alert type="warning" showIcon message={t("metrics.loadFailed")} description={metrics.error.message} />
      ) : !hasAnyMetric ? (
        // No engine series: the engine may report none at all, in which case
        // the gateway's measurements are what there is to show.
        deploymentId != null ? (
          <GatewayMetricsNotice
            deploymentId={deploymentId}
            sinceMS={Date.now() - eff.startMS}
            pollMs={eff.pollMs || 15000}
            allRunning={liveInstances.every((inst) => inst.state === "running")}
            fallback={<Alert type="info" showIcon message={t("metrics.waiting")} description={t("metrics.waitingDescription")} />}
          />
        ) : (
          <Alert type="info" showIcon message={t("metrics.waiting")} description={t("metrics.waitingDescription")} />
        )
      ) : (
        <Row gutter={[12, 12]}>
          <Col xs={24} sm={12} xl={4}>
            <MetricStat title={t("metrics.runningInstances")} value={liveInstances.length} />
          </Col>
          <Col xs={24} sm={12} xl={5}>
            <MetricStat title={t("metrics.runningRequests")} value={runningNow} />
          </Col>
          <Col xs={24} sm={12} xl={5}>
            <MetricStat title={t("metrics.waitingRequests")} value={waitingNow} />
          </Col>
          <Col xs={24} sm={12} xl={5}>
            <MetricStat title={t("metrics.peakCache")} value={cachePeak != null ? percentScaled(cachePeak) : undefined} suffix="%" />
          </Col>
          <Col xs={24} sm={12} xl={5}>
            <MetricStat title={t("metrics.generationThroughput")} value={generationNow} suffix="tok/s" precision={1} />
          </Col>
        </Row>
      )}

      <RangeHeader selection={selection} onChange={setSelection} beyondRetention={eff.beyondRetention} />

      {hasAnyMetric && (
        <Row gutter={[12, 12]}>
          <Col xs={24} xl={12}>
            <ChartCard title={t("metrics.concurrency")} description={t("metrics.concurrencyDescription")}>
              <TimeSeriesChart
                height={190}
                series={concurrencySeries}
                valueFmt={(v) => v.toFixed(0)}
                integer
              />
            </ChartCard>
          </Col>
          <Col xs={24} xl={12}>
            <ChartCard title={t("metrics.tokenThroughput")} description={t("metrics.tokenDescription", { prompt: promptNow.toFixed(1), generation: generationNow.toFixed(1) })}>
              <TimeSeriesChart height={190} series={tokenSeries} unit="tok/s" valueFmt={autoValueFmt} />
            </ChartCard>
          </Col>
          <Col xs={24}>
            <ChartCard title={t("metrics.instanceCache")} description={t("metrics.instanceCacheDescription")}>
              <TimeSeriesChart height={210} series={data.cache} unit="%" valueFmt={(v) => percentScaled(v).toFixed(0)} />
            </ChartCard>
          </Col>
        </Row>
      )}
    </div>
  );
}

function RangeHeader({
  selection,
  onChange,
  beyondRetention,
}: {
  selection: TimeRangeSelection;
  onChange: (next: TimeRangeSelection) => void;
  beyondRetention?: boolean;
}) {
  const { t } = useTranslation("deployments");
  return (
    <div style={{ marginBottom: 12 }}>
      {/* Metrics always have a concrete window, so the picker's null (live)
          value cannot occur here; filter it for the state setter's type. */}
      <TimeRangePicker value={selection} onChange={(next) => next && onChange(next)} />
      {beyondRetention && (
        <Alert
          style={{ marginTop: 8 }}
          type="warning"
          showIcon
          message={t("timeRange.beyondRetention", { days: RETENTION_WARNING_DAYS })}
        />
      )}
    </div>
  );
}

// loadMetricData fans one query per instance per metric out into the
// panel-level MetricData, tagging every series with its instance so the
// per-instance cache chart can tell the lines apart.
async function loadMetricData(liveInstances: Instance[], selection: TimeRangeSelection): Promise<MetricData> {
  if (liveInstances.length === 0) return EMPTY_DATA;
  const eff = resolveRange(selection, Date.now());
  const params = { start: eff.startMS, end: eff.endMS, step: stepString(eff.stepMS) };
  const perInstance = await Promise.all(
    liveInstances.map(async (inst) => {
      const entries = await Promise.all(
        (Object.keys(METRICS) as MetricKey[]).map(async (key) => ({
          key,
          series: withInstanceLabel(
            (await api.metricsQuery(METRICS[key], { instance: inst.uuid, ...params })).items,
            inst.uuid,
          ),
        })),
      );
      return entries;
    }),
  );

  const next: MetricData = { ...EMPTY_DATA };
  for (const entries of perInstance) {
    for (const { key, series } of entries) next[key] = next[key].concat(series);
  }
  return next;
}

function MetricStat({
  title,
  value,
  suffix,
  precision = 0,
}: {
  title: string;
  value?: number;
  suffix?: string;
  precision?: number;
}) {
  return (
    <Card size="small">
      <Statistic title={title} value={value ?? 0} precision={precision} suffix={suffix} />
    </Card>
  );
}

function ChartCard({ title, description, children }: { title: string; description: string; children: ReactNode }) {
  return (
    <div style={{ border: `1px solid ${colors.borderSoft}`, borderRadius: 4, padding: 16, background: colors.white }}>
      <Typography.Text strong>{title}</Typography.Text>
      <Typography.Text type="secondary" style={{ display: "block", fontSize: 12, marginBottom: 8 }}>
        {description}
      </Typography.Text>
      {children}
    </div>
  );
}

function withInstanceLabel(series: MetricSeries[], instanceUuid: string): MetricSeries[] {
  return series.map((item) => ({
    labels: { instance: instanceUuid, ...item.labels },
    points: item.points ?? [],
  }));
}

function hasPoints(series?: MetricSeries[] | null): boolean {
  return !!series?.some((item) => item.points?.length > 0);
}

function latestSum(series: MetricSeries[]): number {
  return latestValues(series).reduce((sum, value) => sum + value, 0);
}

function latestMax(series: MetricSeries[]): number | undefined {
  const values = latestValues(series);
  return values.length ? Math.max(...values) : undefined;
}

function latestValues(series: MetricSeries[]): number[] {
  const values: number[] = [];
  for (const item of series) {
    const point = item.points?.[item.points.length - 1];
    if (point) values.push(point[1]);
  }
  return values;
}

function rateSeries(series: MetricSeries[]): MetricSeries[] {
  return series.map((item) => ({
    labels: item.labels,
    points: toRatePoints(item.points ?? []),
  }));
}

function toRatePoints(points: MetricPoint[]): MetricPoint[] {
  const out: MetricPoint[] = [];
  for (let i = 1; i < points.length; i++) {
    const prev = points[i - 1];
    const cur = points[i];
    const seconds = (cur[0] - prev[0]) / 1000;
    if (seconds <= 0) continue;
    const delta = cur[1] - prev[1];
    out.push([cur[0], Math.max(0, delta / seconds)]);
  }
  return out;
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
