import { useState } from "react";
import { Alert, Button, Checkbox, List, Modal, Tooltip, Typography, message } from "antd";
import { DeleteOutlined } from "@ant-design/icons";
import { useTranslation } from "react-i18next";
import { api } from "../../api";
import { APIError, apiErrorText } from "../../apiError";
import { agoText } from "../../format";
import { storageTypeInfo, type StorageControlPlane } from "../../storageTypes";

/** Code of the 409 that asks for an explicit force: other gpuworks installations use the same storage. */
const SHARED_BY_OTHERS = "STORAGE_SHARED_BY_OTHER_CONTROL_PLANES";

function controlPlanesOf(error: APIError): StorageControlPlane[] {
  const raw = error.params?.control_planes;
  return Array.isArray(raw) ? (raw as StorageControlPlane[]) : [];
}

/**
 * Deletes a model's single copy in a shared storage. The confirmation says who
 * is affected; if other gpuworks installations use the storage the server
 * answers 409 and the dialog turns into a warning that lists them and only
 * lets the delete go ahead once "force" is ticked.
 */
export function ModelStorageFileDelete({ modelId, storageId, storageName, storageType, writable, cachedNodes, onDone }: {
  modelId: number;
  storageId: number;
  storageName: string;
  storageType: string;
  /** Whether this gpuworks may change the storage now. */
  writable: boolean;
  /** For an nfs_cache storage: how many nodes cache the model (the delete clears those caches too). */
  cachedNodes: number;
  onDone: () => void;
}) {
  const { t } = useTranslation("models");
  const { t: tCommon } = useTranslation("common");
  const [open, setOpen] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [sharedBy, setSharedBy] = useState<StorageControlPlane[] | null>(null);
  const [force, setForce] = useState(false);

  const close = () => { setOpen(false); setError(null); setSharedBy(null); setForce(false); };
  const submit = async () => {
    setBusy(true);
    setError(null);
    try {
      await api.deleteModelInStorage(modelId, storageId, sharedBy !== null && force);
      message.success(t("storageDelete.deleted", { storage: storageName }));
      close();
      onDone();
    } catch (e) {
      if (e instanceof APIError && e.code === SHARED_BY_OTHERS) {
        setSharedBy(controlPlanesOf(e));
        setForce(false);
      } else {
        setError(apiErrorText(tCommon, e));
      }
    } finally {
      setBusy(false);
    }
  };

  const impact = storageTypeInfo(storageType).cache
    ? t("storageDelete.impactCache", { storage: storageName, count: cachedNodes })
    : t("storageDelete.impactNfs", { storage: storageName });
  return <>
    <Tooltip title={writable ? t("action.deleteModelFiles") : t("storageDelete.readOnly", { storage: storageName })}>
      <Button danger type="text" size="small" icon={<DeleteOutlined />} aria-label={t("action.deleteModelFiles")} disabled={!writable} onClick={() => setOpen(true)} />
    </Tooltip>
    <Modal
      title={t("storageDelete.title", { storage: storageName })}
      open={open}
      onCancel={close}
      onOk={() => void submit()}
      confirmLoading={busy}
      okText={t(sharedBy ? "storageDelete.forceConfirm" : "storageDelete.confirm")}
      cancelText={t("action.cancel")}
      okButtonProps={{ danger: true, disabled: sharedBy !== null && !force }}
      destroyOnHidden
    >
      <Typography.Paragraph>{impact}</Typography.Paragraph>
      {sharedBy !== null && <Alert
        type="warning"
        showIcon
        style={{ marginBottom: 12 }}
        message={t("storageDelete.sharedTitle")}
        description={<>
          <Typography.Paragraph>{t("storageDelete.sharedDescription", { storage: storageName })}</Typography.Paragraph>
          <List size="small" dataSource={sharedBy} renderItem={(plane) => (
            <List.Item key={plane.id}>
              <Typography.Text strong>{plane.name || plane.id}</Typography.Text>
              <Typography.Text type="secondary"> · {t("storageDelete.lastSeen", { ago: agoText(t, plane.last_seen_seconds) })}</Typography.Text>
            </List.Item>
          )} />
        </>}
      />}
      {sharedBy !== null && <Checkbox checked={force} onChange={(event) => setForce(event.target.checked)}>{t("storageDelete.force")}</Checkbox>}
      {error && <Alert type="error" showIcon style={{ marginTop: 12 }} message={error} />}
    </Modal>
  </>;
}
