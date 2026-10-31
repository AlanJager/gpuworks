import { useEffect, useMemo, useState, type ReactNode } from "react";
import { Alert, Button, InputNumber, Modal, Select, Spin, Typography } from "antd";
import { useTranslation } from "react-i18next";
import { batchApi, type Batch, type BatchDictionary, type BatchEndpoint } from "../../batchApi";
import { batchErrorText, type BatchErrorText } from "./batchErrors";
import { batchSampleJSONL, downloadBatchSample, sampleFileName } from "./batchSample";
import { modelsInFile, FILE_MODEL_SCAN_MAX_BYTES } from "./batchModels";
import BatchFileModels from "./BatchFileModels";
import BatchModelField, { useModelStateText } from "./BatchModelField";

export const BATCH_FILE_ACCEPT = ".jsonl";

const MB = 1024 * 1024;

function sizeText(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < MB) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / MB).toFixed(1)} MB`;
}

/**
 * useDictionary reads what a batch can be created with each time the dialog
 * opens, so the models' states are the server's of now; retry reads it again.
 */
function useDictionary(open: boolean) {
  const [dictionary, setDictionary] = useState<BatchDictionary | null>(null);
  const [error, setError] = useState<Error | null>(null);
  const [attempt, setAttempt] = useState(0);
  useEffect(() => {
    if (!open) return;
    let live = true;
    setError(null);
    batchApi.dictionary().then(
      (found) => { if (live) setDictionary(found); },
      (e: unknown) => { if (live) setError(e instanceof Error ? e : new Error(String(e))); },
    );
    return () => { live = false; };
  }, [open, attempt]);
  return { dictionary, error, retry: () => setAttempt((n) => n + 1) };
}

/**
 * BatchCreateModal uploads a JSONL requests file with the endpoint it targets.
 * What it offers - the endpoints, the completion window, the file limits and
 * the models - is the server's batch dictionary. An invalid file is answered
 * with the line to fix; the dialog stays open so the file can be replaced, and
 * closes once the batch exists.
 */
export default function BatchCreateModal({ open, onClose, onCreated }: {
  open: boolean;
  onClose: () => void;
  onCreated: (batch: Batch) => void;
}) {
  const { t } = useTranslation("batches");
  const { t: tCommon } = useTranslation("common");
  const { dictionary, error: dictionaryError, retry } = useDictionary(open);
  const [file, setFile] = useState<File | null>(null);
  // The endpoint and the window the reader chose; until then the dictionary's first and default.
  const [chosenEndpoint, setEndpoint] = useState<string | undefined>(undefined);
  const [chosenWindow, setWindowHours] = useState<number | null | undefined>(undefined);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<BatchErrorText | null>(null);
  const [model, setModel] = useState<string | undefined>(undefined);
  const [sampleModel, setSampleModel] = useState<string>("");
  const endpoint = chosenEndpoint ?? dictionary?.endpoints[0] ?? "";
  const bounds = dictionary?.completion_window_hours;
  const windowHours = chosenWindow === undefined ? (bounds?.default ?? null) : chosenWindow;
  const windowValid = !!bounds && windowHours !== null && Number.isInteger(windowHours) && windowHours >= bounds.min && windowHours <= bounds.max;
  const maxFileMB = dictionary ? Math.floor(dictionary.max_file_bytes / MB) : 0;
  const tooLarge = !!file && !!dictionary && file.size > dictionary.max_file_bytes;
  // The models the platform serves, as the server judges them for a batch: the
  // batch's model to choose, and the sample's, so it runs as downloaded.
  const modelOptions = useMemo(() => dictionary?.models ?? [], [dictionary]);
  const known = useMemo(() => (dictionary ? new Set(dictionary.models.map((option) => option.name)) : null), [dictionary]);
  const stateText = useModelStateText();
  const defaultSample = modelOptions.find((option) => option.state === "served")?.name ?? modelOptions[0]?.name ?? "";
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
  const sampleModelName = model || sampleModel || defaultSample || "your-model";
  const useSample = () => {
    const ep = endpoint as BatchEndpoint;
    pick(new File([batchSampleJSONL(ep, sampleModelName)], sampleFileName(ep), { type: "application/jsonl" }));
  };

  const reset = () => {
    setFile(null);
    setError(null);
    setEndpoint(undefined);
    setModel(undefined);
    setWindowHours(undefined);
  };
  const pick = (picked: File | null) => {
    setFile(picked);
    setError(null);
  };
  const submit = async () => {
    if (!dictionary || !file || tooLarge || !windowValid) return;
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

  let body: ReactNode;
  if (!dictionary && dictionaryError) {
    body = (
      <Alert
        type="error"
        showIcon
        message={t("create.dictionary.failed")}
        description={dictionaryError.message}
        action={<Button size="small" onClick={retry}>{t("create.dictionary.retry")}</Button>}
      />
    );
  } else if (!dictionary || !bounds) {
    body = <div role="status" className="batch-create-loading"><Spin size="small" /> <span>{t("create.dictionary.loading")}</span></div>;
  } else {
    body = (
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
            options={dictionary.endpoints.map((value) => ({ value, label: value }))}
            style={{ width: "100%" }}
          />
        </div>
        <div className="batch-create-field">
          <label htmlFor="batch-create-window">{t("create.window.label")}</label>
          <InputNumber
            id="batch-create-window"
            min={bounds.min}
            max={bounds.max}
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
            {t("create.window.help", { min: bounds.min, max: bounds.max, default: bounds.default })}
          </Typography.Text>
        </div>
        <BatchModelField options={modelOptions} value={model} onChange={setModel} disabled={submitting} />
        <div className="batch-create-field">
          <label htmlFor="batch-create-sample-model">{t("create.sample.title")}</label>
          <div className="batch-create-sample">
            <Select
              id="batch-create-sample-model"
              aria-label={t("create.sample.model")}
              value={model || sampleModel || defaultSample || undefined}
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
              <li>{t("create.hint.format", { size: maxFileMB, requests: dictionary.max_file_lines })}</li>
              <li>{t("create.hint.line")}</li>
              <li>{t("create.hint.unique")}</li>
              <li>{t("create.hint.sameUrl")}</li>
              <li>{t("create.hint.noStream")}</li>
              <li>{t("create.hint.retention", { days: dictionary.output_retention_days })}</li>
            </ul>
          }
        />
        {tooLarge && <Alert type="error" showIcon message={t("create.tooLarge", { limit: maxFileMB })} />}
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
    );
  }

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
        <Button key="submit" type="primary" onClick={() => void submit()} loading={submitting} disabled={!dictionary || !file || tooLarge || !windowValid}>
          {submitting ? t("create.submitting") : t("create.submit")}
        </Button>,
      ]}
    >
      {body}
    </Modal>
  );
}
