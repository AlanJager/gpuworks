import { useMemo, useState } from "react";
import { Input, Space, Tag, Tooltip, Typography } from "antd";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import ResourceTable, { type ResourceColumn } from "../../components/resourceTable/ResourceTable";
import { formatTime } from "../../i18n/datetime";
import { storageTypeInfo, type Storage, type StorageModelFile, type StorageNodeStatus } from "../../storageTypes";
import { storageReasonText, storageStateLabel } from "../../storageDisplay";
import { agoText, formatBytes } from "../../format";
import { nodeRowName } from "../../nodeDisplay";
import StorageCapacity from "./StorageCapacity";
import { cacheBytesByNode, cacheLimitOf } from "./storageFiles";

const stateColor: Record<string, string> = { ready: "green", mounting: "blue", failed: "red", unresponsive: "orange", unsupported: "default", offline: "default" };

/**
 * Every node the storage applies to: its state there, where it is mounted, and
 * the capacity the node last measured (its own disk for a local storage, the
 * shared volume otherwise); an nfs_cache storage adds the cache disk and how
 * much of the cache limit the cached model files use.
 */
export default function StorageNodesTab({ storage, files }: { storage: Storage; files: StorageModelFile[] }) {
  const { t } = useTranslation("storage");
  const [search, setSearch] = useState("");
  const cached = useMemo(() => cacheBytesByNode(files), [files]);
  const withCache = storageTypeInfo(storage.type).cache;
  const rows = useMemo(() => {
    const all = storage.nodes.all ?? [];
    const needle = search.trim().toLowerCase();
    return needle ? all.filter((n) => nodeRowName(n).toLowerCase().includes(needle)) : all;
  }, [storage.nodes.all, search]);

  const columns: ResourceColumn<StorageNodeStatus>[] = [
    {
      key: "node",
      role: "name",
      title: t("nodes.column.node"),
      width: 200,
      sortValue: (n) => nodeRowName(n),
      render: (_, n) => <Link to="/nodes" state={{ nodeUUID: n.node_uuid }}>{nodeRowName(n)}</Link>,
    },
    {
      key: "state",
      title: t("nodes.column.state"),
      width: 220,
      ellipsis: false,
      sortValue: (n) => n.state,
      filters: Object.keys(stateColor).map((state) => ({ value: state, text: storageStateLabel(t, state) })),
      onFilter: (value, n) => n.state === value,
      render: (_, n) => {
        const detail = [n.reason ? storageReasonText(t, n.reason) : "", n.message].filter(Boolean).join(" · ");
        return (
          <Space direction="vertical" size={0}>
            <Tag color={stateColor[n.state] ?? "default"} style={{ marginInlineEnd: 0 }}>{storageStateLabel(t, n.state)}</Tag>
            {n.state === "offline" && <Typography.Text type="secondary" style={{ fontSize: 12 }}>{t("nodes.offlineHint")}</Typography.Text>}
            {detail && <Typography.Text type="secondary" style={{ fontSize: 12 }}>{detail}</Typography.Text>}
            {n.needs_own_directory && <Typography.Text type="warning" style={{ fontSize: 12 }}>{t("nodes.ownDirectoryHint")}</Typography.Text>}
          </Space>
        );
      },
    },
    {
      key: "path",
      title: t(withCache ? "nodes.column.mountAndCache" : "nodes.column.path"),
      width: 260,
      render: (_, n) => (
        <Space direction="vertical" size={0}>
          {n.path ? <Typography.Text code>{n.path}</Typography.Text> : <Typography.Text type="secondary">—</Typography.Text>}
          {withCache && n.cache_path && <Typography.Text type="secondary" style={{ fontSize: 12 }}>{t("nodes.cacheAt", { path: n.cache_path })}</Typography.Text>}
        </Space>
      ),
    },
    { key: "writable", title: t("nodes.column.writable"), width: 100, sortValue: (n) => (n.writable ? 1 : 0), render: (_, n) => t(n.writable ? "nodes.yes" : "nodes.no") },
    {
      key: "capacity",
      title: t("nodes.column.capacity"),
      width: 200,
      sortValue: (n) => n.available_bytes,
      render: (_, n) => <StorageCapacity compact total={n.total_bytes} available={n.available_bytes} label={t("nodes.capacityAria", { node: nodeRowName(n) })} />,
    },
    ...(withCache ? [{
      key: "cache",
      title: t("nodes.column.cache"),
      width: 220,
      ellipsis: false,
      render: (_: unknown, n: StorageNodeStatus) => {
        const limit = cacheLimitOf(storage, n.node_uuid);
        const used = cached.get(n.node_uuid) ?? 0;
        return (
          <Space direction="vertical" size={2}>
            <StorageCapacity compact total={n.cache_total_bytes} available={n.cache_available_bytes} label={t("nodes.cacheAria", { node: nodeRowName(n) })} />
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>
              {limit ? t("nodes.cacheModels", { used: formatBytes(used), limit: formatBytes(limit) }) : t("nodes.cacheModelsUnlimited", { used: formatBytes(used) })}
            </Typography.Text>
          </Space>
        );
      },
    }] : []),
    {
      key: "updated",
      title: t("nodes.column.updated"),
      width: 130,
      sortValue: (n) => n.updated_at,
      render: (_, n) => {
        if (!n.updated_at) return <Typography.Text type="secondary">—</Typography.Text>;
        const seconds = Math.max(0, (Date.now() - new Date(n.updated_at).getTime()) / 1000);
        return <Tooltip title={formatTime(n.updated_at)}><Typography.Text type="secondary">{agoText(t, seconds)}</Typography.Text></Tooltip>;
      },
    },
  ];
  return (
    <ResourceTable<StorageNodeStatus>
        tableId="storage-nodes"
        rowKey="node_uuid"
        columns={columns}
        data={rows}
        emptyDescription={t(storage.nodes.total === 0 ? "health.none" : "nodes.empty")}
        toolbar={<Input.Search allowClear aria-label={t("nodes.search")} placeholder={t("nodes.search")} value={search} onChange={(e) => setSearch(e.target.value)} style={{ width: 280 }} />}
        onRow={(n) => ({ className: n.state === "offline" ? "storage-node-offline" : undefined })}
      />
  );
}
