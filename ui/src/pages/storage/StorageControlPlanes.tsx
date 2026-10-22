import { useEffect, useState } from "react";
import { Alert, Space, Tag, Typography } from "antd";
import { useTranslation } from "react-i18next";
import { agoText } from "../../format";
import { api } from "../../api";
import { storageErrorText } from "../../storageDisplay";
import type { StorageControlPlane } from "../../storageTypes";

/**
 * The other gpuworks installations that use a shared storage. One that has not
 * refreshed its heartbeat lately is greyed out and only says when it was last
 * seen; it does not count as using the storage.
 */
export default function StorageControlPlanes({ storageId }: { storageId: number }) {
  const { t } = useTranslation("storage");
  const [planes, setPlanes] = useState<StorageControlPlane[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    let alive = true;
    setPlanes(null);
    setError(null);
    api.storageControlPlanes(storageId).then((rows) => { if (alive) setPlanes(rows.items); }).catch((e) => { if (alive) setError(storageErrorText(t, e)); });
    return () => { alive = false; };
  }, [storageId]);
  if (error) return <Alert type="warning" showIcon message={t("controlPlanes.loadFailed")} description={error} />;
  if (!planes) return <Typography.Text type="secondary">{t("controlPlanes.loading")}</Typography.Text>;
  if (planes.length === 0) return <Typography.Text type="secondary">{t("controlPlanes.none")}</Typography.Text>;
  return (
    <ul className="storage-control-planes" aria-label={t("controlPlanes.title")}>
      {planes.map((plane) => (
        <li key={plane.id}>
          <Space size={6} wrap>
            <Typography.Text strong type={plane.fresh ? undefined : "secondary"}>{plane.name || plane.id}</Typography.Text>
            {plane.version && <Typography.Text type="secondary">{plane.version}</Typography.Text>}
            {plane.fresh
              ? <Tag color="blue">{t("controlPlanes.inUse", { ago: agoText(t, plane.last_seen_seconds) })}</Tag>
              : <Tag>{t("controlPlanes.lastSeen", { ago: agoText(t, plane.last_seen_seconds) })}</Tag>}
          </Space>
        </li>
      ))}
    </ul>
  );
}
