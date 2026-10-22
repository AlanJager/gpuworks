import { type ReactNode } from "react";
import { Button, Popconfirm, Progress, Space, Table, Tag, Tooltip, Typography, message } from "antd";
import { useTranslation } from "react-i18next";
import { api, type Model, type ModelNodePresence } from "../../api";
import { formatTime } from "../../i18n/datetime";
import { useIsAdmin } from "../../components/useIsAdmin";
import { nodeRowName } from "../../nodeDisplay";
import { storageErrorText, storageTypeLabel } from "../../storageDisplay";
import { formatBytes } from "../../format";
import { storageTypeInfo, type ModelStoragePresence } from "../../storageTypes";
import { ModelNodeFileDelete } from "./ModelNodeFileDelete";
import { ModelStorageFileDelete } from "./ModelStorageFileDelete";
import { acquisitionLabel, modelPresenceBytes, presenceVisual, rateLabel, transferOrigin } from "./modelPresenceDisplay";

interface NodeGroup {
  key: string;
  storageName?: string;
  storageType?: string;
  rows: ModelNodePresence[];
}

/** Node rows grouped by the storage they are in; rows from before storages existed share one unnamed group. */
function groupByStorage(rows: ModelNodePresence[], cache: boolean): NodeGroup[] {
  const groups = new Map<string, NodeGroup>();
  for (const row of rows.filter((r) => !!r.cache === cache)) {
    const key = String(row.storage_id ?? 0);
    const group = groups.get(key) ?? { key, storageName: row.storage_name, storageType: row.storage_type, rows: [] };
    group.rows.push(row);
    groups.set(key, group);
  }
  return [...groups.values()];
}

function StateTag({ state }: { state: string }) {
  const { t } = useTranslation("models");
  const visual = presenceVisual[state];
  return <Tag color={visual?.color}>{visual ? t(visual.labelKey) : state}</Tag>;
}

function SourceText({ row }: { row: Parameters<typeof transferOrigin>[0] }) {
  const { t } = useTranslation("models");
  const value = row.acquisition;
  const label = value ? (acquisitionLabel[value] ? t(acquisitionLabel[value]) : value) : "—";
  const origin = transferOrigin(row);
  if (!origin) return <>{label}</>;
  if ("node" in origin) return <>{label} · {t("files.fromNode", { node: origin.node })}</>;
  const host = t("files.fromHost", { host: origin.host });
  // After a source failed the agent says why it moved on; the reason is long,
  // so it stays behind the host.
  return <>{label} · {origin.switched ? <Tooltip title={t("files.switchedSource", { reason: origin.switched })}><span style={{ textDecoration: "underline dotted" }}>{host}</span></Tooltip> : host}</>;
}

function StorageHeading({ name, type }: { name?: string; type?: string }) {
  const { t } = useTranslation("models");
  if (!name && !type) return null;
  return <div style={{ display: "flex", alignItems: "center", gap: 8, margin: "12px 0 6px" }}>
    {name && <Typography.Text strong>{name}</Typography.Text>}
    {type && <Tag>{storageTypeLabel(t, type)}</Tag>}
  </div>;
}

/**
 * Whether the storage a node's files are in can be changed now. A node row does
 * not carry it: an nfs_cache copy follows its shared storage's row, and a local
 * storage's files are always the node's to delete.
 */

const NODE_FILE_PAGE_SIZE = 10;

/** A transfer's progress, bytes and, while it moves, its rate. */
function TransferProgress({ row }: { row: Pick<ModelNodePresence, "state" | "progress" | "downloaded" | "size_bytes" | "rate_bps"> }) {
  const moving = row.state === "downloading" || row.state === "syncing";
  const rate = moving ? rateLabel(row.rate_bps) : "";
  return <Space direction="vertical" size={0} style={{ width: "100%" }}>
    <Progress percent={Math.round((row.progress || 0) * 100)} size="small" />
    <Typography.Text type="secondary">{modelPresenceBytes(row)}{rate ? ` · ${rate}` : ""}</Typography.Text>
  </Space>;
}

function NodeFileTable({ model, group, isAdmin, onChanged }: { model: Model; group: NodeGroup; isAdmin: boolean; onChanged: () => void }) {
  const { t } = useTranslation("models");
  const run = async (action: () => Promise<unknown>, success: string) => {
    try {
      await action();
      message.success(success);
      onChanged();
    } catch (e) {
      message.error(storageErrorText(t, e));
    }
  };
  const retry = (row: ModelNodePresence) => {
    if (row.acquisition === "download" && !row.cache) {
      return run(() => row.storage_id !== undefined
        ? api.downloadModelToStorage(model.id, { storage_id: row.storage_id, node_uuids: [row.node_uuid] })
        : api.downloadModel(model.id, row.node_uuid), t("presenceMessage.downloadStarted"));
    }
    return run(() => api.syncModel(model.id, row.node_uuid, row.source_node_uuid, row.last_rate_limit_kib_s ?? 0), t("presenceMessage.syncStarted"));
  };
  const deleteButton = (row: ModelNodePresence) => (
    <ModelNodeFileDelete
      modelId={model.id}
      nodeUuid={row.node_uuid}
      nodeName={row.node_name}
      storageId={row.storage_id}
      storageName={row.storage_name}
      cache={row.cache}
      writable={row.deletable === true}
      onDone={onChanged}
    />
  );
  // A model on a hundred nodes pages instead of stretching the dialog.
  return <Table<ModelNodePresence> size="small" rowKey={(row) => `${row.node_uuid}:${row.storage_id ?? 0}:${row.cache ? "c" : "l"}`} pagination={group.rows.length > NODE_FILE_PAGE_SIZE ? { pageSize: NODE_FILE_PAGE_SIZE, size: "small", showSizeChanger: false } : false} dataSource={group.rows} columns={[
    { title: t("detail.node"), key: "node", render: (_: unknown, row) => <Space size={4} wrap>
      <Tooltip title={row.node_uuid}><Typography.Text strong={!!row.node_name} code={!row.node_name}>{nodeRowName(row)}</Typography.Text></Tooltip>
      {row.cache && <Tag>{t("files.cache")}</Tag>}
    </Space> },
    { title: t("files.path"), dataIndex: "path", render: (value: string) => value ? <Typography.Text code style={{ wordBreak: "break-all" }}>{value}</Typography.Text> : "—" },
    { title: t("detail.size"), dataIndex: "size_bytes", render: (value: number) => value ? formatBytes(value) : "—" },
    { title: t("detail.status"), dataIndex: "state", render: (value: string, row) => <Space direction="vertical" size={0}>
      <StateTag state={value} />
      {row.message && (value === "failed" || value === "download_required") && <Typography.Text type="danger" style={{ wordBreak: "break-word" }}>{row.message}</Typography.Text>}
    </Space> },
    { title: t("detail.progress"), key: "progress", render: (_: unknown, row) => <TransferProgress row={row} /> },
    { title: t("column.source"), key: "source", render: (_: unknown, row) => <SourceText row={row} /> },
    { title: t("files.lastUsed"), dataIndex: "last_used_at", render: (value?: string) => value ? formatTime(value) : <Typography.Text type="secondary">{t("files.neverUsed")}</Typography.Text> },
    { title: t("column.actions"), key: "action", render: (_: unknown, row) => <Space size={4}>
      {row.state === "download_required" && <Button size="small" onClick={() => run(() => api.downloadModel(model.id, row.node_uuid), t("presenceMessage.downloadStarted"))}>{t("action.internetDownload")}</Button>}
      {(row.state === "failed" || row.state === "download_required") && <Button size="small" onClick={() => retry(row)}>{t("action.retry")}</Button>}
      {row.state === "syncing" && <Popconfirm title={t("presenceAction.cancelConfirm")} onConfirm={() => run(() => api.cancelModelSync(model.id, row.node_uuid), t("presenceMessage.syncCanceled"))} okText={t("presenceAction.cancelSync")} cancelText={t("presenceAction.continueSync")}><Button size="small" danger>{t("presenceAction.cancelSync")}</Button></Popconfirm>}
      {isAdmin && deleteButton(row)}
    </Space> },
  ]} />;
}

function SharedFileTable({ model, rows, isAdmin, onChanged }: { model: Model; rows: ModelStoragePresence[]; isAdmin: boolean; onChanged: () => void }) {
  const { t } = useTranslation("models");
  return <Table<ModelStoragePresence> size="small" rowKey="storage_id" pagination={false} dataSource={rows} columns={[
    { title: t("files.storage"), key: "storage", render: (_: unknown, row) => <Space size={4} wrap><Typography.Text strong>{row.storage_name}</Typography.Text><Tag>{storageTypeLabel(t, row.storage_type)}</Tag></Space> },
    { title: t("files.relativePath"), dataIndex: "path", render: (value: string) => value ? <Typography.Text code style={{ wordBreak: "break-all" }}>{value}</Typography.Text> : "—" },
    { title: t("detail.size"), dataIndex: "size_bytes", render: (value: number) => value ? formatBytes(value) : "—" },
    { title: t("detail.status"), dataIndex: "state", render: (value: string, row) => <Space direction="vertical" size={0}>
      <StateTag state={value} />
      {value === "missing"
        ? <Typography.Text type="danger">{t("files.missingHint")}</Typography.Text>
        : row.message && (value === "failed" || value === "download_required") && <Typography.Text type="danger" style={{ wordBreak: "break-word" }}>{row.message}</Typography.Text>}
    </Space> },
    { title: t("column.source"), key: "source", render: (_: unknown, row) => <SourceText row={row} /> },
    { title: t("detail.progress"), key: "progress", render: (_: unknown, row) => row.state === "downloading" || row.state === "syncing" ? <TransferProgress row={row} /> : "—" },
    { title: t("files.availability"), key: "availability", render: (_: unknown, row) => <Space direction="vertical" size={0}>
      <Typography.Text strong>{t("files.readyNodes", { count: row.ready_nodes })}</Typography.Text>
      {storageTypeInfo(row.storage_type).cache && <Typography.Text>{t("files.cachedNodes", { count: row.cached_nodes })}</Typography.Text>}
      {row.writer_node && <Typography.Text type="secondary">{t("files.writerNode", { node: row.writer_node })}</Typography.Text>}
    </Space> },
    ...(isAdmin ? [{ title: t("column.actions"), key: "action", render: (_: unknown, row: ModelStoragePresence) => <ModelStorageFileDelete modelId={model.id} storageId={row.storage_id} storageName={row.storage_name} storageType={row.storage_type} writable={row.writable} cachedNodes={row.cached_nodes} onDone={onChanged} /> }] : []),
  ]} />;
}

/**
 * A model's files, grouped by where they are. Local storages list one row per
 * node; a shared storage is one row (its files exist once) that says how many
 * nodes can use it; an nfs_cache storage also lists the nodes that cached it.
 * With a single local storage and nothing shared it is the plain node table.
 */
export function ModelFilesSection({ model, onChanged }: { model: Model; onChanged: () => void }) {
  const { t } = useTranslation("models");
  const isAdmin = useIsAdmin();
  const shared = model.storages ?? [];
  const local = groupByStorage(model.nodes ?? [], false);
  const cached = groupByStorage(model.nodes ?? [], true);
  const headings = shared.length > 0 || local.length + cached.length > 1;
  const empty = local.length + cached.length === 0 && shared.length === 0;
  const block = (key: string, heading: ReactNode, body: ReactNode) => <div key={key}>{heading}{body}</div>;
  return <div id="model-files" style={{ marginTop: 16, scrollMarginTop: 16 }}>
    <Typography.Text strong style={{ display: "block", marginBottom: 8 }}>{t("files.title")}</Typography.Text>
    {empty && <Table size="small" pagination={false} dataSource={[]} columns={[{ title: t("detail.node"), key: "node" }]} locale={{ emptyText: t("files.empty") }} />}
    {local.map((group) => block(`local-${group.key}`, headings ? <StorageHeading name={group.storageName} type={group.storageType} /> : null, <NodeFileTable model={model} group={group} isAdmin={isAdmin} onChanged={onChanged} />))}
    {shared.length > 0 && block("shared", <StorageHeading name={t("files.sharedTitle")} />, <SharedFileTable model={model} rows={shared} isAdmin={isAdmin} onChanged={onChanged} />)}
    {cached.map((group) => block(`cache-${group.key}`, <StorageHeading name={group.storageName} type={group.storageType} />, <NodeFileTable model={model} group={group} isAdmin={isAdmin} onChanged={onChanged} />))}
  </div>;
}
