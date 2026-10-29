import { useState } from "react";
import { Alert, Button, Modal, Select, Typography } from "antd";
import { useTranslation } from "react-i18next";
import { BATCH_ENDPOINTS, BATCH_LIMITS, batchApi, type Batch } from "../../batchApi";
import { batchErrorText, type BatchErrorText } from "./batchErrors";

export const BATCH_FILE_ACCEPT = ".jsonl,.json,.txt";

function sizeText(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

/**
 * BatchCreateModal uploads a JSONL requests file with the endpoint it targets.
 * An invalid file is answered with the line to fix; the dialog stays open so
 * the file can be replaced, and closes once the batch exists.
 */
export default function BatchCreateModal({ open, onClose, onCreated }: {
  open: boolean;
  onClose: () => void;
  onCreated: (batch: Batch) => void;
}) {
  const { t } = useTranslation("batches");
  const { t: tCommon } = useTranslation("common");
  const [file, setFile] = useState<File | null>(null);
  const [endpoint, setEndpoint] = useState<string>(BATCH_ENDPOINTS[0]);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<BatchErrorText | null>(null);
  const tooLarge = !!file && file.size > BATCH_LIMITS.maxFileMB * 1024 * 1024;

  const reset = () => {
    setFile(null);
    setError(null);
    setEndpoint(BATCH_ENDPOINTS[0]);
  };
  const pick = (picked: File | null) => {
    setFile(picked);
    setError(null);
  };
  const submit = async () => {
    if (!file || tooLarge) return;
    setSubmitting(true);
    setError(null);
    try {
      const batch = await batchApi.create(file, endpoint);
      reset();
      onCreated(batch);
    } catch (e) {
      setError(batchErrorText(t, tCommon, e));
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <Modal
      open={open}
      title={t("create.title")}
      onCancel={onClose}
      afterClose={reset}
      maskClosable={false}
      keyboard={!submitting}
      closable={!submitting}
      destroyOnHidden
      footer={[
        <Button key="close" onClick={onClose} disabled={submitting}>{t("create.cancel")}</Button>,
        <Button key="submit" type="primary" onClick={() => void submit()} loading={submitting} disabled={!file || tooLarge}>
          {submitting ? t("create.submitting") : t("create.submit")}
        </Button>,
      ]}
    >
      <div className="batch-create-form">
        <div className="batch-create-field">
          <label htmlFor="batch-create-file">{t("create.file")}</label>
          <input
            id="batch-create-file"
            type="file"
            accept={BATCH_FILE_ACCEPT}
            disabled={submitting}
            aria-describedby="batch-create-file-help"
            onChange={(event) => pick(event.target.files?.[0] ?? null)}
          />
          <Typography.Text id="batch-create-file-help" type="secondary">
            {file ? t("create.fileSelected", { name: file.name, size: sizeText(file.size) }) : t("create.fileHelp")}
          </Typography.Text>
        </div>
        <div className="batch-create-field">
          <label htmlFor="batch-create-endpoint">{t("create.endpoint")}</label>
          <Select
            id="batch-create-endpoint"
            value={endpoint}
            onChange={setEndpoint}
            disabled={submitting}
            options={BATCH_ENDPOINTS.map((value) => ({ value, label: value }))}
            style={{ width: "100%" }}
          />
        </div>
        <Alert
          type="info"
          showIcon
          message={t("create.hintTitle")}
          description={
            <ul className="batch-create-hint">
              <li>{t("create.hint.format", { size: BATCH_LIMITS.maxFileMB, requests: BATCH_LIMITS.maxRequests })}</li>
              <li>{t("create.hint.line")}</li>
              <li>{t("create.hint.unique")}</li>
              <li>{t("create.hint.sameUrl")}</li>
              <li>{t("create.hint.noStream")}</li>
              <li>{t("create.hint.window")}</li>
            </ul>
          }
        />
        {tooLarge && <Alert type="error" showIcon message={t("create.tooLarge", { limit: BATCH_LIMITS.maxFileMB })} />}
        {error && (
          <Alert
            type="error"
            showIcon
            message={t("create.failed")}
            description={<>
              <div>{error.message}</div>
              {error.detail && <Typography.Text type="secondary">{t("errors.detail", { detail: error.detail })}</Typography.Text>}
            </>}
          />
        )}
      </div>
    </Modal>
  );
}
