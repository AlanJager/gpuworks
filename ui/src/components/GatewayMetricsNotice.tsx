import { Alert, Card, Col, Row, Statistic } from "antd";
import type { ReactNode } from "react";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { api, engineReportsNoMetrics, usePolling, type MonitorRange, type MonitorSeries } from "../api";

// GatewayMetricsNotice stands in for the deployment drawer's engine charts
// when the engine reports no metrics of its own (Strata has no /metrics; a
// callable template may declare no mapping). Which source a signal comes from
// is the server's decision (monitor's signal sources, ADR 0039): the notice
// only reads the resolved deployment snapshot, and renders `fallback` - the
// usual "waiting for metrics" message - unless that snapshot says every
// signal came from the gateway.
//
// sinceMS is how far back the drawer's selected window reaches from now; the
// snapshot covers at least that much. allRunning says every live instance is
// past starting: an engine that reports metrics has them by then, so a window
// with nothing from the engine and nothing through the gateway is an idle
// engine that reports none, not one still coming up.
export default function GatewayMetricsNotice({
  deploymentId,
  sinceMS,
  pollMs,
  allRunning,
  fallback,
}: {
  deploymentId: number;
  sinceMS: number;
  pollMs: number;
  allRunning: boolean;
  fallback: ReactNode;
}) {
  const { t } = useTranslation("deployments");
  const range = monitorRangeCovering(sinceMS);
  const snapshot = usePolling(() => api.monitorDeployment(deploymentId, range), pollMs, [deploymentId, range, pollMs]);
  const data = snapshot.data;
  const toServiceMonitor = <Link to={`/monitor/d/${deploymentId}`}>{t("metrics.openServiceMonitor")}</Link>;
  if (data && allRunning && Object.keys(data.sources ?? {}).length === 0) {
    return (
      <Alert
        type="info"
        showIcon
        message={t("metrics.noEngineNoTraffic")}
        description={
          <>
            {t("metrics.noEngineNoTrafficDescription")} {toServiceMonitor}
          </>
        }
      />
    );
  }
  if (!data || !engineReportsNoMetrics(data.sources)) return <>{fallback}</>;

  const ttft = latest(data.charts.latency, "ttft_p95");
  const e2e = latest(data.charts.latency, "e2e_p95");
  const generation = latest(data.charts.throughput, "generation_tok_s");
  const inflight = latest(data.charts.concurrency, "running");
  return (
    <div>
      <Alert
        type="info"
        showIcon
        message={t("metrics.engineReportsNone")}
        description={
          <>
            {t("metrics.engineReportsNoneDescription")} {toServiceMonitor}
          </>
        }
        style={{ marginBottom: 12 }}
      />
      <Row gutter={[12, 12]}>
        <Stat title={t("metrics.gatewayTTFT")} value={ttft} suffix="ms" />
        <Stat title={t("metrics.gatewayE2E")} value={e2e} suffix="ms" />
        <Stat title={t("metrics.generationThroughput")} value={generation} suffix="tok/s" precision={1} />
        <Stat title={t("metrics.gatewayInflight")} value={inflight} />
      </Row>
    </div>
  );
}

// The deployment monitor answers fixed windows ending now; the drawer's
// window maps to the shortest one that reaches as far back. A minute of slack
// absorbs the milliseconds between resolving a quick range and rendering, so
// "15 minutes" stays 15m rather than spilling into 1h.
const MONITOR_RANGES: Array<[MonitorRange, number]> = [
  ["15m", 15 * 60 * 1000],
  ["1h", 60 * 60 * 1000],
  ["6h", 6 * 60 * 60 * 1000],
  ["24h", 24 * 60 * 60 * 1000],
  ["168h", 7 * 24 * 60 * 60 * 1000],
];

export function monitorRangeCovering(sinceMS: number): MonitorRange {
  return (MONITOR_RANGES.find(([, ms]) => ms + 60 * 1000 >= sinceMS) ?? MONITOR_RANGES[MONITOR_RANGES.length - 1])[0];
}

function Stat({ title, value, suffix, precision = 0 }: { title: string; value?: number; suffix?: string; precision?: number }) {
  return (
    <Col xs={24} sm={12} xl={6}>
      <Card size="small">
        {/* An absent value reads as "-", never as a misleading zero. */}
        <Statistic title={title} value={value ?? "-"} precision={value == null ? undefined : precision} suffix={value == null ? undefined : suffix} />
      </Card>
    </Col>
  );
}

// latest is the newest point of a chart line, summed across the line's
// series (one per replica where the chart is per-instance).
function latest(series: MonitorSeries[], metric: string): number | undefined {
  let at = -1;
  let total: number | undefined;
  for (const entry of series) {
    if (entry.labels.metric !== metric) continue;
    const point = entry.points[entry.points.length - 1];
    if (!point) continue;
    if (point[0] > at) {
      at = point[0];
      total = point[1];
    } else if (point[0] === at) {
      total = (total ?? 0) + point[1];
    }
  }
  return total;
}
