import { Space, Typography } from "antd";
import { useTranslation } from "react-i18next";
import type { ModelCore, ModelPresenceSummary } from "../../api";

/** List summaries identify ready locations, not their storage backend or name. */
export default function ModelRegistrationDetails({ model }: {
  model: ModelCore & { presence?: ModelPresenceSummary };
}) {
  const { t } = useTranslation("deploy");
  const presence = model.presence;
  const parts: string[] = [];
  if (presence && presence.ready_nodes > 0) parts.push(t("weightSourceModal.onNodes", { count: presence.ready_nodes }));
  if (presence && presence.ready_storages > 0) parts.push(t("weightSourceModal.onStorages", { count: presence.ready_storages }));
  const location = !presence ? t("weightSourceModal.locationUnknown") : parts.length ? parts.join(" · ") : t("weightSourceModal.noFiles");
  return <Space size={6} wrap>
    <Typography.Text type="secondary" title={model.revision || undefined}>{model.revision ? t("weightSourceModal.revision", { revision: model.revision }) : t("weightSourceModal.revisionUnknown")}</Typography.Text>
    <Typography.Text type="secondary">{location}</Typography.Text>
  </Space>;
}
