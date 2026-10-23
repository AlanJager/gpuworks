import { useMemo, useState } from "react";
import { Input, Tag, Tooltip, Typography } from "antd";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import ResourceTable, { type ResourceColumn } from "../../components/resourceTable/ResourceTable";
import { detailExpandable } from "../../components/detail/DetailDrawer";
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

  // The row keeps what the table is read for (where, whether it works, how
  // full it is); the mount, the cache disk, the state's detail and since when
  // sit one step down so the row fits the 720px detail drawer.
  const columns: ResourceColumn<StorageNodeStatus>[] = [
    {
      key: "node",
      role: "name",
      title: t("nodes.column.node"),
      width: 156,
      sortValue: (n) => nodeRowName(n),
      render: (_, n) => <Link to="/nodes" state={{ nodeUUID: n.node_uuid }} onClick={(event) => event.stopPropagation()}>{nodeRowName(n)}</Link>,
    },
    {
      key: "state",
      title: t("nodes.column.state"),
      width: 130,
      sortValue: (n) => n.state,
      filters: Object.keys(stateColor).map((state) => ({ value: state, text: storageStateLabel(t, state) })),
      onFilter: (value, n) => n.state === value,
      render: (_, n) => <Tag color={stateColor[n.state] ?? "default"} className="status-tag" title={storageStateLabel(t, n.state)} style={{ marginInlineEnd: 0 }}>{storageStateLabel(t, n.state)}</Tag>,
    },
    {
      key: "capacity",
      title: t("nodes.column.capacity"),
      width: 230,
      sortValue: (n) => n.available_bytes,
      render: (_, n) => <StorageCapacity compact total={n.total_bytes} available={n.available_bytes} label={t("nodes.capacityAria", { node: nodeRowName(n) })} />,
    },
    { key: "writable", title: t("nodes.column.writable"), width: 90, sortValue: (n) => (n.writable ? 1 : 0), render: (_, n) => t(n.writable ? "nodes.yes" : "nodes.no") },
  ];
  const extra = (n: StorageNodeStatus) => {
    const detail = [n.reason ? storageReasonText(t, n.reason) : "", n.message].filter(Boolean).join(" · ");
    const limit = cacheLimitOf(storage, n.node_uuid);
    const used = cached.get(n.node_uuid) ?? 0;
    const seconds = n.updated_at ? Math.max(0, (Date.now() - new Date(n.updated_at).getTime()) / 1000) : 0;
    return (
      <dl className="detail-extra">
        <div style={{ gridColumn: "1 / -1" }}>
          <dt>{t(withCache ? "nodes.column.mountAndCache" : "nodes.column.path")}</dt>
          <dd>
            {n.path ? <Typography.Text code>{n.path}</Typography.Text> : "—"}
            {withCache && n.cache_path && <div>{t("nodes.cacheAt", { path: n.cache_path })}</div>}
          </dd>
        </div>
        {withCache && (
          <div style={{ gridColumn: "1 / -1" }}>
            <dt>{t("nodes.column.cache")}</dt>
            <dd>
              <StorageCapacity compact total={n.cache_total_bytes} available={n.cache_available_bytes} label={t("nodes.cacheAria", { node: nodeRowName(n) })} />
              <div>{limit ? t("nodes.cacheModels", { used: formatBytes(used), limit: formatBytes(limit) }) : t("nodes.cacheModelsUnlimited", { used: formatBytes(used) })}</div>
            </dd>
          </div>
        )}
        <div>
          <dt>{t("nodes.column.updated")}</dt>
          <dd>{n.updated_at ? <Tooltip title={formatTime(n.updated_at)}><span>{agoText(t, seconds)}</span></Tooltip> : "—"}</dd>
        </div>
        {(detail || n.state === "offline" || n.needs_own_directory) && (
          <div style={{ gridColumn: "1 / -1" }}>
            <dt>{t("nodes.column.state")}</dt>
            <dd>
              {n.state === "offline" && <div>{t("nodes.offlineHint")}</div>}
              {detail && <div>{detail}</div>}
              {n.needs_own_directory && <Typography.Text type="warning">{t("nodes.ownDirectoryHint")}</Typography.Text>}
            </dd>
          </div>
        )}
      </dl>
    );
  };
  return (
    <ResourceTable<StorageNodeStatus>
        tableId="storage-nodes"
        rowKey="node_uuid"
        columns={columns}
        data={rows}
        emptyDescription={t(storage.nodes.total === 0 ? "health.none" : "nodes.empty")}
        toolbar={<Input.Search allowClear aria-label={t("nodes.search")} placeholder={t("nodes.search")} value={search} onChange={(e) => setSearch(e.target.value)} style={{ width: 280 }} />}
        onRow={(n) => ({ className: n.state === "offline" ? "storage-node-offline" : undefined })}
        columnSettings={false}
        expandable={detailExpandable<StorageNodeStatus>(extra, { expand: t("detail.expand"), collapse: t("detail.collapse") })}
      />
  );
}
