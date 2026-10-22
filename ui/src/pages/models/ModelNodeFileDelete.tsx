import { Button, Popconfirm, Tooltip, message } from "antd";
import { DeleteOutlined } from "@ant-design/icons";
import { useTranslation } from "react-i18next";
import { api } from "../../api";
import { nodeRowName } from "../../nodeDisplay";
import { storageErrorText } from "../../storageDisplay";

/**
 * Deletes one model's files on one node (or, for a cache row, the node's cache
 * of it). The button is there only for admins (the callers decide) and is
 * disabled with the storage's name when the storage cannot be changed now.
 */
export function ModelNodeFileDelete({ modelId, nodeUuid, nodeName, storageId, storageName, cache, writable, label, onDone }: {
  modelId: number;
  nodeUuid: string;
  nodeName?: string;
  /** Absent for rows from before storages existed. */
  storageId?: number;
  storageName?: string;
  /** The row is a node's cache of a shared storage, not a copy of its own. */
  cache?: boolean;
  /** Whether the storage can be changed now. */
  writable: boolean;
  /** The button's name when it differs from "delete the model files on this node". */
  label?: string;
  onDone: () => void;
}) {
  const { t } = useTranslation("models");
  const buttonLabel = label ?? t("files.deleteOnNode");
  const remove = async () => {
    try {
      await api.deleteModelOnNode(modelId, nodeUuid, storageId);
      message.success(t("files.nodeDeleted"));
      onDone();
    } catch (e) {
      message.error(storageErrorText(t, e));
    }
  };
  return (
    <Popconfirm
      title={t("files.deleteOnNodeConfirm", { node: nodeRowName({ node_name: nodeName, node_uuid: nodeUuid }) })}
      description={t(cache ? "files.deleteCacheDescription" : "files.deleteOnNodeDescription")}
      okText={t("action.delete")}
      cancelText={t("action.cancel")}
      okButtonProps={{ danger: true }}
      disabled={!writable}
      onConfirm={() => void remove()}
    >
      <Tooltip title={writable ? buttonLabel : t("storageDelete.readOnly", { storage: storageName })}>
        <Button danger type="text" size="small" icon={<DeleteOutlined />} aria-label={buttonLabel} disabled={!writable} />
      </Tooltip>
    </Popconfirm>
  );
}
