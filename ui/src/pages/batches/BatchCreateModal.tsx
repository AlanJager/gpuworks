import { useEffect, useMemo, useState } from "react";
import { Alert, Button, InputNumber, Modal, Select, Typography } from "antd";
import { useTranslation } from "react-i18next";
import { api } from "../../api";
import { BATCH_ENDPOINTS, BATCH_LIMITS, BATCH_WINDOW_HOURS, batchApi, type Batch, type BatchEndpoint } from "../../batchApi";
import { batchErrorText, type BatchErrorText } from "./batchErrors";
import { batchSampleJSONL, downloadBatchSample, sampleFileName } from "./batchSample";
import { batchModelOptions, modelsInFile, FILE_MODEL_SCAN_MAX_BYTES, type BatchModelOption } from "./batchModels";
import BatchFileModels from "./BatchFileModels";
import BatchModelField, { useModelStateText } from "./BatchModelField";

export const BATCH_FILE_ACCEPT = ".jsonl";

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
  // The models the platform serves: the batch's model to choose, and the
  // sample's, so it runs as downloaded. null when the list could not be read.
  const [options, setOptions] = useState<BatchModelOption[] | null>(null);
  const [model, setModel] = useState<string | undefined>(undefined);
  const [windowHours, setWindowHours] = useState<number | null>(BATCH_WINDOW_HOURS.default);
  const windowValid = windowHours !== null && Number.isInteger(windowHours)
    && windowHours >= BATCH_WINDOW_HOURS.min && windowHours <= BATCH_WINDOW_HOURS.max;
  const [sampleModel, setSampleModel] = useState<string>("");
  useEffect(() => {
    if (!open) return;
    let live = true;
    api.deployments().then((list) => {
      if (!live) return;
      const found = batchModelOptions(list.items);
      setOptions(found);
      setSampleModel((current) => current || found.find((option) => option.state === "running")?.name || found[0]?.name || "");
    }).catch(() => { /* the sample still works with a model typed in later */ });
    return () => { live = false; };
  }, [open]);
  const modelOptions = useMemo(() => options ?? [], [options]);
  const known = useMemo(() => (options ? new Set(options.map((option) => option.name)) : null), [options]);
  const stateText = useModelStateText();
  // The models the chosen file names, read here when it is small enough.
  const [fileModels, setFileModels] = useState<string[] | null>(null);
  const skipped = !!file && file.size > FILE_MODEL_SCAN_MAX_BYTES;
  useEffect(() => {
    setFileModels(null);
    if (!file || file.size > FILE_MODEL_SCAN_MAX_BYTES) return;
    let live = true;
    void modelsInFile(file).then((found) => { if (live) setFileModels(found); });
    return () => { live = false; };
  }, [file]);
  const sampleModelName = model || sampleModel || "your-model";
  const useSample = () => {
    const ep = endpoint as BatchEndpoint;
    pick(new File([batchSampleJSONL(ep, sampleModelName)], sampleFileName(ep), { type: "application/jsonl" }));
  };

  const reset = () => {
    setFile(null);
    setError(null);
    setEndpoint(BATCH_ENDPOINTS[0]);
    setModel(undefined);
    setWindowHours(BATCH_WINDOW_HOURS.default);
  };
  const pick = (picked: File | null) => {
    setFile(picked);
    setError(null);
  };
  const submit = async () => {
    if (!file || tooLarge || !windowValid) return;
    setSubmitting(true);
    setError(null);
    try {
      const batch = await batchApi.create(file, endpoint, { model, completionWindowHours: windowHours ?? undefined });
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
        <Button key="submit" type="primary" onClick={() => void submit()} loading={submitting} disabled={!file || tooLarge || !windowValid}>
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
          <BatchFileModels fileModels={fileModels} skipped={skipped && !tooLarge} known={known} override={model} />
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
        <div className="batch-create-field">
          <label htmlFor="batch-create-window">{t("create.window.label")}</label>
          <InputNumber
            id="batch-create-window"
            min={BATCH_WINDOW_HOURS.min}
            max={BATCH_WINDOW_HOURS.max}
            precision={0}
            value={windowHours}
            onChange={setWindowHours}
            disabled={submitting}
            addonAfter={t("create.window.unit")}
            aria-describedby="batch-create-window-help"
            status={windowValid ? undefined : "error"}
            style={{ width: 160 }}
          />
          <Typography.Text id="batch-create-window-help" type={windowValid ? "secondary" : "danger"}>
            {t("create.window.help", { min: BATCH_WINDOW_HOURS.min, max: BATCH_WINDOW_HOURS.max, default: BATCH_WINDOW_HOURS.default })}
          </Typography.Text>
        </div>
        <BatchModelField options={modelOptions} value={model} onChange={setModel} disabled={submitting} />
        <div className="batch-create-field">
          <label htmlFor="batch-create-sample-model">{t("create.sample.title")}</label>
          <div className="batch-create-sample">
            <Select
              id="batch-create-sample-model"
              aria-label={t("create.sample.model")}
              value={model || sampleModel || undefined}
              placeholder={t("create.sample.noModel")}
              onChange={setSampleModel}
              disabled={submitting || !!model || modelOptions.length === 0}
              options={modelOptions.map((option) => ({ value: option.name, label: `${option.name} · ${stateText(option.state)}` }))}
              style={{ minWidth: 200, flex: 1 }}
            />
            <Button onClick={() => downloadBatchSample(endpoint as BatchEndpoint, sampleModelName)} disabled={submitting}>
              {t("create.sample.download")}
            </Button>
            <Button onClick={useSample} disabled={submitting}>{t("create.sample.use")}</Button>
          </div>
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
