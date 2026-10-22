import { Empty, Typography } from "antd";
import { useTranslation } from "react-i18next";
import type { Instance } from "../api";
import InferenceMetricsPanel from "../components/InferenceMetricsPanel";
import KVTierPanel from "../components/KVTierPanel";
import InstanceGpuStrip from "../components/InstanceGpuStrip";
import InstanceStagePanel from "../components/InstanceStagePanel";

// The metrics tab: the existing per-request inference panel, the KV offload
// tiers, a per-GPU telemetry strip scoped to this instance's own GPUs, and
// the stage/preemption cards fed by the deployment monitor snapshot.
// VRAMBreakdown stays where it already rendered, above the tabs, since it
// compares an estimate the deployment made once against a live measurement -
// not a live series.
export default function InstanceMetricsTab({ inst, params }: { inst: Instance; params?: Record<string, unknown> }) {
  const { t } = useTranslation("deployments");
  const live = inst.state === "running" || inst.state === "starting" || inst.state === "optimizing";
  if (!live) return <Empty description={t("instance.noMetrics")} />;
  return (
    <div>
      <InferenceMetricsPanel instanceUuid={inst.uuid} />
      <KVTierPanel params={params} instanceUuid={inst.uuid} />
      <div style={{ marginTop: 12 }}>
        <Typography.Text strong style={{ display: "block", marginBottom: 6 }}>{t("instanceGpu.title")}</Typography.Text>
        <InstanceGpuStrip nodeUuid={inst.node_uuid} gpuIndexes={inst.gpu_indexes ?? []} instanceUuid={inst.uuid} vramLimitBytes={inst.vram_limit_bytes} />
      </div>
      <div style={{ marginTop: 12 }}>
        <InstanceStagePanel instanceUuid={inst.uuid} deploymentId={inst.deployment_id} />
      </div>
    </div>
  );
}
