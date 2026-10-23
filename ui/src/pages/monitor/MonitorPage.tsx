import { useState } from "react";
import { Alert, Button, Card, Col, Row, Skeleton, Space, Typography } from "antd";
import type { TFunction } from "i18next";
import { useTranslation } from "react-i18next";
import EmptyState from "../../components/EmptyState";
import PageHeader from "../../components/PageHeader";
import type { MonitorService, MonitorSignal } from "../../api";
import AsOfCaption from "./AsOfCaption";
import ClusterTokensChart from "./ClusterTokensChart";
import MetricExplorer from "./MetricExplorer";
import ServiceTable from "./ServiceTable";
import SignalTile from "./SignalTile";
import TimeRangeControl, { PickedRangeNote, TimeRangeProvider, useTimeRange } from "./TimeRangeContext";
import HealthSummary from "./HealthSummary";
import InfoTitle from "./InfoTitle";
import RequestLookup from "./RequestLookup";
import { DistributionState, InputOutputHeatmap } from "./RequestShapeCards";
import { useMonitorDistributions, useMonitorOverview } from "./useMonitor";

const DISTRIBUTION_HEIGHT = 260;

const SIGNAL_UNIT: Record<string, string | undefined> = {
  ready_replicas: undefined,
  error_rate: "%",
  request_rate: "req/s",
  token_rate: "tok/s",
  ttft: "ms",
  tpot: "ms",
  waiting: undefined,
  preemptions: "/s",
};

// A signal narrows the service table to the services it is about where
// that is a fact of each service: ready_replicas to the ones short of
// replicas. The other signals are fleet totals with no per-service verdict
// behind them, so clicking them clears any active filter instead. A
// predicate that matches nothing also clears the filter rather than
// narrowing the table to an empty, confusing blank state.
const SIGNAL_PREDICATE: Partial<Record<string, (s: MonitorService) => boolean>> = {
  ready_replicas: (s) => s.replicas.ready < s.replicas.desired,
};

function filterForSignal(key: string, services: MonitorService[]): number[] | null {
  const predicate = SIGNAL_PREDICATE[key];
  if (!predicate) return null;
  const ids = services.filter(predicate).map((s) => s.deployment_id);
  return ids.length > 0 ? ids : null;
}

function signalDetail(signal: MonitorSignal, t: TFunction): string | undefined {
  const notReady = signal.detail?.not_ready;
  if (signal.key === "ready_replicas" && Array.isArray(notReady) && notReady.length > 0) {
    return t("signals.notReadyDetail", { names: notReady.join(", ") });
  }
  if (signal.key === "error_rate" && signal.detail && "5xx" in signal.detail) {
    return t("signals.errorRateDetail", signal.detail);
  }
  return undefined;
}

// signalTooltip is a native title-attribute hint for a signal whose headline
// number needs a caveat the tile's own label/unit can't carry - today only
// ttft/tpot's detail.upper_bound (p95/p99 here is the max, across the
// window, of each tick's own quantile, not a single quantile over the whole
// window - see MonitorSignal's doc comment in api.ts).
function signalTooltip(signal: MonitorSignal, t: TFunction): string | undefined {
  if (signal.detail?.upper_bound) return t("signals.upperBoundHint");
  return undefined;
}

export default function MonitorPage() {
  return (
    <TimeRangeProvider>
      <MonitorPageContent />
    </TimeRangeProvider>
  );
}

function MonitorPageContent() {
  const { t } = useTranslation("monitoring");
  const { t: tCommon } = useTranslation("common");
  const { range } = useTimeRange();
  const { data, error, loading, refetch } = useMonitorOverview(range);
  const distributions = useMonitorDistributions(range);
  const [filterIds, setFilterIds] = useState<number[] | null>(null);

  return (
    <div>
      <PageHeader className="page-header-wrap" title={t("overview.title")} description={t("overview.description")} breadcrumb={[t("overview.title")]} extra={
          <Space size={12} wrap style={{ justifyContent: "flex-end" }}>
            <RequestLookup />
            <TimeRangeControl />
          </Space>
        }
      />

      {error && (
        <Alert
          type="error"
          showIcon
          style={{ marginBottom: 16 }}
          message={t("overview.loadFailed")}
          description={error.message}
          action={<Button size="small" onClick={() => refetch()}>{tCommon("actions.retry")}</Button>}
        />
      )}

      {loading && !data && <Skeleton active paragraph={{ rows: 6 }} />}

      {!loading && !error && data && data.services.length === 0 && <EmptyState kind="deployments" description={t("overview.empty")} />}

      {data && (
        <>
          <AsOfCaption asOf={data.as_of} stale={data.stale} />
          <PickedRangeNote />

          <div className="monitor-signal-grid">
            {data.signals.map((signal) => (
              <SignalTile
                key={signal.key}
                signal={signal}
                label={t(`signals.${signal.key}`)}
                unit={SIGNAL_UNIT[signal.key]}
                detail={signalDetail(signal, t)}
                valueLabel={signal.key === "token_rate" ? t("signals.tokenRateInput") : undefined}
                denominatorLabel={signal.key === "token_rate" ? t("signals.tokenRateOutput") : undefined}
                onClick={signal.collected ? () => setFilterIds(filterForSignal(signal.key, data.services)) : undefined}
                tooltip={signalTooltip(signal, t)}
              />
            ))}
          </div>

          <div style={{ margin: "12px 0" }}>
            <HealthSummary services={data.services} onFilter={setFilterIds} />
          </div>

          <Card
            title={t("table.service")}
            extra={
              filterIds && (
                <Space size={8}>
                  <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                    {t("table.filtered", { count: filterIds.length })}
                  </Typography.Text>
                  <Button size="small" type="link" onClick={() => setFilterIds(null)}>
                    {t("table.clearFilter")}
                  </Button>
                </Space>
              )
            }
          >
            <ServiceTable services={data.services} filterDeploymentIds={filterIds} onRefresh={refetch} loading={loading} />
          </Card>

          <Row gutter={[16, 16]} style={{ marginTop: 16 }}>
            <Col xs={24} xl={10}>
              <Card style={{ height: "100%" }} title={<InfoTitle title={t("overview.inputOutput")} help={t("overview.help.inputOutput")} />}>
                <DistributionState state={distributions} height={DISTRIBUTION_HEIGHT}>
                  {(dist) => <InputOutputHeatmap dist={dist} height={DISTRIBUTION_HEIGHT} />}
                </DistributionState>
              </Card>
            </Col>
            <Col xs={24} xl={14}>
              <Card style={{ height: "100%" }} title={t("overview.clusterTokens")}>
                <ClusterTokensChart
                  prompt={data.cluster_tokens.prompt}
                  generation={data.cluster_tokens.generation}
                  events={data.events}
                  group="monitor-overview"
                />
              </Card>
            </Col>
          </Row>

          <div style={{ marginTop: 16 }}>
            <MetricExplorer range={range} />
          </div>
        </>
      )}
    </div>
  );
}
