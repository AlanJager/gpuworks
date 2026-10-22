import { useEffect, useState } from "react";
import { Alert, Progress, Space, Spin, Table, Typography } from "antd";
import type { TFunction } from "i18next";
import { useTranslation } from "react-i18next";
import { api, type Node, type StorageNodeUsage } from "../../api";
import type { Storage } from "../../storageTypes";
import { usedPercent } from "../../storageDisplay";
import { formatBytes } from "../../format";

type PathUsage = NonNullable<Node["path_usage"]>[number];

/** What a measured path is: the models directory, a container runtime's store, or a storage (or its cache) by name. */
export function pathKindLabel(t: TFunction, row: PathUsage, storages: Storage[]): string {
  const name = storages.find((s) => s.id === row.storage_id)?.name ?? (row.storage_id ? `#${row.storage_id}` : "");
  switch (row.kind) {
    case "models": return t("diskUsage.kind.models", { ns: "nodes" });
    case "docker": return t("diskUsage.kind.docker", { ns: "nodes" });
    case "udocker": return t("diskUsage.kind.udocker", { ns: "nodes" });
    case "host": return t("diskUsage.kind.host", { ns: "nodes" });
    case "storage": return t("diskUsage.kind.storage", { ns: "nodes", name });
    case "storage_cache": return t("diskUsage.kind.storageCache", { ns: "nodes", name });
    default: return row.kind;
  }
}

function Bar({ total, available, label }: { total: number; available: number; label: string }) {
  const { t } = useTranslation("nodes");
  if (total <= 0) return <Typography.Text type="secondary">{t("diskUsage.unknown")}</Typography.Text>;
  const percent = usedPercent(total, available);
  return (
    <Space size={8}>
      <Progress percent={percent} size="small" showInfo={false} aria-label={label} status={percent >= 90 ? "exception" : "normal"} style={{ width: 120, margin: 0 }} />
      <Typography.Text type="secondary">{formatBytes(total - available)} / {formatBytes(total)}</Typography.Text>
    </Space>
  );
}

/**
 * Disk of one node: how the platform's models, images and everything else
 * split what is used, and the paths it measures (models directory, container
 * runtime, each storage and its cache). Loaded when the node's details open.
 */
export default function NodeDiskUsage({ node }: { node: Node }) {
  const { t } = useTranslation("nodes");
  const [usage, setUsage] = useState<StorageNodeUsage | null>(null);
  const [storages, setStorages] = useState<Storage[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    let alive = true;
    setLoading(true);
    setError(null);
    Promise.all([api.storageUsage(), api.storages()])
      .then(([rows, list]) => { if (alive) { setUsage(rows.items.find((r) => r.node_uuid === node.uuid) ?? null); setStorages(list.items); } })
      .catch((e: Error) => { if (alive) setError(e.message); })
      .finally(() => { if (alive) setLoading(false); });
    return () => { alive = false; };
  }, [node.uuid]);

  if (loading) return <Spin size="small" aria-label={t("diskUsage.loading")} />;
  if (error) return <Alert type="warning" showIcon message={t("diskUsage.loadFailed")} description={error} />;
  const total = usage?.total_bytes ?? 0;
  const paths = node.path_usage ?? usage?.path_usage ?? [];
  return (
    <Space direction="vertical" size={8} style={{ width: "100%" }}>
      {usage?.capacity_known && total > 0 ? (
        <div>
          <Bar total={total} available={usage.available_bytes} label={t("diskUsage.barAria")} />
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>
            {t("diskUsage.breakdown", { models: formatBytes(usage.model_bytes), images: formatBytes(usage.image_bytes), other: formatBytes(usage.other_bytes), available: formatBytes(usage.available_bytes) })}
          </Typography.Text>
        </div>
      ) : <Typography.Text type="secondary">{t("diskUsage.unknown")}</Typography.Text>}
      {paths.length > 0 && (
        <Table<PathUsage>
          size="small"
          pagination={false}
          scroll={{ x: "max-content" }}
          rowKey={(row) => `${row.kind}-${row.storage_id ?? 0}-${row.path}`}
          dataSource={paths}
          columns={[
            { title: t("diskUsage.column.kind"), key: "kind", render: (_, row) => pathKindLabel(t, row, storages) },
            { title: t("diskUsage.column.path"), dataIndex: "path", render: (value: string) => <Typography.Text code>{value}</Typography.Text> },
            { title: t("diskUsage.column.usage"), key: "usage", render: (_, row) => <Bar total={row.total_bytes} available={row.available_bytes} label={t("diskUsage.pathAria", { path: row.path })} /> },
          ]}
        />
      )}
    </Space>
  );
}
