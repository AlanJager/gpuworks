import { useEffect, useMemo, useRef, useState } from "react";
import { Alert, Button, Card, Col, Input, Modal, Row, Select, Space, Tag, Tooltip, Typography, message } from "antd";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { api, type Node } from "../../api";
import { DetailExtra, detailExpandable } from "../../components/detail/DetailDrawer";
import ResourceTable, { type ResourceColumn } from "../../components/resourceTable/ResourceTable";
import type { Storage, StorageModelFile } from "../../storageTypes";
import { storageErrorText, storageStateLabel } from "../../storageDisplay";
import { agoText, formatBytes } from "../../format";
import { formatTime } from "../../i18n/datetime";
import StorageCapacity from "./StorageCapacity";
import StorageCacheEditor, { type CacheEditorSession } from "./StorageCacheEditor";
import { cacheLimitOf } from "./storageFiles";
import { scopeText } from "./storageScope";
import { cacheDiskKnown, cacheNodeRows, cachePolicyMode, cacheRowState, filterCacheRows, type CacheNodeRow } from "./storageCachePolicy";

const stateColors: Record<string, string> = { satisfied: "green", pending: "blue", cleaning: "blue", running: "blue", failed: "red", blocked: "orange", offline: "default", unavailable: "orange", inactive: "default", unknown: "default" };
const cacheStates = ["satisfied", "pending", "running", "failed", "blocked", "offline", "unavailable", "inactive", "unknown"];

// A column of the expanded detail, as DetailExtra takes it.
type ExtraColumn = Parameters<typeof DetailExtra<CacheNodeRow>>[0]["columns"][number];

export default function StorageCacheNodes({ storage, nodes, files, isAdmin, onChanged, onEditScope }: {
  storage: Storage;
  nodes: Node[];
  files: StorageModelFile[] | null;
  isAdmin: boolean;
  onChanged: () => void;
  onEditScope: () => void;
}) {
  const { t } = useTranslation("storage");
  const [search, setSearch] = useState("");
  const [modes, setModes] = useState<string[]>([]);
  const [states, setStates] = useState<string[]>([]);
  const [selection, setSelection] = useState<string[]>([]);
  const [editing, setEditing] = useState<CacheEditorSession | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [clearIDs, setClearIDs] = useState<string[] | null>(null);
  const alive = useRef(true);
  useEffect(() => { alive.current = true; return () => { alive.current = false; }; }, []);
  useEffect(() => { if (!isAdmin) { setEditing(null); setClearIDs(null); } }, [isAdmin]);
  const allRows = useMemo(() => cacheNodeRows(storage, nodes), [storage, nodes]);
  const rows = useMemo(() => filterCacheRows(allRows, search, modes, states), [allRows, search, modes, states]);
  const byID = useMemo(() => new Map(allRows.map((row) => [row.node_uuid, row])), [allRows]);
  const selected = selection.filter((id) => byID.has(id));
  const selectedRows = selected.map((id) => byID.get(id)!);
  const applicable = allRows.filter((row) => row.active);
  const canClean = selectedRows.length > 0 && storage.enabled && selectedRows.every((row) => row.active);
  const openEditor = (kind: CacheEditorSession["kind"], selectedRows: CacheNodeRow[], restore = false) => {
    // Scope and node identities are frozen at opening, even if polling changes the table.
    setEditing({ kind, storage: structuredClone(storage), rows: structuredClone(selectedRows), restore });
  };
  const runReconcile = async (ids: string[], mode: "trim" | "clear") => {
    setBusy(true); setError("");
    try {
      await api.reconcileStorageCache(storage.id, ids, mode);
      if (!alive.current) return;
      message.success(t("cacheManage.queued", { count: ids.length })); onChanged();
    } catch (e) { if (alive.current) setError(storageErrorText(t, e)); }
    finally { if (alive.current) setBusy(false); }
  };
  const clear = () => setClearIDs([...selected]);
  // Each column is valid both in the table and in the expanded detail.
  const allColumns: (ResourceColumn<CacheNodeRow> & ExtraColumn)[] = [
    { key: "node", role: "name", title: t("nodes.column.node"), width: 160, sortValue: (row) => row.node_name, render: (_, row) => <Space direction="vertical" size={0}><Link to="/nodes" state={{ nodeUUID: row.node_uuid }} onClick={(event) => event.stopPropagation()}>{row.node_name}</Link>{row.node?.management_address && <Typography.Text type="secondary">{row.node.management_address}</Typography.Text>}</Space> },
    { key: "state", title: t("nodes.column.state"), width: 110, render: (_, row) => { const label = row.active ? storageStateLabel(t, row.state) : t("cacheManage.state.inactive"); return <Tag className="status-tag" title={label}>{label}</Tag>; } },
    { key: "stateDetail", title: t("nodes.column.state"), wide: true, width: 0, render: (_, row) => <Space direction="vertical" size={0}>{row.state === "offline" && <Typography.Text type="secondary">{t("nodes.offlineHint")}</Typography.Text>}{row.needs_own_directory && <Typography.Text type="warning">{t("nodes.ownDirectoryHint")}</Typography.Text>}{row.message && <Typography.Text type="secondary">{row.message}</Typography.Text>}{row.state !== "offline" && !row.needs_own_directory && !row.message && "—"}</Space> },
    { key: "path", title: t("nodes.column.mountAndCache"), wide: true, width: 0, render: (_, row) => <Space direction="vertical" size={0}><Typography.Text code>{row.path || row.target?.path || storage.path}</Typography.Text><Typography.Text type="secondary">{t("nodes.cacheAt", { path: row.target?.cache_path || storage.cache_path || "—" })}</Typography.Text><Typography.Text type="secondary">{cacheDiskKnown(storage, row) ? t("cacheManage.freeValue", { value: formatBytes(row.cache_available_bytes ?? 0) }) : t("cacheManage.unverified")}</Typography.Text></Space> },
    { key: "writable", title: t("nodes.column.writable"), width: 100, render: (_, row) => t(row.writable ? "nodes.yes" : "nodes.no") },
    { key: "capacity", title: t("nodes.column.capacity"), width: 210, render: (_, row) => <StorageCapacity compact total={row.total_bytes} available={row.available_bytes} label={t("nodes.capacityAria", { node: row.node_name })} /> },
    { key: "cache", title: t("nodes.column.cache"), wide: true, width: 0, render: (_, row) => {
      const limit = cacheLimitOf(storage, row.node_uuid);
      return <Space direction="vertical" size={2}>{cacheDiskKnown(storage, row) && <StorageCapacity compact total={row.cache_total_bytes} available={row.cache_available_bytes} label={t("nodes.cacheAria", { node: row.node_name })} />}<Typography.Text>{row.cache ? (limit > 0 ? t("nodes.cacheModels", { used: formatBytes(row.cache.used_bytes), limit: formatBytes(limit) }) : t("nodes.cacheModelsUnlimited", { used: formatBytes(row.cache.used_bytes) })) : t("cacheManage.unknown")}</Typography.Text><Typography.Text type="secondary">{t("cacheManage.effectiveLimit", { value: limit ? formatBytes(limit) : t("overview.cacheUnlimited") })}</Typography.Text>{row.cache && <Typography.Text type="secondary">{t("cacheManage.reclaimableValue", { value: formatBytes(row.cache.reclaimable_bytes) })}</Typography.Text>}</Space>;
    } },
    { key: "policy", title: t("cacheManage.source"), width: 135, render: (_, row) => <Tag color={cachePolicyMode(row.target) === "inherit" ? "default" : "blue"}>{t(`cacheManage.mode.${cachePolicyMode(row.target)}`)}</Tag> },
    { key: "cacheState", title: t("cacheManage.status"), width: 130, render: (_, row) => { const label = t(`cacheManage.state.${cacheRowState(row)}`, { defaultValue: cacheRowState(row) }); return <Tag color={stateColors[cacheRowState(row)]} className="status-tag" title={label}>{label}</Tag>; } },
    { key: "cacheDetail", title: t("cacheManage.status"), wide: true, width: 0, render: (_, row) => {
      const cache = row.cache;
      const explanation = cache?.reason && cache.reason !== "cleanup_failed" && cache.state !== "failed"
        ? t(`cacheManage.reason.${cache.reason}`, { defaultValue: cache.message ?? "" })
        : cache?.message;
      return <Space direction="vertical" size={0}>{explanation && <Typography.Text type="secondary">{explanation}</Typography.Text>}{cache && cache.protected_bytes > 0 && <Typography.Text type="secondary">{t("cacheManage.protectedValue", { value: formatBytes(cache.protected_bytes) })}</Typography.Text>}{!explanation && !(cache && cache.protected_bytes > 0) && "—"}</Space>;
    } },
    { key: "updated", title: t("nodes.column.updated"), width: 180, render: (_, row) => row.updated_at ? <Tooltip title={formatTime(row.updated_at)}><Typography.Text type="secondary">{agoText(t, Math.max(0, (Date.now() - new Date(row.updated_at).getTime()) / 1000))}</Typography.Text></Tooltip> : "—" },
    ...(isAdmin ? [{ key: "actions", role: "actions" as const, title: t("list.column.actions"), width: 130, render: (_: unknown, row: CacheNodeRow) => <Space wrap size={4} style={{ maxWidth: "100%" }} onClick={(event) => event.stopPropagation()}><Button type="link" size="small" onClick={() => openEditor("node", [row])}>{t("cacheManage.configure")}</Button>{row.active && ["failed", "blocked", "offline", "unavailable"].includes(cacheRowState(row)) && <Button size="small" type="link" disabled={!storage.enabled || busy} onClick={() => void runReconcile([row.node_uuid], "trim")}>{t("cacheManage.retry")}</Button>}</Space> }] : []),
  ];
  // The row keeps the node, its state, the cache's state and the actions,
  // beside the selection the batch actions use; the mount and cache disk, the
  // capacity, the policy, since when and every explanation sit one step down
  // so the row fits the 720px detail drawer.
  const rowKeys = ["node", "state", "cacheState", "actions"];
  const columns = allColumns.filter((column) => rowKeys.includes(column.key));
  // Keep cache policy and outcomes visible before mount and disk details, in
  // the row and in the expanded detail alike.
  const columnOrder = ["node", "cacheState", "cacheDetail", "cache", "policy", "state", "stateDetail", "path", "writable", "capacity", "updated", "actions"];
  const byOrder = (a: { key: string }, b: { key: string }) => columnOrder.indexOf(a.key) - columnOrder.indexOf(b.key);
  columns.sort(byOrder);
  const extraColumns = allColumns.filter((column) => !rowKeys.includes(column.key)).sort(byOrder);
  const metrics = [
    { key: "all", count: applicable.length, label: "total", states: [] },
    { key: "satisfied", count: applicable.filter((row) => cacheRowState(row) === "satisfied").length, label: "satisfied", states: ["satisfied"] },
    { key: "pending", count: applicable.filter((row) => ["pending", "cleaning", "running"].includes(cacheRowState(row))).length, label: "pending", states: ["pending", "cleaning", "running"] },
    { key: "attention", count: applicable.filter((row) => ["blocked", "failed", "unavailable"].includes(cacheRowState(row))).length, label: "attention", states: ["blocked", "failed", "unavailable"] },
    { key: "offline", count: applicable.filter((row) => cacheRowState(row) === "offline").length, label: "offline", states: ["offline"] },
  ];
  return <Space className="storage-cache-management" direction="vertical" size={16} style={{ width: "100%" }}>
    <Card size="small"><Row gutter={[20, 16]}><Col xs={24} md={14}><Typography.Text strong>{t("cacheManage.defaultPolicy")}</Typography.Text><Typography.Paragraph style={{ margin: "8px 0" }}><Typography.Text code>{storage.cache_path || "—"}</Typography.Text> <Tag color="blue">{t("cacheManage.defaultLimitValue", { value: storage.cache_max_bytes ? formatBytes(storage.cache_max_bytes) : t("overview.cacheUnlimited") })}</Tag></Typography.Paragraph><Typography.Text type="secondary">{t("cacheManage.inheritanceCount", { inherited: applicable.filter((row) => cachePolicyMode(row.target) === "inherit").length, overridden: applicable.filter((row) => cachePolicyMode(row.target) !== "inherit").length })}</Typography.Text>{isAdmin && <div><Button type="link" style={{ paddingLeft: 0 }} onClick={() => openEditor("defaults", allRows)}>{t("cacheManage.defaults")}</Button></div>}</Col><Col xs={24} md={10}><Typography.Text strong>{t("overview.scope")}</Typography.Text><Typography.Paragraph>{scopeText(t, storage)}</Typography.Paragraph>{isAdmin && <Button type="link" style={{ paddingLeft: 0 }} onClick={onEditScope}>{t("cacheManage.editScope")}</Button>}</Col></Row></Card>
    <Row gutter={[12, 12]}>{metrics.map((metric) => <Col flex="1 1 150px" key={metric.key}><Button block style={{ height: "auto", textAlign: "left", padding: 12 }} onClick={() => setStates(metric.states)}><Typography.Text type="secondary">{t(`cacheManage.metric.${metric.label}`)}</Typography.Text><div style={{ fontSize: 24 }}>{metric.count}</div></Button></Col>)}</Row>
    {error && <Alert type="error" showIcon closable onClose={() => setError("")} message={error} />}
    {selected.length > 0 && isAdmin && <Alert type="info" message={<Space wrap><span>{t("cacheManage.selectedCount", { count: selected.length })}</span><Button type="link" size="small" onClick={() => setSelection(rows.map((row) => row.node_uuid))}>{t("cacheManage.selectFiltered", { count: rows.length })}</Button><Button type="link" size="small" onClick={() => setSelection([])}>{t("cacheManage.deselect")}</Button></Space>} />}
    <ResourceTable<CacheNodeRow>
      tableId="storage-cache-nodes" rowKey="node_uuid" columns={columns} data={rows}
      emptyDescription={t(allRows.length ? "nodes.empty" : "health.none")}
      onRefresh={onChanged}
      onRow={(row) => ({ className: row.state === "offline" ? "storage-node-offline" : undefined })}
      columnSettings={false}
      expandable={detailExpandable<CacheNodeRow>((row) => <DetailExtra columns={extraColumns} record={row} />, { expand: t("detail.expand"), collapse: t("detail.collapse") })}
      toolbar={<Space wrap><Input.Search allowClear value={search} aria-label={t("nodes.search")} placeholder={t("cacheManage.search")} onChange={(e) => setSearch(e.target.value)} style={{ width: 240 }} /><Select mode="multiple" optionFilterProp="label" allowClear aria-label={t("cacheManage.filterMode")} placeholder={t("cacheManage.filterMode")} value={modes} onChange={setModes} style={{ minWidth: 170 }} options={["inherit", "custom", "unlimited"].map((value) => ({ value, label: t(`cacheManage.mode.${value}`) }))} /><Select mode="multiple" optionFilterProp="label" allowClear aria-label={t("cacheManage.filterState")} placeholder={t("cacheManage.filterState")} value={states} onChange={setStates} style={{ minWidth: 180 }} options={cacheStates.map((value) => ({ value, label: t(`cacheManage.state.${value}`) }))} /><Button onClick={() => { setSearch(""); setModes([]); setStates([]); }}>{t("cacheManage.clearFilters")}</Button></Space>}
      toolbarExtra={isAdmin ? <Space wrap><Button disabled={!selected.length} onClick={() => openEditor("bulk", selectedRows)}>{t("cacheManage.batch")}</Button><Button disabled={!selected.length} onClick={() => openEditor("bulk", selectedRows, true)}>{t("cacheManage.restore")}</Button><Button disabled={!canClean || busy} onClick={clear}>{t("cacheManage.clear")}</Button></Space> : undefined}
      rowSelection={isAdmin ? { selectedRowKeys: selected, preserveSelectedRowKeys: true, onChange: (keys) => setSelection(keys.map(String)), getCheckboxProps: (row) => ({ title: row.node_name, "aria-label": t("cacheManage.selectNode", { node: row.node_name }) }) } : undefined}
    />
    <Typography.Paragraph type="secondary">{t("cacheManage.budgetHelp")}</Typography.Paragraph>
    <Modal open={clearIDs !== null} title={t("cacheManage.clearTitle", { count: clearIDs?.length ?? 0 })}
      okText={t("cacheManage.clear")} cancelText={t("actions.cancel")} confirmLoading={busy}
      onCancel={() => setClearIDs(null)} onOk={async () => {
        if (!clearIDs) return;
        await runReconcile(clearIDs, "clear");
        if (alive.current) setClearIDs(null);
      }}>{t("cacheManage.clearHelp")}</Modal>
    {editing && <StorageCacheEditor session={editing} files={files} onClose={() => setEditing(null)} onSaved={onChanged} />}
  </Space>;
}
