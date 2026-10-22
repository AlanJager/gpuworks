import { useEffect, useRef, useState } from "react";
import { Alert, Button, Checkbox, Form, Input, Modal, Progress, Space, Typography, Upload, message } from "antd";
import { InboxOutlined } from "@ant-design/icons";
import { useTranslation } from "react-i18next";
import { APIError } from "../../apiError";
import { formatBytes } from "../../format";
import { storageErrorText } from "../../storageDisplay";
import {
  archiveFormatOf, archiveStem, joinStorageRel, validEntryName, type StorageUploadResult,
} from "../../storageTypes";
import { isUploadAbort, StorageUploadNetworkError, uploadStorageFile, type StorageUploadProgress } from "../../storageUpload";

type Phase = "idle" | "uploading" | "exists" | "failed";

/**
 * Uploads one file into dir in a single request, with a progress bar and a
 * cancel button; an archive can be unpacked into a new directory instead. A
 * target that exists is reported and can be overwritten. Closing the dialog,
 * or the page taking it away (leaving the tab, another node), cancels a
 * running upload.
 */
export default function StorageUploadDialog({ storageId, node, dir, onClose, onUploaded }: {
  storageId: number;
  node?: string;
  dir: string;
  onClose: () => void;
  onUploaded: (result: StorageUploadResult) => void;
}) {
  const { t } = useTranslation("storage");
  const [file, setFile] = useState<File | null>(null);
  const [name, setName] = useState("");
  const [nameEdited, setNameEdited] = useState(false);
  const [extract, setExtract] = useState(false);
  const [phase, setPhase] = useState<Phase>("idle");
  const [progress, setProgress] = useState<StorageUploadProgress | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [cancelled, setCancelled] = useState(false);
  const controller = useRef<AbortController | null>(null);
  useEffect(() => () => controller.current?.abort(), []);

  const format = file ? archiveFormatOf(file.name) : undefined;
  const unpack = extract && !!format;
  const suggested = file ? (unpack ? archiveStem(file.name) : file.name) : "";
  const targetName = (nameEdited ? name : suggested).trim();
  const nameValid = validEntryName(targetName);
  const target = joinStorageRel(dir, targetName);
  const uploading = phase === "uploading";
  const shownDir = dir ? `/${dir}` : "/";

  const pick = (picked: File) => {
    setFile(picked);
    setExtract(false);
    setNameEdited(false);
    setPhase("idle");
    setError(null);
    setCancelled(false);
    setProgress(null);
  };

  const start = async (overwrite: boolean) => {
    if (!file || !nameValid || uploading) return;
    const abort = new AbortController();
    controller.current = abort;
    setPhase("uploading");
    setError(null);
    setCancelled(false);
    setProgress({ loaded: 0, total: file.size });
    try {
      const result = await uploadStorageFile({
        storageId, node, path: target, file, extract: unpack ? format : "", overwrite, signal: abort.signal, onProgress: setProgress,
      });
      controller.current = null;
      message.success(unpack ? t("browse.uploadDialog.doneFiles", { count: result.files, path: result.path }) : t("browse.uploadDialog.done", { path: result.path }));
      onUploaded(result);
    } catch (e) {
      controller.current = null;
      if (isUploadAbort(e)) {
        setPhase("idle");
        setCancelled(true);
        setProgress(null);
      } else if (e instanceof APIError && e.code === "STORAGE_FILE_EXISTS") {
        setPhase("exists");
      } else {
        setPhase("failed");
        setError(e instanceof StorageUploadNetworkError ? t("browse.uploadDialog.networkError") : storageErrorText(t, e));
      }
    }
  };

  const cancel = () => controller.current?.abort();
  const close = () => {
    cancel();
    onClose();
  };

  const percent = progress && progress.total > 0 ? Math.floor((progress.loaded / progress.total) * 100) : 0;
  const footer = uploading
    ? [<Button key="cancel" danger onClick={cancel}>{t("browse.uploadDialog.cancelUpload")}</Button>]
    : phase === "exists"
      ? [
        <Button key="close" onClick={close}>{t("browse.uploadDialog.close")}</Button>,
        <Button key="overwrite" type="primary" danger onClick={() => void start(true)}>{t("browse.uploadDialog.overwrite")}</Button>,
      ]
      : [
        <Button key="close" onClick={close}>{t("browse.uploadDialog.close")}</Button>,
        <Button key="start" type="primary" disabled={!file || !nameValid} onClick={() => void start(false)}>{t("browse.uploadDialog.start")}</Button>,
      ];

  return (
    <Modal open title={t("browse.uploadDialog.title", { path: shownDir })} onCancel={close} maskClosable={!uploading} footer={footer}>
      <Space direction="vertical" size={12} style={{ width: "100%" }}>
        <Upload.Dragger
          beforeUpload={(picked) => { pick(picked); return false; }}
          showUploadList={false}
          multiple={false}
          disabled={uploading}
        >
          <p className="ant-upload-drag-icon"><InboxOutlined /></p>
          <p className="ant-upload-text">{t("browse.uploadDialog.pick")}</p>
          <p className="ant-upload-hint">{t("browse.uploadDialog.pickHint")}</p>
        </Upload.Dragger>
        {file && (
          <>
            <Typography.Text strong>{t("browse.uploadDialog.chosen", { name: file.name, size: formatBytes(file.size) })}</Typography.Text>
            {format
              ? (
                <div>
                  <Checkbox checked={extract} disabled={uploading} onChange={(e) => { setExtract(e.target.checked); if (phase === "exists") setPhase("idle"); }}>
                    {t("browse.uploadDialog.extract", { format })}
                  </Checkbox>
                  <div><Typography.Text type="secondary">{t("browse.uploadDialog.extractHint")}</Typography.Text></div>
                </div>
              )
              : <Typography.Text type="secondary">{t("browse.uploadDialog.noExtract")}</Typography.Text>}
            <Form layout="vertical" component="div">
              <Form.Item
                label={t(unpack ? "browse.uploadDialog.unpackInto" : "browse.uploadDialog.saveAs")}
                htmlFor="storage-upload-name"
                validateStatus={nameValid ? undefined : "error"}
                help={nameValid ? t("browse.uploadDialog.target", { path: `/${target}` }) : t("browse.nameInvalid")}
                style={{ marginBottom: 0 }}
              >
                <Input id="storage-upload-name" value={nameEdited ? name : suggested} disabled={uploading} onChange={(e) => { setName(e.target.value); setNameEdited(true); if (phase === "exists") setPhase("idle"); }} />
              </Form.Item>
            </Form>
          </>
        )}
        {uploading && progress && (
          <div>
            <Progress percent={percent} status="active" aria-label={t("browse.uploadDialog.progressLabel")} />
            <Typography.Text type="secondary">
              {progress.loaded >= progress.total
                ? t("browse.uploadDialog.processing")
                : t("browse.uploadDialog.progress", { loaded: formatBytes(progress.loaded), total: formatBytes(progress.total) })}
            </Typography.Text>
          </div>
        )}
        {cancelled && <Alert type="info" showIcon message={t("browse.uploadDialog.cancelled")} />}
        {phase === "exists" && <Alert type="warning" showIcon message={t("browse.uploadDialog.exists", { path: `/${target}` })} description={t("browse.uploadDialog.overwriteHint")} />}
        {phase === "failed" && error && <Alert type="error" showIcon message={t("browse.uploadDialog.failed")} description={error} />}
      </Space>
    </Modal>
  );
}
