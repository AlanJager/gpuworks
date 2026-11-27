import { useTranslation } from "react-i18next";
import { Progress } from "../../ui-kit/Progress";
import type { AgentUpgradeCounts } from "../../agentUpgradesTypes";

const resultCounts = ["succeeded", "failed", "skipped", "cancelled", "interrupted", "unconfirmed", "queued", "active"] as const;

export default function UpgradeSummary({ counts }: { counts: AgentUpgradeCounts }) {
  const { t } = useTranslation("nodes");
  const issues = counts.failed + counts.interrupted + counts.unconfirmed;
  const processed = t("batchUpgrade.processed", { processed: counts.processed, total: counts.total });
  return <div className="flex w-full flex-col gap-1">
    <strong className="font-semibold text-fg-default">{processed}</strong>
    {/* Failed, interrupted or unknown attempts turn the bar red, as the record's outcome says. */}
    <Progress value={counts.processed} max={counts.total} label={processed} tone={issues > 0 ? "danger" : "brand"} />
    <span className="text-fg-default">{resultCounts.map((key) => t(`batchUpgrade.result.${key}`, { value: counts[key] })).join(" · ")}</span>
    {issues > 0 && <span className="text-fg-warning">{t("batchUpgrade.processingNotSuccess")}</span>}
  </div>;
}
