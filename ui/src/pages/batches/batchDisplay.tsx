import { Progress, Tag, Typography, theme } from "antd";
import { useTranslation } from "react-i18next";
import type { Batch, BatchStatus } from "../../batchApi";

const STATUS_COLORS: Record<BatchStatus, string> = {
  validating: "default",
  in_progress: "processing",
  finalizing: "processing",
  cancelling: "orange",
  completed: "success",
  failed: "error",
  expired: "warning",
  cancelled: "default",
};

/** BatchStatusTag is a batch's status, coloured by how it ended or that it is still moving. */
export function BatchStatusTag({ status }: { status: string }) {
  const { t } = useTranslation("batches");
  const label = t(`status.${status}`, { defaultValue: status });
  return <Tag color={STATUS_COLORS[status as BatchStatus] ?? "default"}>{label}</Tag>;
}

/** The share of a batch's requests that reached a final result, and how many of those failed. */
export function batchProgress(batch: Pick<Batch, "request_counts">) {
  const { total, completed, failed } = batch.request_counts;
  const done = completed + failed;
  const percent = total > 0 ? Math.min(100, (done / total) * 100) : 0;
  const succeeded = total > 0 ? Math.min(100, (completed / total) * 100) : 0;
  return { total, completed, failed, done, percent, succeeded };
}

/**
 * BatchProgress is the done / total text with a bar. Requests the platform is
 * retrying are in neither count, so they stay in the unfilled part of the
 * bar; the failed share is drawn in the warning colour.
 */
export function BatchProgress({ batch }: { batch: Batch }) {
  const { t } = useTranslation("batches");
  const p = batchProgress(batch);
  const { token } = theme.useToken();
  return (
    <div className="batch-progress">
      <Progress
        size="small"
        showInfo={false}
        percent={p.percent}
        success={{ percent: p.succeeded }}
        strokeColor={token.colorWarning}
        aria-label={t("progress.aria", { done: p.done, total: p.total, failed: p.failed })}
      />
      <div>
        <Typography.Text>{t("progress.summary", { done: p.done, total: p.total })}</Typography.Text>
        {p.failed > 0 && <Typography.Text type="warning" className="batch-progress-failed">{t("progress.failed", { count: p.failed })}</Typography.Text>}
      </div>
    </div>
  );
}
