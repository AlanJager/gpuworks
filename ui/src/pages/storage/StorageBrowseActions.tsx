import { cloneElement, useId, type ReactElement, type CSSProperties } from "react";
import { Button, Dropdown, Popconfirm, Space, Tooltip } from "antd";
import { DeleteOutlined, DownloadOutlined, DownOutlined, EditOutlined } from "@ant-design/icons";
import { useTranslation } from "react-i18next";
import { storageFileDownloadURL } from "../../api";
import type { Storage, StorageDownloadFormat, StorageFileItem } from "../../storageTypes";
import { itemReadonlyReason, startDownload } from "./storageBrowse";

/**
 * A control that may be unavailable: when a reason is given, the control is
 * disabled and wrapped in a focusable span, so the tooltip with the reason
 * shows on hover and on keyboard focus, and the control's accessible
 * description carries the same text for a screen reader.
 */
export function WithReason({ reason, children }: { reason?: string | null; children: ReactElement<{ disabled?: boolean; "aria-describedby"?: string; style?: CSSProperties }> }) {
  const id = useId();
  if (!reason) return children;
  return (
    <Tooltip title={reason} trigger={["hover", "focus"]}>
      <span tabIndex={0} className="storage-browse-disabled" aria-describedby={id} style={{ display: "inline-block", cursor: "not-allowed" }}>
        {cloneElement(children, { disabled: true, "aria-describedby": id, style: { ...(children.props.style ?? {}), pointerEvents: "none" } })}
        <span id={id} hidden>{reason}</span>
      </span>
    </Tooltip>
  );
}

/** One entry's actions: download (a directory as .zip, or .tar.gz on request), rename, delete. */
export default function StorageBrowseActions({ storage, node, item, onRename, onDelete }: {
  storage: Storage;
  node?: string;
  item: StorageFileItem;
  onRename: (item: StorageFileItem) => void;
  onDelete: (item: StorageFileItem) => Promise<void>;
}) {
  const { t } = useTranslation("storage");
  const reason = itemReadonlyReason(storage, item);
  const reasonText = reason ? t(`browse.readonly.${reason}`, { name: item.name }) : null;
  const url = (format?: StorageDownloadFormat) => storageFileDownloadURL(storage.id, node, item.path, format);
  const downloadLabel = t("browse.download", { name: item.name });

  let download: ReactElement;
  if (item.symlink) {
    download = (
      <WithReason reason={t("browse.symlinkHint")}>
        <Button type="text" size="small" icon={<DownloadOutlined />} aria-label={downloadLabel} />
      </WithReason>
    );
  } else if (item.dir) {
    download = (
      <Space.Compact>
        <Tooltip title={t("browse.downloadZip")}>
          <Button type="text" size="small" icon={<DownloadOutlined />} href={url("zip")} download aria-label={downloadLabel} />
        </Tooltip>
        <Dropdown
          trigger={["click"]}
          menu={{
            items: [
              { key: "zip", label: t("browse.downloadZip") },
              { key: "tar.gz", label: t("browse.downloadTarGz") },
            ],
            onClick: ({ key }) => startDownload(url(key as StorageDownloadFormat)),
          }}
        >
          <Button type="text" size="small" icon={<DownOutlined />} aria-label={t("browse.downloadFormats", { name: item.name })} />
        </Dropdown>
      </Space.Compact>
    );
  } else {
    download = (
      <Tooltip title={downloadLabel}>
        <Button type="text" size="small" icon={<DownloadOutlined />} href={url()} download aria-label={downloadLabel} />
      </Tooltip>
    );
  }

  const renameButton = <Button type="text" size="small" icon={<EditOutlined />} aria-label={t("browse.rename", { name: item.name })} onClick={() => onRename(item)} />;
  const deleteButton = <Button type="text" size="small" danger icon={<DeleteOutlined />} aria-label={t("browse.delete", { name: item.name })} />;
  return (
    <Space size={0}>
      {download}
      {reasonText ? <WithReason reason={reasonText}>{renameButton}</WithReason> : renameButton}
      {reasonText
        ? <WithReason reason={reasonText}>{deleteButton}</WithReason>
        : (
          <Popconfirm
            title={t("browse.deleteConfirm", { name: item.name })}
            description={t("browse.deleteDescription")}
            okText={t("browse.deleteOk")}
            cancelText={t("browse.cancel")}
            okButtonProps={{ danger: true }}
            onConfirm={() => onDelete(item)}
          >
            {deleteButton}
          </Popconfirm>
        )}
    </Space>
  );
}
