import { useEffect, useState } from "react";
import { Descriptions, Segmented, Space, Tag, Typography } from "antd";
import {
  AppstoreOutlined, DashboardOutlined, DatabaseOutlined, FireOutlined, ThunderboltOutlined,
} from "@ant-design/icons";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { api, type DGPU, type MetricSeries, type PhysicalGPU } from "../../api";
import DetailDrawer from "../../components/detail/DetailDrawer";
import DetailSection from "../../components/detail/DetailSection";
import CardHealthTab from "../../components/cardHealth/CardHealthTab";
import GPUHealthTag from "../../components/GPUHealth";
import GPUMemoryComposition from "../../components/GPUMemoryComposition";
import PartitionCapability from "../../components/PartitionCapability";
import TimeSeriesChart from "../../components/TimeSeriesChart";
import { cardHasDetail } from "../../nodes/cardIdentity";
import { GIB, allocationLabels, dash, fmtGiB, fmtRate, gpuHeading, optionalPercent, shortID, vendorLabel } from "./gpuFormat";

// PhysicalGPUDetail is one card's detail: what it is and who holds it, then
// how it is doing now and over time.
export function PhysicalGPUDetail({ gpu, onClose }: { gpu: PhysicalGPU | null; onClose: () => void }) {
  const { t } = useTranslation("gpus");
  const { t: tNodes } = useTranslation("nodes");
  const [tab, setTab] = useState("basic");
  useEffect(() => setTab("basic"), [gpu?.uuid, gpu?.node_uuid, gpu?.index]);
  return (
    <DetailDrawer
      open={!!gpu}
      onClose={onClose}
      title={gpu ? `${gpu.name.replace(/^NVIDIA\s+/i, "")} · ${gpuHeading(gpu)}` : t("detail.physicalTitle")}
      subtitle={gpu?.node_name}
      className="gpu-detail-drawer"
      activeTab={tab}
      onTabChange={setTab}
      tabs={
        gpu
          ? [
              {
                key: "basic",
                label: t("detail.tab.basic"),
                children: (
                  <>
                    {/* What the card is doing now is what one opens it for most. */}
                    {cardHasDetail(gpu) && gpu.memory_total_bytes > 0 && (
                      <DetailSection title={t("composition.title")}><GPUMemoryComposition gpu={gpu} /></DetailSection>
                    )}
                    <DetailSection title={t("detail.basicInfo")}>
                      <Descriptions bordered size="small" column={1}>
                        <Descriptions.Item label={t("column.allocationState")}>
                          <Tag color={allocationLabels[gpu.allocation_state].color}>{t(allocationLabels[gpu.allocation_state].labelKey)}</Tag>
                        </Descriptions.Item>
                        <Descriptions.Item label={t("column.health")}><GPUHealthTag gpu={gpu} /></Descriptions.Item>
                        <Descriptions.Item label={t("column.node")}><Link to="/nodes" state={{ nodeUUID: gpu.node_uuid }}>{gpu.node_name}</Link></Descriptions.Item>
                        <Descriptions.Item label={t("column.occupants")}>
                          {gpu.occupants.length
                            ? gpu.occupants.map((item) => (
                                <div key={item.instance_uuid}>
                                  <Link to={`/deployments?deployment=${item.deployment_id}`}>{item.deployment_name || shortID(item.instance_uuid)}</Link>
                                  {item.limit_bytes ? ` · ${fmtGiB(item.limit_bytes)}` : ` · ${t("fullGPU")}`}
                                </div>
                              ))
                            : "-"}
                        </Descriptions.Item>
                        <Descriptions.Item label="BDF">{dash(gpu.pcie_bdf)}</Descriptions.Item>
                        <Descriptions.Item label="NUMA">{gpu.numa_node < 0 ? "-" : gpu.numa_node}</Descriptions.Item>
                        <Descriptions.Item label="UUID"><Typography.Text copyable code>{gpu.uuid || "-"}</Typography.Text></Descriptions.Item>
                        <Descriptions.Item label={t("detail.partitionCapability")}>
                          <Space direction="vertical" size={6}>
                            {(gpu.partition_capabilities ?? []).map((capability) => <PartitionCapability key={capability.backend} capability={capability} />)}
                            {!gpu.partition_capabilities?.length && t("detail.partitionCapabilityEmpty")}
                          </Space>
                        </Descriptions.Item>
                      </Descriptions>
                    </DetailSection>
                    <DetailSection title={t("detail.hardwareInfo")}>
                      <Descriptions bordered size="small" column={1}>
                        <Descriptions.Item label={t("column.vendor")}>{vendorLabel(gpu.vendor, t)}</Descriptions.Item>
                        <Descriptions.Item label={t("column.model")}>{gpu.name}</Descriptions.Item>
                        <Descriptions.Item label={t("detail.architecture")}>{gpu.arch_id || gpu.arch_family || "-"}</Descriptions.Item>
                        <Descriptions.Item label={t("metric.vram")}>{fmtGiB(gpu.memory_total_bytes)}</Descriptions.Item>
                        <Descriptions.Item label={t("detail.driver")}>{dash(gpu.driver_version)}</Descriptions.Item>
                        <Descriptions.Item label={t("detail.maxPower")}>{gpu.power_limit_watts ? `${gpu.power_limit_watts.toFixed(0)} W` : "-"}</Descriptions.Item>
                      </Descriptions>
                    </DetailSection>
                  </>
                ),
              },
              {
                key: "monitoring",
                label: t("detail.tab.monitoring"),
                children: (
                  <>
                    <DetailSection title={t("detail.liveLoad")}><LoadTiles gpu={gpu} /></DetailSection>
                    <DetailSection title={t("detail.monitoringData")}><GPUHistory node={gpu.node_uuid} gpu={gpu.index} /></DetailSection>
                  </>
                ),
              },
              ...(gpu.node_uuid && (gpu.pcie_bdf || gpu.uuid)
                ? [{
                    key: "health",
                    label: tNodes("cardHealth.tab"),
                    children: <CardHealthTab nodeUUID={gpu.node_uuid} card={{ pcie_bdf: gpu.pcie_bdf, uuid: gpu.uuid }} cardLabel={gpuHeading(gpu)} />,
                  }]
                : []),
            ]
          : undefined
      }
    />
  );
}

// DGPUDetail is one slice's detail: what it uses now, its quota, then its use
// over time. Too little for tabs. Only some slicing backends report a slice's
// own use (NVIDIA's HAMi writes it; Ascend and PPU do not), so the use and its
// charts show only for a slice that has reported it: an empty block on every
// other slice would read as a missing feature.
export function DGPUDetail({ gpu, onClose }: { gpu: DGPU | null; onClose: () => void }) {
  const { t } = useTranslation("gpus");
  return (
    <DetailDrawer
      open={!!gpu}
      onClose={onClose}
      title={gpu ? `${t("tab.dgpu")} · ${gpu.deployment_name || shortID(gpu.instance_uuid)}` : t("detail.dgpuTitle")}
      subtitle={gpu?.node_name}
    >
      {gpu && (
        <>
          {reportsUse(gpu) && <DetailSection title={t("detail.currentUse")}><SliceUseTiles gpu={gpu} /></DetailSection>}
          <DetailSection title={t("detail.quotaInfo")}>
            <Descriptions bordered size="small" column={1}>
              <Descriptions.Item label={t("column.deployment")}><Link to={`/deployments?deployment=${gpu.deployment_id}`}>{gpu.deployment_name || t("unnamedDeployment")}</Link></Descriptions.Item>
              <Descriptions.Item label={t("column.node")}><Link to="/nodes" state={{ nodeUUID: gpu.node_uuid }}>{gpu.node_name}</Link></Descriptions.Item>
              <Descriptions.Item label={t("column.physicalGPU")}>{gpu.gpu_name || "GPU"} #{gpu.gpu_index}</Descriptions.Item>
              <Descriptions.Item label={t("column.vramLimit")}>{fmtGiB(gpu.memory_limit_bytes)}</Descriptions.Item>
              <Descriptions.Item label={t("column.smLimit")}>{gpu.sm_limit_pct ? `${gpu.sm_limit_pct}%` : "-"}</Descriptions.Item>
              <Descriptions.Item label={t("column.instance")}><Typography.Text code>{shortID(gpu.instance_uuid)}</Typography.Text></Descriptions.Item>
            </Descriptions>
          </DetailSection>
          {reportsUse(gpu) && <DetailSection title={t("detail.monitoringData")}><DGPUHistory gpu={gpu} /></DetailSection>}
        </>
      )}
    </DetailDrawer>
  );
}

const reportsUse = (gpu: DGPU) => gpu.memory_used_bytes != null || gpu.sm_util_pct != null;

function SliceUseTiles({ gpu }: { gpu: DGPU }) {
  const { t } = useTranslation("gpus");
  const items = [
    { icon: <DatabaseOutlined />, label: t("column.actualVRAM"), value: gpu.memory_used_bytes == null ? "-" : `${fmtGiB(gpu.memory_used_bytes)} / ${fmtGiB(gpu.memory_limit_bytes)}` },
    { icon: <DashboardOutlined />, label: t("column.smReference"), value: gpu.sm_util_pct == null ? "-" : `${gpu.sm_util_pct.toFixed(0)}%` },
  ];
  return <div className="gpu-load-grid">{items.map((item) => <div className="gpu-load-tile" key={item.label}><span>{item.icon}</span><div><Typography.Text type="secondary">{item.label}</Typography.Text><strong>{item.value}</strong></div></div>)}</div>;
}

function LoadTiles({ gpu }: { gpu: PhysicalGPU }) {
  const { t } = useTranslation("gpus");
  const items = [
    { icon: <DashboardOutlined />, label: t("metric.gpuUtilization"), value: optionalPercent(gpu.load.utilization_pct) },
    { icon: <DatabaseOutlined />, label: t("metric.vram"), value: gpu.load.memory_used_bytes == null ? "-" : `${fmtGiB(gpu.load.memory_used_bytes)} / ${fmtGiB(gpu.memory_total_bytes)}` },
    { icon: <FireOutlined />, label: t("metric.temperature"), value: gpu.load.temperature_c == null ? "-" : `${gpu.load.temperature_c.toFixed(0)} °C` },
    { icon: <ThunderboltOutlined />, label: t("metric.power"), value: gpu.load.power_watts == null ? "-" : `${gpu.load.power_watts.toFixed(0)} W` },
    { icon: <AppstoreOutlined />, label: t("metric.fan"), value: optionalPercent(gpu.load.fan_speed_pct) },
    { icon: <ThunderboltOutlined />, label: "PCIe RX / TX", value: `${fmtRate(gpu.load.pcie_rx_bytes_per_sec)} / ${fmtRate(gpu.load.pcie_tx_bytes_per_sec)}` },
  ];
  return <div className="gpu-load-grid">{items.map((item) => <div className="gpu-load-tile" key={item.label}><span>{item.icon}</span><div><Typography.Text type="secondary">{item.label}</Typography.Text><strong>{item.value}</strong></div></div>)}</div>;
}

const ranges = { "15m": "15m", "1h": "1h", "6h": "6h", "24h": "24h" } as const;
const steps: Record<keyof typeof ranges, string> = { "15m": "10s", "1h": "30s", "6h": "2m", "24h": "10m" };

function GPUHistory({ node, gpu }: { node: string; gpu: number }) {
  const { t } = useTranslation("gpus");
  const [range, setRange] = useState<keyof typeof ranges>("15m");
  const charts = useMetricCharts([
    [t("metric.gpuUtilization"), "gpuworks_gpu_utilization", "%", (value) => value.toFixed(0), undefined, PERCENT],
    [t("metric.vramUsage"), "gpuworks_gpu_memory_used_bytes", "GiB", (value) => (value / GIB).toFixed(1)],
    [t("metric.temperature"), "gpuworks_gpu_temperature_celsius", "°C", (value) => value.toFixed(0)],
    [t("metric.power"), "gpuworks_gpu_power_watts", "W", (value) => value.toFixed(0)],
    [t("metric.fanSpeed"), "gpuworks_gpu_fan_speed_percent", "%", (value) => value.toFixed(0), undefined, PERCENT],
    ["PCIe RX", "gpuworks_gpu_pcie_rx_bytes_per_sec", "MiB/s", (value) => (value / 1024 ** 2).toFixed(1)],
    ["PCIe TX", "gpuworks_gpu_pcie_tx_bytes_per_sec", "MiB/s", (value) => (value / 1024 ** 2).toFixed(1)],
  ], { node, gpu: String(gpu), range, step: steps[range] });
  return <><Segmented value={range} onChange={(value) => setRange(value as keyof typeof ranges)} options={Object.keys(ranges)} size="small" /><ChartGrid charts={charts} /></>;
}

function DGPUHistory({ gpu }: { gpu: DGPU }) {
  const { t } = useTranslation("gpus");
  const [range, setRange] = useState<keyof typeof ranges>("15m");
  const charts = useMetricCharts([
    [t("metric.sliceVRAM", { limit: fmtGiB(gpu.memory_limit_bytes) }), "gpuworks_dgpu_memory_used_bytes", "GiB", (value) => (value / GIB).toFixed(1), undefined, [0, gpu.memory_limit_bytes]],
    // HAMi samples which slice held the SMs at that instant, so two slices
    // sharing a card read 0 and 100 by turns; the trend is the average.
    [t("metric.smReferenceAvg"), "gpuworks_dgpu_sm_util", "%", (value) => value.toFixed(0), (series) => movingAverage(series, SM_AVERAGE_WINDOW_MS), PERCENT],
  ], { node: gpu.node_uuid, gpu: String(gpu.gpu_index), instance: gpu.instance_uuid, range, step: steps[range] });
  return <><Segmented value={range} onChange={(value) => setRange(value as keyof typeof ranges)} options={Object.keys(ranges)} size="small" /><ChartGrid charts={charts} /></>;
}

type ChartSpec = [string, string, string, (value: number) => string, ((series: MetricSeries[]) => MetricSeries[])?, [number, number]?];
type ChartModel = { title: string; unit: string; format: (value: number) => string; series: MetricSeries[]; yRange?: [number, number] };

function useMetricCharts(specs: ChartSpec[], params: { node: string; gpu: string; instance?: string; range: string; step: string }): ChartModel[] {
  const [charts, setCharts] = useState<ChartModel[]>(() => specs.map(([title, , unit, format, , yRange]) => ({ title, unit, format, series: [], yRange })));
  useEffect(() => {
    let alive = true;
    Promise.all(specs.map(async ([title, metric, unit, format, transform, yRange]) => {
      const series = (await api.metricsQuery(metric, params)).items;
      return { title, unit, format, series: transform ? transform(series) : series, yRange };
    }))
      .then((data) => { if (alive) setCharts(data); })
      .catch(() => { if (alive) setCharts(specs.map(([title, , unit, format, , yRange]) => ({ title, unit, format, series: [], yRange }))); });
    return () => { alive = false; };
    // Metric names are fixed by the caller; query identity is fully captured below.
  }, [params.node, params.gpu, params.instance, params.range, params.step]);
  return charts;
}

function ChartGrid({ charts }: { charts: ChartModel[] }) {
  return <div className="gpu-chart-grid">{charts.map((chart) => <div className="gpu-chart" key={chart.title}><Typography.Text strong>{chart.title}</Typography.Text><TimeSeriesChart height={180} series={chart.series} unit={chart.unit} valueFmt={chart.format} yRange={chart.yRange} /></div>)}</div>;
}

const SM_AVERAGE_WINDOW_MS = 60_000;
const PERCENT: [number, number] = [0, 100];

// movingAverage replaces each point by the mean of the points within the
// trailing window, per series.
export function movingAverage(series: MetricSeries[], windowMs: number): MetricSeries[] {
  return series.map((entry) => {
    const points = entry.points ?? [];
    let start = 0;
    let sum = 0;
    const out: MetricSeries["points"] = [];
    points.forEach((point, index) => {
      sum += point[1];
      while (points[start][0] < point[0] - windowMs + 1) {
        sum -= points[start][1];
        start += 1;
      }
      out.push([point[0], sum / (index - start + 1)]);
    });
    return { ...entry, points: out };
  });
}
