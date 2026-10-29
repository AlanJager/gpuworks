import { useEffect, useState } from "react";
import { Alert, Button, Form, Modal, Select, Spin, message } from "antd";
import { useTranslation } from "react-i18next";
import { api, type Model } from "../../api";
import NodePicker from "../../nodes/NodePicker";
import { canWriteStorage, isModelFileReady, storageTypeInfo } from "../../storageTypes";
import { storageErrorText, storageTypeLabel } from "../../storageDisplay";
import { preferredWritable, useStorages } from "./useStorages";
import { nodeDisplayName } from "../../nodeDisplay";

/**
 * Downloads a model from its source into a storage. A local storage needs the
 * nodes to download to (one copy each); a shared storage is downloaded once
 * through one chosen writer node. Only storages that take writes can be chosen; with a
 * single writable local storage the choice is made and only the nodes remain.
 */
export function DownloadModal({ model, onClose, onDone }: { model: Model | null; onClose: () => void; onDone: () => void }) {
  const { t } = useTranslation("models");
  const { storages, loading, error, reload } = useStorages(!!model);
  const [storageId, setStorageId] = useState<number>();
  const [nodeUuids, setNodeUuids] = useState<string[]>([]);
  const [submitting, setSubmitting] = useState(false);

  useEffect(() => {
    setNodeUuids([]);
    const open = storages.filter((s) => !(storageTypeInfo(s.type).shared && heldShared.has(s.id)));
    setStorageId(preferredWritable(open)?.id);
    // heldShared derives from model, which model?.id already covers.
  }, [model?.id, storages]);

  // Where the model already is: a shared storage holds one copy for every
  // node; a local storage holds one per node. Downloading again there is a
  // no-op, so those choices are shown as already present and not offered.
  const heldShared = new Set((model?.storages ?? []).filter((p) => isModelFileReady(p.state)).map((p) => p.storage_id));
  const heldOnNode = (storage: number | undefined, node: string) => (model?.nodes ?? []).some((p) =>
    p.node_uuid === node && !p.cache && isModelFileReady(p.state) && (p.storage_id === undefined || p.storage_id === storage));
  const writable = storages.filter(canWriteStorage);
  const chosen = storages.find((s) => s.id === storageId);
  const shared = storageTypeInfo(chosen?.type).shared;
  const onlyChoice = writable.length === 1 && writable[0].type === "local";
  const ready = !loading && !error && !!chosen && canWriteStorage(chosen) && nodeUuids.length > 0;

  const submit = async () => {
    if (!model || !chosen || !ready) return;
    setSubmitting(true);
    try {
      const result = await api.downloadModelToStorage(model.id, { storage_id: chosen.id, node_uuids: nodeUuids });
      if (result?.status === "present") message.info(t("download.alreadyPresent", { storage: chosen.name }));
      else message.success(t("download.started"));
      setNodeUuids([]);
      onDone();
    } catch (e) {
      message.error(t("download.failed", { detail: storageErrorText(t, e) }));
    } finally {
      setSubmitting(false);
    }
  };

  return <Modal title={t("download.title", { name: model?.name || model?.source_id || "" })} open={!!model} onCancel={onClose} onOk={() => void submit()} confirmLoading={submitting} okText={t("download.action")} cancelText={t("action.cancel")} okButtonProps={{ disabled: !ready }} destroyOnHidden>
    <Alert type="warning" showIcon style={{ marginBottom: 12 }} message={model?.weight_gib ? t("download.estimatedSize", { weight: model.weight_gib.toFixed(1) }) : t("download.fromSource")} />
    {loading ? <div style={{ textAlign: "center", padding: 16 }}><Spin /></div>
      : error ? <Alert type="error" showIcon message={t("download.storageLoadFailed", { detail: error.message })} action={<Button size="small" onClick={reload}>{t("action.retry")}</Button>} />
      : writable.length === 0 ? <Alert type="info" showIcon message={t("download.noWritableStorage")} />
      : <Form layout="vertical">
        {!onlyChoice && <Form.Item label={t("download.storage")}>
          <Select
            aria-label={t("download.storage")}
            style={{ width: "100%" }}
            placeholder={t("download.storagePlaceholder")}
            value={storageId}
            onChange={(id) => { setStorageId(id); setNodeUuids([]); }}
            options={storages.map((s) => {
              const held = storageTypeInfo(s.type).shared && heldShared.has(s.id);
              const note = !canWriteStorage(s) ? ` ${t("download.storageReadOnly")}` : held ? ` ${t("download.storageHasIt")}` : "";
              return { value: s.id, disabled: !canWriteStorage(s) || held, label: `${s.name} · ${storageTypeLabel(t, s.type)}${note}` };
            })}
          />
        </Form.Item>}
        {shared && <Alert type="info" showIcon style={{ marginBottom: 12 }} message={t("download.sharedHint")} />}
        <Form.Item label={t(shared ? "download.writer" : "download.nodes")}>
            {/* Online nodes, searched on the server: the fleet is not listed whole. */}
            <NodePicker
              mode={shared ? undefined : "multiple"}
              aria-label={t(shared ? "download.writer" : "download.nodes")}
              style={{ width: "100%" }}
              placeholder={t(shared ? "download.writerPlaceholder" : "download.nodesPlaceholder")}
              value={shared ? nodeUuids[0] : nodeUuids}
              onChange={(next) => setNodeUuids(Array.isArray(next) ? next : next ? [next] : [])}
              filter={{ state: ["online"], ...(shared && chosen ? { label: [`gpuworks.ai/storage=${chosen.id}`] } : {}) }}
              optionDisabled={(n) => !shared && heldOnNode(chosen?.id, n.uuid)}
              optionLabel={(n) => `${nodeDisplayName(n)} · ${t("download.gpuCount", { count: n.gpu_count })}${heldOnNode(chosen?.id, n.uuid) ? ` ${t("download.storageHasIt")}` : ""}`}
              notFoundContent={t("download.noOnlineNodes")}
            />
          </Form.Item>
      </Form>}
  </Modal>;
}
