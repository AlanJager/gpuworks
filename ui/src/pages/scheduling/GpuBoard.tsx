import { Alert, Empty, Space, Tag, Typography } from "antd";
import { WarningOutlined } from "@ant-design/icons";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import type { BoardGPU, BoardNode, BoardOccupant } from "../../schedulingApi";
import { formatBytes } from "../../format";

// Legend explains the three parts of every GPU bar once for the whole board.
export function Legend() {
  const { t } = useTranslation("scheduling");
  return (
    <ul className="scheduling-legend" aria-label={t("nodes.legend.title")}>
      {(["awake", "sleeping", "free"] as const).map((part) => (
        <li key={part}>
          <span className={`scheduling-swatch is-${part}`} aria-hidden="true" />
          {t(`nodes.legend.${part}`)}
        </li>
      ))}
    </ul>
  );
}

// NodeList is one page of nodes, each with its GPUs drawn as who holds the
// card's memory.
export function NodeList({ nodes }: { nodes: BoardNode[] }) {
  return (
    <div className="scheduling-nodes">
      {nodes.map((node) => <NodeCard key={node.uuid} node={node} />)}
    </div>
  );
}

function NodeCard({ node }: { node: BoardNode }) {
  const { t } = useTranslation("scheduling");
  return (
    <section className="scheduling-node" aria-label={node.name}>
      <Space size={8} align="center">
        <Typography.Text strong>{node.name}</Typography.Text>
        <Tag color={node.online ? "green" : "default"}>{t(node.online ? "nodes.online" : "nodes.offline")}</Tag>
      </Space>
      {node.gpus.length === 0 && <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description={t("nodes.noGpus")} />}
      {node.gpus.map((gpu) => <GpuRow key={gpu.index} node={node} gpu={gpu} />)}
    </section>
  );
}

/** The tag colour of a card state; a state the page does not know is plain. */
const STATE_TAG: Record<string, string | undefined> = {
  awake: "blue", asleep: "purple", free: "green", held_outside: "orange", unhealthy: "red", offline: "default",
};

function GpuRow({ node, gpu }: { node: BoardNode; gpu: BoardGPU }) {
  const { t } = useTranslation("scheduling");
  const total = Math.max(gpu.memory_bytes, 1);
  const part = (bytes: number) => `${Math.min(100, Math.max(0, (bytes / total) * 100))}%`;
  const size = (bytes: number) => formatBytes(bytes);
  return (
    <div className="scheduling-gpu">
      <div className="scheduling-gpu-head">
        <Space size={6} align="center">
          <Typography.Text>{t("nodes.gpuLabel", { index: gpu.index, name: gpu.name })}</Typography.Text>
          {/* The server's state of the card, the one its heatmap cell shows. */}
          <Tag className={`scheduling-gpu-state is-${gpu.state}`} color={STATE_TAG[gpu.state]}>{t(`states.${gpu.state}`)}</Tag>
        </Space>
        <Typography.Text type="secondary" className="scheduling-num">
          {t("nodes.figures", { awake: size(gpu.awake_bytes), sleeping: size(gpu.sleeping_bytes), free: size(gpu.free_bytes), total: size(gpu.memory_bytes) })}
        </Typography.Text>
      </div>
      <div
        className="scheduling-bar"
        role="img"
        aria-label={t("nodes.barAria", { node: node.name, index: gpu.index, awake: size(gpu.awake_bytes), sleeping: size(gpu.sleeping_bytes), free: size(gpu.free_bytes), total: size(gpu.memory_bytes) })}
      >
        <span className="is-awake" style={{ width: part(gpu.awake_bytes) }} />
        <span className="is-sleeping" style={{ width: part(gpu.sleeping_bytes) }} />
        <span className="is-free" style={{ width: part(gpu.free_bytes) }} />
      </div>
      {gpu.held_outside && (
        <Alert
          className="scheduling-held"
          type="warning"
          showIcon
          icon={<WarningOutlined />}
          message={t("nodes.heldOutside")}
          description={t("nodes.heldOutsideHint")}
        />
      )}
      {gpu.occupants.length > 0 && (
        <Space size={[4, 4]} wrap className="scheduling-occupants">
          {gpu.occupants.map((occupant) => <OccupantChip key={occupant.instance} occupant={occupant} />)}
        </Space>
      )}
    </div>
  );
}

// OccupantChip names one instance on a card: its deployment (a link) and the
// yield policy it shares the card at, whether
// it is awake or asleep, and the memory it holds - the claim while awake, the
// residue while asleep.
function OccupantChip({ occupant }: { occupant: BoardOccupant }) {
  const { t } = useTranslation("scheduling");
  const { t: tDeploy } = useTranslation("deployments");
  const asleep = occupant.state === "asleep";
  return (
    <Tag className={`scheduling-occupant${asleep ? " is-asleep" : ""}`} color={asleep ? "purple" : "blue"}>
      <Link to={`/deployments?deployment=${occupant.deployment_id}`}>{occupant.deployment}</Link>
      <span className="scheduling-level">{t("nodes.occupant.level", { level: tDeploy(`yieldPolicy.policy.name.${occupant.yield_policy ?? "never"}`) })}</span>
      <span className="scheduling-num">
        {" · "}
        {asleep
          ? t("nodes.occupant.asleep", { level: occupant.sleep_level ?? "", residue: formatBytes(occupant.residue_bytes ?? 0) })
          : t("nodes.occupant.awake", { claim: formatBytes(occupant.claim_bytes) })}
      </span>
    </Tag>
  );
}
