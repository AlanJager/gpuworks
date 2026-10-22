import { RightOutlined } from "@ant-design/icons";
import { Alert, Descriptions, Empty, Progress, Space, Table, Typography } from "antd";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import type { GPU, Node, TopoHighlight, Topology } from "../../api";
import AcceleratorChoice, { hasAcceleratorChoice } from "../../components/AcceleratorChoice";
import GPUHealthTag from "../../components/GPUHealth";
import PartitionCapability from "../../components/PartitionCapability";
import TopologyGraph from "../../components/TopologyGraph";
import { formatTime } from "../../i18n/datetime";
import { cardHasDetail, cardKey } from "../../nodes/cardIdentity";
import { useGpuTelemetry, type GpuSnapshot } from "../../nodes/useGpuTelemetry";
import { colors } from "../../theme/tokens";
import { formatFramework, formatGPUDriver } from "./nodeColumns";
import NodeDetailSection from "./NodeDetailSection";

const GIB = 1024 ** 3;

// NodeGpuTab is the node detail's GPU tab: what the node's cards run on, the
// card list with each card's live readings first and its other facts in the
// expanded row, then the topology.
export default function NodeGpuTab({
  node,
  topology,
  highlight,
  onUpdated,
}: {
  node: Node;
  topology?: Topology;
  highlight?: TopoHighlight;
  onUpdated: (node: Node) => void;
}) {
  const { t } = useTranslation("nodes");
  const gpus = node.gpus;
  const telemetry = useGpuTelemetry(node.uuid, gpus);
  const numbered = gpus.some((gpu) => gpu.index >= 0);

  return (
    <>
      <NodeDetailSection title={t("accelerators.overview")}>
        <Descriptions bordered size="small" column={1}>
          <Descriptions.Item label={t("detail.gpuDriver")}>{formatGPUDriver(node)}</Descriptions.Item>
          <Descriptions.Item label={t("detail.computeFramework")}>{formatFramework(node)}</Descriptions.Item>
          {hasAcceleratorChoice(node) && (
            <Descriptions.Item label={t("detail.accelerator")}>
              <AcceleratorChoice node={node} onUpdated={onUpdated} />
            </Descriptions.Item>
          )}
          <Descriptions.Item label={t("detail.partitioningCapability")}>
            <Space direction="vertical" size={4}>
              {(node.partition_capabilities ?? []).map((capability) => (
                <PartitionCapability
                  key={`${capability.backend}:${capability.status}:${capability.reason ?? ""}`}
                  capability={capability}
                  compact
                />
              ))}
              {!node.partition_capabilities?.length && (
                <Typography.Text type="secondary">{t("detail.partitioningCapabilityEmpty")}</Typography.Text>
              )}
            </Space>
          </Descriptions.Item>
        </Descriptions>
      </NodeDetailSection>

      <NodeDetailSection title={t("accelerators.list")}>
        {gpus.length === 0 ? (
          <Empty description={t("accelerators.empty")} />
        ) : (
          <>
            {numbered && !telemetry.hasAnyMetric && !telemetry.loading ? (
              <Alert
                className="node-gpu-waiting"
                type="info"
                showIcon
                style={{ marginBottom: 10 }}
                message={t("telemetry.waiting")}
                description={t("telemetry.waitingDescription")}
              />
            ) : (
              <Typography.Paragraph type="secondary" style={{ fontSize: 12, margin: "-4px 0 10px" }}>
                {t("accelerators.listHint")}
              </Typography.Paragraph>
            )}
            <Table<GPU>
              className="node-gpu-table"
              rowKey={cardKey}
              size="small"
              pagination={false}
              scroll={{ x: "max-content" }}
              dataSource={gpus}
              columns={gpuColumns(t, telemetry.snapshotOf)}
              expandable={{
                expandRowByClick: true,
                columnWidth: 36,
                expandIcon: ({ expanded, onExpand, record }) => (
                  <button
                    type="button"
                    className="node-gpu-expand"
                    aria-expanded={expanded}
                    aria-label={t(expanded ? "accelerators.collapse" : "accelerators.expand")}
                    onClick={(event) => onExpand(record, event)}
                  >
                    <RightOutlined />
                  </button>
                ),
                expandedRowRender: (gpu) => <GpuExtra gpu={gpu} snapshot={telemetry.snapshotOf(gpu)} />,
              }}
            />
          </>
        )}
      </NodeDetailSection>

      {gpus.length > 0 && (
        <NodeDetailSection title={t("accelerators.topology")}>
          <TopologyGraph topology={topology} highlight={highlight} gpus={gpus} />
        </NodeDetailSection>
      )}
    </>
  );
}

function gpuColumns(t: TFunction<"nodes">, snapshotOf: (gpu: GPU) => GpuSnapshot) {
  return [
    {
      // A card the vendor CLI does not list has no index of its own; its
      // PCI address is what names it.
      title: "#",
      dataIndex: "index",
      width: 32,
      render: (index: number) => (index < 0 ? "—" : index),
    },
    {
      title: t("accelerators.column.model"),
      dataIndex: "name",
      render: (name: string, gpu: GPU) =>
        cardHasDetail(gpu) ? (
          <Link to={`/gpus?gpu=${encodeURIComponent(gpu.uuid)}`} onClick={(event) => event.stopPropagation()}>
            {name}
          </Link>
        ) : (
          // Nothing is known about this card beyond what PCI says, so
          // there is no page about it to open. The address goes under the
          // name: beside it, it widened the column past the drawer.
          <div className="node-gpu-model">
            <span>{name}</span>
            {gpu.pcie_bdf && <Typography.Text type="secondary">{gpu.pcie_bdf}</Typography.Text>}
          </div>
        ),
    },
    {
      title: t("accelerators.column.vram"),
      key: "vram",
      width: 112,
      render: (_: unknown, gpu: GPU) => {
        const used = snapshotOf(gpu).memoryUsedBytes;
        const total = gpu.memory_total_bytes > 0 ? `${(gpu.memory_total_bytes / GIB).toFixed(0)} GiB` : "—";
        return (
          <div className="node-gpu-vram">
            <span className="node-gpu-num">{used == null ? "—" : (used / GIB).toFixed(1)} / {total}</span>
            {used != null && gpu.memory_total_bytes > 0 && (
              <Progress
                percent={Math.min(100, (used / gpu.memory_total_bytes) * 100)}
                size="small"
                showInfo={false}
                strokeColor={colors.chartPrimary}
                trailColor={colors.borderSoft}
              />
            )}
          </div>
        );
      },
    },
    {
      title: t("telemetry.utilization"),
      key: "utilization",
      width: 92,
      render: (_: unknown, gpu: GPU) => {
        const value = snapshotOf(gpu).utilization;
        if (value == null) return "—";
        const percent = Math.max(0, Math.min(100, value));
        return (
          <div className="node-gpu-meter">
            <Progress percent={percent} size="small" showInfo={false} strokeColor={colors.chartPrimary} trailColor={colors.borderSoft} />
            <span className="node-gpu-num">{percent.toFixed(0)}%</span>
          </div>
        );
      },
    },
    {
      title: t("telemetry.temperature"),
      key: "temperature",
      width: 52,
      render: (_: unknown, gpu: GPU) => {
        const value = snapshotOf(gpu).temperatureC;
        return <span className="node-gpu-num">{value == null ? "—" : `${value.toFixed(0)}°C`}</span>;
      },
    },
    {
      title: t("telemetry.power"),
      key: "power",
      width: 54,
      // Too narrow a window drops this column; the expanded row keeps it.
      responsive: ["lg" as const],
      render: (_: unknown, gpu: GPU) => {
        const value = snapshotOf(gpu).powerWatts;
        return <span className="node-gpu-num">{value == null ? "—" : `${value.toFixed(0)} W`}</span>;
      },
    },
    {
      title: t("accelerators.column.health"),
      key: "health",
      width: 122,
      render: (_: unknown, gpu: GPU) => <GPUHealthTag gpu={gpu} />,
    },
  ];
}

function GpuExtra({ gpu, snapshot }: { gpu: GPU; snapshot: GpuSnapshot }) {
  const { t } = useTranslation("nodes");
  const arch = gpu.arch_id || gpu.arch_family || "—";
  const power = snapshot.powerWatts == null ? "—" : `${snapshot.powerWatts.toFixed(0)} W`;
  return (
    <dl className="node-gpu-extra">
      <div>
        <dt>{t("accelerators.column.architecture")}</dt>
        <dd>
          {gpu.vendor ? `${gpu.vendor} · ` : ""}
          {arch}
          {gpu.compute_capability ? ` (cc ${gpu.compute_capability})` : ""}
        </dd>
      </div>
      <div>
        <dt>NUMA</dt>
        <dd>{gpu.numa_node < 0 ? "—" : gpu.numa_node}</dd>
      </div>
      <div>
        <dt>{t("accelerators.field.pcie")}</dt>
        <dd>{gpu.pcie_bdf || "—"}</dd>
      </div>
      <div>
        <dt>{t("accelerators.column.partitioning")}</dt>
        <dd>
          {gpu.partition_capabilities?.length ? (
            <Space direction="vertical" size={2}>
              {gpu.partition_capabilities.map((capability) => (
                <PartitionCapability key={capability.backend} capability={capability} compact />
              ))}
            </Space>
          ) : (
            "—"
          )}
        </dd>
      </div>
      <div>
        <dt>{t("telemetry.power")}</dt>
        <dd>{gpu.power_limit_watts ? `${power} / ${gpu.power_limit_watts.toFixed(0)} W` : power}</dd>
      </div>
      <div>
        <dt>{t("accelerators.field.updatedAt")}</dt>
        <dd>{formatTime(snapshot.updatedAtMS, t("telemetry.notReported"))}</dd>
      </div>
      <div style={{ gridColumn: "1 / -1" }}>
        <dt>{t("accelerators.field.uuid")}</dt>
        <dd>{gpu.uuid ? <Typography.Text copyable style={{ fontSize: 12 }}>{gpu.uuid}</Typography.Text> : "—"}</dd>
      </div>
    </dl>
  );
}
