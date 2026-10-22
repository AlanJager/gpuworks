import { useState } from "react";
import { Button, Popconfirm, Tooltip } from "antd";
import { DeleteOutlined } from "@ant-design/icons";
import type { TFunction } from "i18next";
import { useTranslation } from "react-i18next";
import { storageTypeInfo, type Storage } from "../../storageTypes";

/** The words of the delete confirmation, for the list's button and the detail page's menu alike. */
export function storageDeleteConfirmText(t: TFunction<"storage">, storage: Storage) {
  return {
    title: t("actions.deleteConfirm", { name: storage.name }),
    description: t(storageTypeInfo(storage.type).shared ? "actions.deleteSharedHelp" : "actions.deleteHelp"),
    okText: t("actions.delete"),
    cancelText: t("actions.cancel"),
  };
}

/** Delete with a confirmation that says what stays: model files are never deleted, only the storage record. */
export default function StorageDeleteButton({ storage, loading, link = false, onConfirm }: {
  storage: Storage;
  loading?: boolean;
  /** An icon for a table row's actions; a normal danger button otherwise. */
  link?: boolean;
  onConfirm: () => void;
}) {
  const { t } = useTranslation("storage");
  const [confirmOpen, setConfirmOpen] = useState(false);
  const text = storageDeleteConfirmText(t, storage);
  return (
    <Popconfirm
      title={text.title}
      description={text.description}
      okText={text.okText}
      cancelText={text.cancelText}
      okButtonProps={{ danger: true }}
      onConfirm={onConfirm}
      onOpenChange={setConfirmOpen}
    >
      {link
        ? <Tooltip title={t("actions.delete")} open={confirmOpen ? false : undefined}><Button size="small" type="text" danger loading={loading} icon={<DeleteOutlined />} aria-label={t("actions.delete")} /></Tooltip>
        : <Button danger loading={loading}>{t("actions.delete")}</Button>}
    </Popconfirm>
  );
}
