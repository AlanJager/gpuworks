import { Progress, Typography } from "antd";
import { useTranslation } from "react-i18next";
import { usedPercent } from "../../storageDisplay";
import { formatBytes } from "../../format";

/**
 * A used/total bar for a volume with a one-line figure under it; a dash when
 * nothing was reported. `compact` keeps it to a table cell.
 */
export default function StorageCapacity({ total, available, compact = false, label }: {
  total?: number;
  available?: number;
  compact?: boolean;
  /** Accessible name of the bar, e.g. which disk it measures. */
  label?: string;
}) {
  const { t } = useTranslation("storage");
  if (!total) return <Typography.Text type="secondary">—</Typography.Text>;
  const percent = usedPercent(total, available);
  const used = total - (available ?? 0);
  return (
    <div style={{ minWidth: compact ? 140 : 240, maxWidth: compact ? 220 : 400 }}>
      <Progress percent={percent} size="small" showInfo={false} aria-label={label} status={percent >= 90 ? "exception" : "normal"} style={{ margin: 0 }} />
      <Typography.Text type="secondary" style={{ fontSize: 12 }}>
        {t("capacity.value", { used: formatBytes(used), total: formatBytes(total), available: formatBytes(available ?? 0) })}
      </Typography.Text>
    </div>
  );
}
