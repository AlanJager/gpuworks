import { useState, type ReactNode } from "react";
import { Alert, Card, Col, Progress, Row, Segmented, Select, Space, Statistic, Tag, Tooltip, Typography } from "antd";
import {
  api,
  useEvents,
  usePolling,
  type CapacityGroup,
  type CloudProvider,
  type DashboardSummary,
  type Health,
  type List,
  type MetricSeries,
} from "../api";
import TimeSeriesChart from "../components/TimeSeriesChart";
import PageHeader from "../components/PageHeader";
import { colors } from "../theme/tokens";
import { useTranslation } from "react-i18next";

type RangeKey = "1h" | "6h" | "24h";
// A coarser step for wider windows keeps the point count sane.
const STEP: Record<RangeKey, string> = { "1h": "30s", "6h": "60s", "24h": "300s" };

const GIB = 1024 ** 3;
const fmtGiB = (v: number) => `${(v / GIB).toFixed(1)}`;
const fmtWatts = (v: number) => v.toFixed(0);
const fmtCelsius = (v: number) => v.toFixed(0);

export default function DashboardPage() {
  const { t } = useTranslation("dashboard");
  const health = usePolling<Health>(api.health, 10000);
  const [pickedLabelKey, setPickedLabelKey] = useState<string>();
  // The server counts the fleet; fetching every node and deployment to count
  // them here did not scale to a thousand nodes.
  const summary = usePolling<DashboardSummary>(() => api.dashboardSummary(pickedLabelKey), 10000, [pickedLabelKey]);
  const providers = usePolling<List<CloudProvider>>(api.providers, 15000);
  useEvents("node", () => {
    health.refetch();
    summary.refetch();
    providers.refetch();
  });
  useEvents("deployment", () => {
    summary.refetch();
  });
  useEvents("provider", () => {
    providers.refetch();
  });

  const [range, setRange] = useState<RangeKey>("6h");
  const util = usePolling<List<MetricSeries>>(
    () => api.metricsQuery("gpuworks_gpu_utilization", { range, step: STEP[range] }),
    30000,
    [range],
  );
  const mem = usePolling<List<MetricSeries>>(
    () => api.metricsQuery("gpuworks_gpu_memory_used_bytes", { range, step: STEP[range] }),
    30000,
    [range],
  );
  const temp = usePolling<List<MetricSeries>>(
    () => api.metricsQuery("gpuworks_gpu_temperature_celsius", { range, step: STEP[range] }),
    30000,
    [range],
  );
  const power = usePolling<List<MetricSeries>>(
    () => api.metricsQuery("gpuworks_gpu_power_watts", { range, step: STEP[range] }),
    30000,
    [range],
  );

  const fleet = summary.data;
  const providerList = providers.data?.items ?? [];
  const publicProviderList = providerList.filter((provider) => provider.kind !== "zstack");
  const privateProviderList = providerList.filter((provider) => provider.kind === "zstack");
  const nodeTotal = fleet?.nodes.total ?? 0;
  const online = fleet?.nodes.online ?? 0;
  const offline = fleet?.nodes.offline ?? 0;
  const gpuTotal = fleet?.nodes.gpus ?? 0;
  const unusableGPUs = fleet?.nodes.unusable_gpus ?? 0;
  const vramTotal = fleet?.nodes.vram_gib ?? 0;
  const memTotal = fleet?.nodes.memory_gib ?? 0;
  const publicProviderValid = publicProviderList.filter((p) => p.state === "valid").length;
  const publicProviderInvalid = publicProviderList.filter((p) => p.state === "invalid").length;
  const privateProviderInvalid = privateProviderList.filter((p) => p.state === "invalid").length;
  const deployTotal = fleet?.deployments.total ?? 0;
  const deployRunning = fleet?.deployments.running ?? 0;
  const deployWarming = fleet?.deployments.warming ?? 0;
  const deploySleepable = fleet?.deployments.sleepable ?? 0;
  const current = summary.error ? undefined : fleet?.current_metrics;
  const avgUtil = current?.utilization_pct ?? undefined;
  const maxTemp = current?.temperature_c ?? undefined;
  const powerTotal = current?.power_watts ?? undefined;
  const nodeMemTotal = current?.memory_used_bytes != null ? current.memory_used_bytes / GIB : undefined;
  const issueCount = unusableGPUs + offline + publicProviderInvalid + privateProviderInvalid + (health.data?.status === "ok" ? 0 : 1);

  const vendorRows = fleet?.by_vendor ?? [];
  const keys = fleet?.label_keys ?? [];
  // The server falls back to the most widely used key, also when a label edit
  // removes the picked key from every node.
  const labelKey = fleet?.label_key || undefined;
  const labelRows = fleet?.by_label ?? [];

  return (
    <div>
      <PageHeader title={t("title")} description={t("description")} />
      <Alert
        type={issueCount > 0 ? "warning" : "success"}
        showIcon
        style={{ marginBottom: 16 }}
        message={issueCount > 0 ? t("health.attention", { count: issueCount }) : t("health.normal")}
        description={
          issueCount > 0
            ? t("health.attentionDescription")
            : t("health.normalDescription")
        }
      />
      <Row gutter={[16, 16]}>
        <Col xs={24} md={12} xl={6}>
          <Card>
            <Statistic title={t("stats.nodesTotal")} value={nodeTotal} />
          </Card>
        </Col>
        <Col xs={24} md={12} xl={6}>
          <Card>
            <Statistic title={t("stats.nodesOnline")} value={online} valueStyle={{ color: colors.successText }} />
          </Card>
        </Col>
        <Col xs={24} md={12} xl={6}>
          <Card>
            <Statistic
              // Capacity, not hardware: cards passed through to a VM or
              // without a bound driver are listed on the GPU page and
              // counted there, and the two numbers differ by design.
              title={<Tooltip title={t("stats.gpusTotalHelp")}><span className="gpu-column-help">{t("stats.gpusTotal")}</span></Tooltip>}
              value={gpuTotal}
              suffix={unusableGPUs > 0 ? <Tag color="red">{t("labels.unusableCount", { count: unusableGPUs })}</Tag> : undefined}
            />
          </Card>
        </Col>
        <Col xs={24} md={12} xl={6}>
          <Card>
            <Statistic title={t("stats.vramTotal")} value={vramTotal} precision={0} />
          </Card>
        </Col>
      </Row>

      <Row gutter={[16, 16]} style={{ marginTop: 16 }}>
        <Col xs={24} md={12} xl={6}>
          <Card>
            <Statistic title={t("stats.averageUtilization")} value={avgUtil ?? "—"} precision={0} suffix="%" valueStyle={{ color: utilColor(avgUtil) }} />
          </Card>
        </Col>
        <Col xs={24} md={12} xl={6}>
          <Card>
            <Statistic title={t("stats.maximumTemperature")} value={maxTemp ?? "—"} precision={0} suffix="C" valueStyle={{ color: tempColor(maxTemp) }} />
          </Card>
        </Col>
        <Col xs={24} md={12} xl={6}>
          <Card>
            <Statistic title={t("stats.totalPower")} value={powerTotal ?? "—"} precision={0} suffix="W" />
          </Card>
        </Col>
        <Col xs={24} md={12} xl={6}>
          <Card>
            <Statistic title={t("stats.hostMemoryUsed")} value={nodeMemTotal ?? "—"} precision={1} suffix={`/ ${memTotal.toFixed(0)} GiB`} />
          </Card>
        </Col>
      </Row>

      <Row gutter={[16, 16]} style={{ marginTop: 16 }}>
        <Col xs={24} md={12} xl={6}>
          <Card>
            <Statistic
              title={t("stats.publicClouds")}
              value={publicProviderList.length}
              suffix={publicProviderInvalid > 0 ? <Tag color="red">{t("labels.invalidCount", { count: publicProviderInvalid })}</Tag> : <Tag color="green">{t("labels.validCount", { count: publicProviderValid })}</Tag>}
            />
          </Card>
        </Col>
        <Col xs={24} md={12} xl={6}>
          <Card>
            <Statistic title={t("stats.deploymentsTotal")} value={deployTotal} />
          </Card>
        </Col>
        <Col xs={24} md={12} xl={6}>
          <Card>
            <Statistic title={t("stats.deploymentsRunning")} value={deployRunning} valueStyle={{ color: colors.successText }} />
          </Card>
        </Col>
        <Col xs={24} md={12} xl={6}>
          <Card>
            <Statistic title={t("stats.deploymentsWarming")} value={deployWarming} />
          </Card>
        </Col>
      </Row>

      <Card style={{ marginTop: 16 }} title={t("situation.title")}>
        <Space wrap>
          <Tag color={health.data?.status === "ok" ? "green" : "gold"}>{t("situation.platform", { state: health.data?.status ?? "unknown" })}</Tag>
          <Tag color={offline > 0 ? "red" : "green"}>{t("situation.offlineNodes", { count: offline })}</Tag>
          <Tag color={unusableGPUs > 0 ? "red" : "green"}>{t("situation.unusableGPUs", { count: unusableGPUs })}</Tag>
          <Tag color={publicProviderInvalid > 0 ? "red" : "green"}>{t("situation.invalidPublicClouds", { count: publicProviderInvalid })}</Tag>
          <Tag color={privateProviderInvalid > 0 ? "red" : "green"}>{t("situation.invalidPrivateClouds", { count: privateProviderInvalid })}</Tag>
          <Tag color={deploySleepable > 0 ? "blue" : "default"}>{t("situation.sleepableDeployments", { count: deploySleepable })}</Tag>
          <Tag color="blue">{t("situation.activeAgentSessions", { count: health.data?.sessions ?? 0 })}</Tag>
        </Space>
      </Card>

      <Row gutter={[16, 16]} style={{ marginTop: 16 }}>
        <Col xs={24} xl={12}>
          <Card title={t("capacity.byVendor")}>
            <CapacityRows rows={vendorRows} totalGPUs={gpuTotal} />
          </Card>
        </Col>
        <Col xs={24} xl={12}>
          <Card
            title={t("capacity.byLabel")}
            extra={
              keys.length > 0 && (
                <Select
                  size="small"
                  style={{ minWidth: 140 }}
                  value={labelKey}
                  onChange={setPickedLabelKey}
                  aria-label={t("capacity.labelKeyAria")}
                  options={keys.map((key) => ({ value: key, label: key }))}
                />
              )
            }
          >
            <CapacityRows
              rows={labelRows}
              totalGPUs={gpuTotal}
              emptyText={nodeTotal > 0 && keys.length === 0 ? t("capacity.noLabels") : undefined}
            />
          </Card>
        </Col>
      </Row>

      <Card
        style={{ marginTop: 16 }}
        title={t("metrics.title")}
        extra={
          <Segmented
            size="small"
            value={range}
            onChange={(v) => setRange(v as RangeKey)}
            options={["1h", "6h", "24h"]}
          />
        }
      >
        <Row gutter={[16, 16]}>
          <Col xs={24} xl={12}>
            <MetricBlock title={t("metrics.utilization.title")} subtitle={t("metrics.utilization.description")}>
              <TimeSeriesChart series={util.data?.items ?? []} unit="%" valueFmt={(v) => v.toFixed(0)} yRange={[0, 100]} />
            </MetricBlock>
          </Col>
          <Col xs={24} xl={12}>
            <MetricBlock title={t("metrics.vram.title")} subtitle={t("metrics.vram.description")}>
              <TimeSeriesChart series={mem.data?.items ?? []} unit="GiB" valueFmt={fmtGiB} />
            </MetricBlock>
          </Col>
          <Col xs={24} xl={12}>
            <MetricBlock title={t("metrics.temperature.title")} subtitle={t("metrics.temperature.description")}>
              <TimeSeriesChart series={temp.data?.items ?? []} unit="C" valueFmt={fmtCelsius} />
            </MetricBlock>
          </Col>
          <Col xs={24} xl={12}>
            <MetricBlock title={t("metrics.power.title")} subtitle={t("metrics.power.description")}>
              <TimeSeriesChart series={power.data?.items ?? []} unit="W" valueFmt={fmtWatts} />
            </MetricBlock>
          </Col>
        </Row>
      </Card>

      <Card style={{ marginTop: 16 }} title={t("platform.title")}>
        <Space wrap>
          <Tag color={health.data?.status === "ok" ? "green" : "orange"}>{health.data?.status ?? "unknown"}</Tag>
          <Typography.Text>{t("platform.version", { version: health.data?.version ?? "—" })}</Typography.Text>
          <Typography.Text>{t("situation.activeAgentSessions", { count: health.data?.sessions ?? 0 })}</Typography.Text>
        </Space>
      </Card>
    </div>
  );
}

function CapacityRows({ rows, totalGPUs, emptyText }: { rows: CapacityGroup[]; totalGPUs: number; emptyText?: string }) {
  const { t } = useTranslation("dashboard");
  if (rows.length === 0) {
    return (
      <Typography.Text type="secondary" style={{ display: "block", padding: "22px 0" }}>
        {emptyText ?? t("capacity.empty")}
      </Typography.Text>
    );
  }
  return (
    <Space direction="vertical" size={10} style={{ width: "100%" }}>
      {rows.map((row) => {
        const pct = totalGPUs > 0 ? (row.gpus / totalGPUs) * 100 : 0;
        const name = row.unlabeled ? t("capacity.unlabeled") : row.key;
        return (
          <div key={row.unlabeled ? "\u0000unlabeled" : row.key}>
            <div style={{ display: "flex", justifyContent: "space-between", gap: 12, marginBottom: 4 }}>
              <Space size={6} wrap>
                <Typography.Text strong>{name}</Typography.Text>
                <Tag>{t("capacity.nodeCount", { count: row.nodes })}</Tag>
                {row.unusable_gpus > 0 && <Tag color="red">{t("capacity.unusableGPUCount", { count: row.unusable_gpus })}</Tag>}
              </Space>
              <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                {row.gpus} GPU · {row.vram_gib.toFixed(0)} GiB
              </Typography.Text>
            </div>
            <Progress percent={pct} size="small" showInfo={false} strokeColor={colors.chartPrimary} trailColor={colors.borderSoft} />
          </div>
        );
      })}
    </Space>
  );
}

function MetricBlock({ title, subtitle, children }: { title: string; subtitle: string; children: ReactNode }) {
  return (
    <div style={{ border: `1px solid ${colors.borderSoft}`, borderRadius: 4, padding: 16, minHeight: 282, background: colors.white }}>
      <div style={{ marginBottom: 8 }}>
        <Typography.Text strong>{title}</Typography.Text>
        <Typography.Text type="secondary" style={{ display: "block", fontSize: 12 }}>
          {subtitle}
        </Typography.Text>
      </div>
      {children}
    </div>
  );
}

function utilColor(value?: number): string {
  if (value == null) return colors.ink;
  if (value >= 85) return colors.warningText;
  return colors.successText;
}

function tempColor(value?: number): string {
  if (value == null) return colors.ink;
  if (value >= 85) return colors.dangerText;
  if (value >= 75) return colors.warningText;
  return colors.successText;
}
