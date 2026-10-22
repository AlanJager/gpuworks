import { Tag, Tooltip } from "antd";
import { useTranslation } from "react-i18next";

import type { Node } from "../api";
import { formatTime } from "../i18n/datetime";

// GPUProbeFailedTag warns that the cards shown are as the last successful
// probe left them, because the latest one failed. It says nothing about the
// node's connection: a node whose driver stopped answering is still
// reachable, and is the one an operator most needs a terminal on.
export function GPUProbeFailedTag({ node }: { node: Pick<Node, "gpu_probe_error" | "gpu_probe_failed_at"> }) {
  const { t } = useTranslation("nodes");
  if (!node.gpu_probe_error) return null;
  const detail = t("gpuProbeFailed.detail", {
    at: formatTime(node.gpu_probe_failed_at),
    detail: node.gpu_probe_error,
  });
  return (
    <Tooltip trigger={["hover", "focus"]} title={detail}>
      <Tag color="orange" tabIndex={0} aria-label={detail} style={{ cursor: "help" }}>
        {t("gpuProbeFailed.tag")}
      </Tag>
    </Tooltip>
  );
}
