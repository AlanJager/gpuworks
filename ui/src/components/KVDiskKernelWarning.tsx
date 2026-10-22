import { ExclamationCircleFilled } from "@ant-design/icons";
import { Tooltip } from "antd";
import { useTranslation } from "react-i18next";
import type { EngineProfile } from "../api";
import { kvDiskKernelShortfall } from "../kernelSupport";

// KVDiskKernelWarning marks a node whose kernel is below the KV disk tier's
// floor, which the engines state on /engines. Nothing is rendered when the node
// clears the floor or when its kernel string cannot be read, so the mark only
// ever appears on a node that really cannot run the tier.
export default function KVDiskKernelWarning({ kernel, engines }: { kernel?: string; engines?: readonly EngineProfile[] | null }) {
  const { t } = useTranslation("nodes");
  const shortfall = kvDiskKernelShortfall(kernel, engines);
  if (!shortfall) return null;
  const hint = t("detail.kvDiskKernelTooOld", { required: shortfall.required, current: shortfall.current });
  return (
    <Tooltip title={hint}>
      <ExclamationCircleFilled role="img" aria-label={hint} style={{ color: "#faad14", marginLeft: 6 }} />
    </Tooltip>
  );
}
