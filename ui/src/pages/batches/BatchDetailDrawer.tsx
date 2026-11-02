import { useState } from "react";
import { Alert, Button, Descriptions, Drawer, Modal, Space, Spin, Tooltip, Typography, message } from "antd";
import { DownloadOutlined } from "@ant-design/icons";
import { useTranslation } from "react-i18next";
import { usePolling } from "../../api";
import { BATCH_POLL_MS, batchApi, batchFileURL, isBatchActive, type Batch, type BatchFileKind } from "../../batchApi";
import { formatTimeSec } from "../../i18n/datetime";
import { batchErrorText } from "./batchErrors";
import { BatchProgress, BatchStatusTag } from "./batchDisplay";
import { useBatchPolling } from "./useBatchPolling";

const TIME_FIELDS = [
  "created_at", "in_progress_at", "finalizing_at", "completed_at", "failed_at", "expired_at", "cancelling_at", "cancelled_at", "expires_at",
] as const;

/** The file ids by kind; a file that was never produced, or was deleted at the server's output_expires_at, has none to download. */
function fileAvailable(batch: Batch, kind: BatchFileKind, now = Date.now()): { available: boolean; expired: boolean } {
  if (kind === "input") return { available: !!batch.input_file_id, expired: false };
  const id = kind === "output" ? batch.output_file_id : batch.error_file_id;
  const expiresAt = batch.output_expires_at ? new Date(batch.output_expires_at).getTime() : Number.POSITIVE_INFINITY;
  const expired = !!id && now >= expiresAt;
  return { available: !!id && !expired, expired };
}

function DownloadButton({ batch, kind }: { batch: Batch; kind: BatchFileKind }) {
  const { t } = useTranslation("batches");
  const label = t(`detail.download.${kind}`);
  const { available, expired } = fileAvailable(batch, kind);
  // The session cookie authenticates a plain link, as it does for storage file downloads.
  if (available) return <Button icon={<DownloadOutlined />} href={batchFileURL(batch.id, kind)} download>{label}</Button>;
  return (
    <Tooltip title={expired ? t("detail.download.expired") : t("detail.download.unavailable")}>
      <span><Button icon={<DownloadOutlined />} disabled>{label}</Button></span>
    </Tooltip>
  );
}

function Metadata({ metadata }: { metadata: Batch["metadata"] }) {
  const { t } = useTranslation("batches");
  const entries = Object.entries(metadata ?? {});
  if (entries.length === 0) return <Typography.Text type="secondary">{t("detail.noMetadata")}</Typography.Text>;
  return (
    <Descriptions size="small" column={1} bordered items={entries.map(([key, value]) => ({ key, label: key, children: value }))} />
  );
}

function BatchBody({ batch, canManage, onChanged }: { batch: Batch; canManage: boolean; onChanged: () => void }) {
  const { t } = useTranslation("batches");
  const { t: tCommon } = useTranslation("common");
  const [actionError, setActionError] = useState<string | null>(null);
  const [confirming, setConfirming] = useState(false);
  const counts = batch.request_counts;
  const pending = Math.max(0, counts.total - counts.completed - counts.failed);
  const ended = !isBatchActive(batch.status);

  const cancel = async () => {
    setActionError(null);
    try {
      await batchApi.cancel(batch.id);
      setConfirming(false);
      message.success(t("detail.cancelled", { id: batch.id }));
      onChanged();
    } catch (e) {
      setConfirming(false);
      setActionError(t("detail.cancelFailed", { detail: batchErrorText(t, tCommon, e).message }));
      // A 409 means the batch just ended: show what it is now.
      onChanged();
    }
  };

  return (
    <Space direction="vertical" size="large" style={{ width: "100%" }}>
      {actionError && <Alert type="error" showIcon message={actionError} closable onClose={() => setActionError(null)} />}
      {batch.status === "cancelling" && <Alert type="info" showIcon message={t("detail.cancelPending")} />}
      <section aria-label={t("detail.sections.summary")}>
        <Descriptions
          size="small"
          column={1}
          bordered
          title={t("detail.sections.summary")}
          extra={canManage && batch.cancellable && (
            <Button danger onClick={() => setConfirming(true)}>{t("action.cancel")}</Button>
          )}
          items={[
            { key: "id", label: t("detail.id"), children: <Typography.Text copyable code>{batch.id}</Typography.Text> },
            { key: "status", label: t("detail.status"), children: <BatchStatusTag status={batch.status} /> },
            { key: "endpoint", label: t("detail.endpoint"), children: <Typography.Text code>{batch.endpoint}</Typography.Text> },
            ...(batch.model ? [{ key: "model", label: t("detail.model"), children: <Typography.Text code>{batch.model}</Typography.Text> }] : []),
            { key: "window", label: t("detail.completionWindow"), children: batch.completion_window },
            ...(batch.input_filename ? [{ key: "file", label: t("detail.inputFilename"), children: batch.input_filename }] : []),
            { key: "owner", label: t("detail.owner"), children: batch.user_id },
            { key: "apiKey", label: t("detail.apiKey"), children: batch.api_key_id },
          ]}
        />
      </section>
      {batch.failure && (
        <Alert
          type="error"
          showIcon
          message={t("detail.sections.failure")}
          description={<><div>{batch.failure.message}</div><Typography.Text type="secondary">{t("detail.failureCode")}: {batch.failure.code}</Typography.Text></>}
        />
      )}
      <section aria-label={t("detail.sections.counts")}>
        <Descriptions
          size="small"
          column={2}
          bordered
          title={t("detail.sections.counts")}
          items={[
            { key: "total", label: t("detail.total"), children: counts.total, span: 2 },
            { key: "completed", label: t("detail.completed"), children: counts.completed },
            { key: "failed", label: t("detail.failed"), children: counts.failed },
            ...(!ended ? [{ key: "pending", label: <Tooltip title={t("detail.pendingHint")}>{t("detail.pending")}</Tooltip>, children: pending, span: 2 }] : []),
            { key: "progress", label: t("columns.progress"), children: <BatchProgress batch={batch} />, span: 2 },
          ]}
        />
      </section>
      <section aria-label={t("detail.sections.times")}>
        <Descriptions
          size="small"
          column={1}
          bordered
          title={t("detail.sections.times")}
          items={TIME_FIELDS.filter((field) => batch[field]).map((field) => ({
            key: field, label: t(`detail.times.${field}`), children: formatTimeSec(batch[field]),
          }))}
        />
      </section>
      <section aria-label={t("detail.sections.metadata")}>
        <Typography.Title level={5}>{t("detail.sections.metadata")}</Typography.Title>
        <Metadata metadata={batch.metadata} />
      </section>
      <section aria-label={t("detail.sections.files")}>
        <Typography.Title level={5}>{t("detail.sections.files")}</Typography.Title>
        <Space wrap>
          <DownloadButton batch={batch} kind="input" />
          <DownloadButton batch={batch} kind="output" />
          <DownloadButton batch={batch} kind="error" />
        </Space>
        {batch.output_expires_at && (
          <Typography.Paragraph type="secondary" style={{ marginTop: 8 }}>
            {t("detail.download.hint", { time: formatTimeSec(batch.output_expires_at) })}
          </Typography.Paragraph>
        )}
      </section>
      <Modal
        open={confirming}
        title={t("action.cancelTitle")}
        okText={t("action.cancelOk")}
        cancelText={t("action.cancelKeep")}
        okButtonProps={{ danger: true }}
        onOk={cancel}
        onCancel={() => setConfirming(false)}
        destroyOnHidden
      >
        {t("action.cancelBody")}
      </Modal>
    </Space>
  );
}

/**
 * BatchDetailDrawer shows one batch: its figures, every timestamp it reached,
 * metadata, the files to download and, for an admin while it can still be
 * stopped, the cancel action behind an in-page confirmation. It refreshes
 * itself while the batch is moving.
 */
export default function BatchDetailDrawer({ id, canManage, pollMs = BATCH_POLL_MS, onClose, onChanged }: {
  id: string | null;
  canManage: boolean;
  pollMs?: number;
  onClose: () => void;
  /** Called when the batch may have changed, so the list follows. */
  onChanged: () => void;
}) {
  const { t } = useTranslation("batches");
  const detail = usePolling<Batch | null>(() => (id ? batchApi.get(id) : Promise.resolve(null)), 0, [id]);
  const batch = detail.data && detail.data.id === id ? detail.data : null;
  useBatchPolling(!!batch && isBatchActive(batch.status), () => { void detail.refresh(); }, pollMs);

  const changed = () => {
    void detail.refresh();
    onChanged();
  };
  let body;
  if (!id) body = null;
  else if (!batch && detail.error) {
    body = (
      <Alert
        type="error"
        showIcon
        message={t("page.detailLoadFailed")}
        description={detail.error.message}
        action={<Button onClick={() => void detail.refetch()}>{t("action.retry")}</Button>}
      />
    );
  } else if (!batch) body = <div role="status"><Spin /></div>;
  else body = <BatchBody batch={batch} canManage={canManage} onChanged={changed} />;

  return (
    <Drawer open={!!id} width={640} title={id ? t("detail.title", { id }) : null} onClose={onClose} destroyOnHidden>
      {body}
    </Drawer>
  );
}
