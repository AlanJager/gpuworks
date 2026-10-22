import { Alert, Card, Col, Row, Statistic, Tooltip, Typography } from "antd";
import { InfoCircleOutlined } from "@ant-design/icons";
import { useCallback, useMemo, useState, type ReactNode } from "react";
import { api, usePolling, type MetricPoint, type MetricSeries } from "../api";
import TimeSeriesChart, { autoValueFmt, defaultSeriesName } from "./TimeSeriesChart";
import TimeRangePicker from "./TimeRangePicker";
import { colors } from "../theme/tokens";
import { RETENTION_WARNING_DAYS, resolveRange, stepString, type TimeRangeSelection } from "../timeRange";
import { useTranslation } from "react-i18next";

const METRICS = {
  running: "gpuworks_infer_requests_running",
  waiting: "gpuworks_infer_requests_waiting",
  cache: "gpuworks_infer_gpu_cache_usage_percent",
  promptTokens: "gpuworks_infer_prompt_tokens_total",
  generationTokens: "gpuworks_infer_generation_tokens_total",
  kvHits: "gpuworks_infer_kv_hits_total",
  kvQueries: "gpuworks_infer_kv_queries_total",
  kvHitRateInstant: "gpuworks_infer_kv_hit_rate_instant",
  specAcceptedTokens: "gpuworks_infer_spec_accepted_tokens_total",
  specDraftTokens: "gpuworks_infer_spec_draft_tokens_total",
  specAcceptRateInstant: "gpuworks_infer_spec_accept_rate",
} as const;

// One metrics query bound to the selected window: quick ranges poll at the
// window's cadence, a custom range is a one-shot fetch (pollMs 0).
function useMetricSeries(metric: string, instanceUuid: string, selection: TimeRangeSelection) {
  const eff = resolveRange(selection, Date.now());
  return usePolling(
    () => api.metricsQuery(metric, { instance: instanceUuid, start: eff.startMS, end: eff.endMS, step: stepString(eff.stepMS) }),
    eff.pollMs,
    [instanceUuid, selection.kind === "quick" ? selection.key : `custom-${selection.startMS}-${selection.endMS}`],
  );
}

export default function InferenceMetricsPanel({ instanceUuid }: { instanceUuid: string }) {
  const { t } = useTranslation("deployments");
  const [selection, setSelection] = useState<TimeRangeSelection>({ kind: "quick", key: "15m" });
  const eff = resolveRange(selection, Date.now());
  const running = useMetricSeries(METRICS.running, instanceUuid, selection);
  const waiting = useMetricSeries(METRICS.waiting, instanceUuid, selection);
  const cache = useMetricSeries(METRICS.cache, instanceUuid, selection);
  const promptTokens = useMetricSeries(METRICS.promptTokens, instanceUuid, selection);
  const generationTokens = useMetricSeries(METRICS.generationTokens, instanceUuid, selection);
  const kvHits = useMetricSeries(METRICS.kvHits, instanceUuid, selection);
  const kvQueries = useMetricSeries(METRICS.kvQueries, instanceUuid, selection);
  const kvHitRateInstant = useMetricSeries(METRICS.kvHitRateInstant, instanceUuid, selection);
  const specAcceptedTokens = useMetricSeries(METRICS.specAcceptedTokens, instanceUuid, selection);
  const specDraftTokens = useMetricSeries(METRICS.specDraftTokens, instanceUuid, selection);
  const specAcceptRateInstant = useMetricSeries(METRICS.specAcceptRateInstant, instanceUuid, selection);

  const promptRate = useMemo(() => rateSeries(promptTokens.data?.items ?? []), [promptTokens.data]);
  const generationRate = useMemo(() => rateSeries(generationTokens.data?.items ?? []), [generationTokens.data]);
  const concurrencySeries = useMemo(() => [
    ...withMetricLabel(running.data?.items ?? [], "running"),
    ...withMetricLabel(waiting.data?.items ?? [], "waiting"),
  ], [running.data, waiting.data]);
  const concurrencySeriesName = useCallback((labels: Record<string, string>, index: number) => {
    if (labels.metric === "running") return t("instanceMetrics.running");
    if (labels.metric === "waiting") return t("instanceMetrics.waitingRequests");
    return defaultSeriesName(labels, index);
  }, [t]);
  // vLLM reports cumulative counters, so the window ratio is the precise
  // metric; SGLang only reports an instantaneous gauge, which is used as the
  // fallback when the counter pair is absent. The two are rendered under the
  // same card but never merged into one series.
  const kvWindowSeries = useMemo(
    () => ratioSeries(kvHits.data?.items ?? [], kvQueries.data?.items ?? []),
    [kvHits.data, kvQueries.data],
  );
  const kvCardSeries = useMemo(
    () => (hasPoints(kvWindowSeries) ? kvWindowSeries : kvHitRateInstant.data?.items ?? []),
    [kvWindowSeries, kvHitRateInstant.data],
  );
  const specWindowSeries = useMemo(
    () => ratioSeries(specAcceptedTokens.data?.items ?? [], specDraftTokens.data?.items ?? []),
    [specAcceptedTokens.data, specDraftTokens.data],
  );
  const specCardSeries = useMemo(
    () => (hasPoints(specWindowSeries) ? specWindowSeries : specAcceptRateInstant.data?.items ?? []),
    [specWindowSeries, specAcceptRateInstant.data],
  );
  // Counters that exist but never advanced mean the engine reports the metric
  // while no qualifying traffic happened yet — a different state from the
  // engine not exposing the metric at all.
  const kvCountersReported = hasPoints(kvHits.data?.items) || hasPoints(kvQueries.data?.items);
  const specCountersReported = hasPoints(specAcceptedTokens.data?.items) || hasPoints(specDraftTokens.data?.items);
  const hasAnyMetric =
    hasPoints(running.data?.items) ||
    hasPoints(waiting.data?.items) ||
    hasPoints(cache.data?.items) ||
    hasPoints(promptTokens.data?.items) ||
    hasPoints(generationTokens.data?.items);

  if (!hasAnyMetric && !running.loading && !waiting.loading && !cache.loading) {
    return (
      <div>
        <Alert
          style={{ marginBottom: 12 }}
          type="info"
          showIcon
          message={t("instanceMetrics.waiting")}
          description={t("instanceMetrics.waitingDescription")}
        />
        <RangeHeader selection={selection} onChange={setSelection} />
      </div>
    );
  }

  const runningNow = latest(running.data?.items ?? []);
  const waitingNow = latest(waiting.data?.items ?? []);
  // Cache gauges are 0..1 fractions on both engines; percentScaled renders
  // them (and any already-percent value) as percentages.
  const cacheNow = latest(cache.data?.items ?? []);
  const promptNow = latest(promptRate);
  const generationNow = latest(generationRate);

  return (
    <div>
      {/* Live readings first; the window control only governs the charts. */}
      <Row gutter={[12, 12]}>
        <Col xs={24} sm={12} xl={4}>
          <MetricStat title={t("instanceMetrics.running")} value={runningNow} />
        </Col>
        <Col xs={24} sm={12} xl={4}>
          <MetricStat title={t("instanceMetrics.waitingRequests")} value={waitingNow} />
        </Col>
        <Col xs={24} sm={12} xl={5}>
          <MetricStat title={t("instanceMetrics.gpuCache")} value={cacheNow != null ? percentScaled(cacheNow) : undefined} suffix="%" />
        </Col>
        <Col xs={24} sm={12} xl={5}>
          <MetricStat title={t("instanceMetrics.promptThroughput")} value={promptNow} suffix="tok/s" precision={1} />
        </Col>
        <Col xs={24} sm={12} xl={6}>
          <MetricStat title={t("instanceMetrics.generationThroughput")} value={generationNow} suffix="tok/s" precision={1} />
        </Col>
      </Row>

      <RangeHeader selection={selection} onChange={setSelection} beyondRetention={eff.beyondRetention} />

      <Row gutter={[12, 12]}>
        <Col xs={24} xl={12}>
          <ChartCard title={t("instanceMetrics.requestConcurrency")} description={t("instanceMetrics.requestConcurrencyDescription")}>
            <TimeSeriesChart
              height={190}
              series={concurrencySeries}
              seriesName={concurrencySeriesName}
              valueFmt={(v) => v.toFixed(0)}
              integer
            />
          </ChartCard>
        </Col>
        <Col xs={24} xl={12}>
          <ChartCard title={t("instanceMetrics.cacheUsage")} description={t("instanceMetrics.cacheUsageDescription")}>
            <TimeSeriesChart height={190} series={cache.data?.items ?? []} unit="%" valueFmt={(v) => percentScaled(v).toFixed(0)} />
          </ChartCard>
        </Col>
        <Col xs={24} xl={12}>
          <ChartCard title={t("instanceMetrics.promptToken")} description={t("instanceMetrics.counterRate")}>
            <TimeSeriesChart height={190} series={promptRate} unit="tok/s" valueFmt={autoValueFmt} />
          </ChartCard>
        </Col>
        <Col xs={24} xl={12}>
          <ChartCard title={t("instanceMetrics.generationToken")} description={t("instanceMetrics.counterRate")}>
            <TimeSeriesChart height={190} series={generationRate} unit="tok/s" valueFmt={autoValueFmt} />
          </ChartCard>
        </Col>
        <Col xs={24} xl={12}>
          <ChartCard
            title={t("instanceMetrics.specAcceptRate")}
            description={t("instanceMetrics.specAcceptRateDescription")}
            hint={t("instanceMetrics.specAcceptRateScopeHint")}
          >
            <RatioChart series={specCardSeries} countersReported={specCountersReported} />
          </ChartCard>
        </Col>
        <Col xs={24} xl={12}>
          <ChartCard
            title={t("instanceMetrics.kvHitRate")}
            description={t("instanceMetrics.kvHitRateDescription")}
            hint={t("instanceMetrics.kvHitRateScopeHint")}
          >
            <RatioChart series={kvCardSeries} countersReported={kvCountersReported} />
          </ChartCard>
        </Col>
      </Row>
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

function RatioChart({ series, countersReported }: { series: MetricSeries[]; countersReported: boolean }) {
  const { t } = useTranslation("deployments");
  if (!hasPoints(series)) {
    return (
      <div style={{ height: 190, display: "flex", alignItems: "center", justifyContent: "center" }}>
        <Typography.Text type="secondary">
          — {countersReported ? t("instanceMetrics.metricNoTraffic") : t("instanceMetrics.metricUnavailable")}
        </Typography.Text>
      </div>
    );
  }
  return (
    <TimeSeriesChart
      height={190}
      series={series}
      unit="%"
      valueFmt={(v) => percentScaled(v).toFixed(1)}
    />
  );
}

// Engines disagree on whether ratio gauges are 0..1 fractions or 0..100
// percentages across versions; anything above 1 can only be a percentage.
export function percentScaled(value: number): number {
  return value > 1 ? value : value * 100;
}

function withMetricLabel(series: MetricSeries[], metric: string): MetricSeries[] {
  return series.map((item) => ({
    labels: { ...item.labels, metric },
    points: item.points ?? [],
  }));
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
      <Statistic title={title} value={value ?? "—"} precision={precision} suffix={value == null ? undefined : suffix} />
    </Card>
  );
}

function ChartCard({
  title,
  description,
  hint,
  children,
}: {
  title: ReactNode;
  description: string;
  hint?: string;
  children: ReactNode;
}) {
  return (
    <div style={{ border: `1px solid ${colors.borderSoft}`, borderRadius: 4, padding: 16, background: colors.white }}>
      <Typography.Text strong>
        {title}
        {hint && (
          <Tooltip title={hint}>
            <InfoCircleOutlined style={{ marginLeft: 6, color: colors.muted }} aria-label={description} />
          </Tooltip>
        )}
      </Typography.Text>
      <Typography.Text type="secondary" style={{ display: "block", fontSize: 12, marginBottom: 8 }}>
        {description}
      </Typography.Text>
      {children}
    </div>
  );
}

function hasPoints(series?: MetricSeries[] | null): boolean {
  return !!series?.some((item) => item.points?.length > 0);
}

function latest(series: MetricSeries[]): number | undefined {
  for (let i = series.length - 1; i >= 0; i--) {
    const points = series[i].points;
    if (points?.length) return points[points.length - 1][1];
  }
  return undefined;
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

// ratioSeries derives a windowed ratio (e.g. cache hits / queries) from two
// cumulative counter series scraped in the same batch, so timestamps align.
// Windows where either counter reset (engine restart) or the denominator did
// not advance produce no point instead of a fabricated value.
export function ratioSeries(numerator: MetricSeries[], denominator: MetricSeries[]): MetricSeries[] {
  const denomByKey = new Map(denominator.map((item) => [JSON.stringify(item.labels ?? {}), item]));
  return numerator.map((num) => {
    const den = denomByKey.get(JSON.stringify(num.labels ?? {}));
    return {
      labels: num.labels,
      points: den ? toRatioPoints(num.points ?? [], den.points ?? []) : [],
    };
  });
}

function toRatioPoints(num: MetricPoint[], den: MetricPoint[]): MetricPoint[] {
  const denByTime = new Map(den.map((p) => [p[0], p[1]]));
  const out: MetricPoint[] = [];
  let prev: { timeMS: number; num: number; den: number } | null = null;
  for (const point of num) {
    const denValue = denByTime.get(point[0]);
    if (denValue === undefined) continue;
    if (prev) {
      const numDelta = point[1] - prev.num;
      const denDelta = denValue - prev.den;
      if (denDelta > 0 && numDelta >= 0) {
        out.push([point[0], numDelta / denDelta]);
      }
    }
    prev = { timeMS: point[0], num: point[1], den: denValue };
  }
  return out;
}
