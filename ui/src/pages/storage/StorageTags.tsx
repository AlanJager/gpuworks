import { Tag, Tooltip } from "antd";
import { useTranslation } from "react-i18next";
import type { Storage } from "../../storageTypes";
import { storageTypeLabel } from "../../storageDisplay";

/** Marks of a storage: default, and read-only when it is disabled. `withType` adds its type first. */
export default function StorageTags({ storage, withType = false }: { storage: Storage; withType?: boolean }) {
  const { t } = useTranslation("storage");
  return (
    <>
      {withType && <Tag color="blue">{storageTypeLabel(t, storage.type)}</Tag>}
      {storage.is_default && <Tag color="gold">{t("tag.default")}</Tag>}
      {!storage.enabled && <Tooltip title={t("tag.readOnlyHelp")}><Tag color="orange">{t("tag.readOnly")}</Tag></Tooltip>}
    </>
  );
}
