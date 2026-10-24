import { useMemo, useState } from "react";
import { Button, Input, Progress, Select, Space, Tag, Tooltip, Typography } from "antd";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { detailExpandable } from "../../components/detail/DetailDrawer";
import ResourceTable, { type ResourceColumn } from "../../components/resourceTable/ResourceTable";
import { formatTime } from "../../i18n/datetime";
import { formatBytes } from "../../format";
import { storageTypeInfo, type Storage, type StorageModelFile } from "../../storageTypes";
import { acquisitionLabel, presenceVisual } from "../models/modelPresenceDisplay";
import StorageFileActions from "./StorageFileActions";
import { fileKey, filterFiles, type FileScope } from "./storageFiles";
import { nodeRowName } from "../../nodeDisplay";

const transferring = ["downloading", "syncing", "copying"];

/** A file's state; while it is being fetched, a bar, and a failure's message in a tooltip. */
function FileState({ file }: { file: StorageModelFile }) {
  const { t } = useTranslation("models");
  const visual = presenceVisual[file.state];
  const tag = <Tag color={visual?.color} style={{ marginInlineEnd: 0 }}>{visual ? t(visual.labelKey) : file.state}</Tag>;
  return (
    <Space direction="vertical" size={2}>
      {file.message ? <Tooltip title={file.message}>{tag}</Tooltip> : tag}
      {transferring.includes(file.state) && <Progress percent={Math.round((file.progress || 0) * 100)} size="small" style={{ width: 120, margin: 0 }} />}
    </Space>
  );
}

/**
 * The model files that are in this storage: the shared copy, node caches, or
 * each node's copy of a local storage. Filter by place, search by model name.
 */
export default function StorageFilesTab({ storage, files, loading, error, isAdmin, onRefresh, onChanged, onScan }: {
  storage: Storage;
  files: StorageModelFile[] | null;
  loading: boolean;
  error: Error | null;
  isAdmin: boolean;
  onRefresh: () => void;
  onChanged: () => void;
  onScan: () => void;
}) {
  const { t } = useTranslation("storage");
  const { t: tModels } = useTranslation("models");
  const [scope, setScope] = useState<FileScope>("all");
  const [search, setSearch] = useState("");
  const shared = storageTypeInfo(storage.type).shared;
  const all = files ?? [];
  const rows = useMemo(() => filterFiles(all, scope, search), [all, scope, search]);
  const nodeOptions = useMemo(() => {
    const seen = new Map<string, string>();
    for (const f of all) if (f.node_uuid) seen.set(f.node_uuid, nodeRowName(f));
    return [...seen].sort((a, b) => a[1].localeCompare(b[1])).map(([value, label]) => ({ value, label }));
  }, [all]);
  const showUsed = storageTypeInfo(storage.type).cache;

  // The row keeps the model, where it is, how big, its state and the actions;
  // its path, how it got there, when it was last used and who deploys it sit
  // one step down so the row fits the 720px detail drawer.
  const columns: ResourceColumn<StorageModelFile>[] = [
    {
      key: "model",
      role: "name",
      title: t("files.column.model"),
      width: 186,
      sortValue: (f) => f.model_name || f.source_id,
      render: (_, f) => (
        <>
          <Link to={`/models?detail=${f.model_id}`} onClick={(event) => event.stopPropagation()}>{f.model_name || f.source_id}</Link>
          {f.source && <span className="resource-table-sub">{f.source}</span>}
        </>
      ),
    },
    {
      key: "place",
      title: t("files.column.place"),
      width: 140,
      sortValue: (f) => (f.node_uuid ? nodeRowName(f) : ""),
      render: (_, f) => (
        <Space size={4}>
          {f.node_uuid ? <Typography.Text>{nodeRowName(f)}</Typography.Text> : <Typography.Text>{t("files.shared")}</Typography.Text>}
          {f.cache && <Tag>{tModels("files.cache")}</Tag>}
        </Space>
      ),
    },
    { key: "size_bytes", title: t("files.column.size"), width: 90, align: "right", sortValue: (f) => f.size_bytes, render: (_, f) => (f.size_bytes ? formatBytes(f.size_bytes) : "—") },
    {
      key: "state",
      title: t("files.column.state"),
      width: 130,
      ellipsis: false,
      sortValue: (f) => f.state,
      filters: [...new Set(all.map((f) => f.state))].map((state) => ({ value: state, text: presenceVisual[state] ? tModels(presenceVisual[state].labelKey) : state })),
      onFilter: (value, f) => f.state === value,
      render: (_, f) => <FileState file={f} />,
    },
    ...(isAdmin ? [{
      key: "actions",
      role: "actions" as const,
      title: t("files.column.actions"),
      width: 80,
      render: (_: unknown, f: StorageModelFile) => (
        <span onClick={(event) => event.stopPropagation()}>
          <StorageFileActions storage={storage} file={f} files={all} onChanged={onChanged} />
        </span>
      ),
    }] : []),
  ];
  const extra = (f: StorageModelFile) => (
    <dl className="detail-extra">
      <div style={{ gridColumn: "1 / -1" }}>
        <dt>{t("files.column.path")}</dt>
        <dd>{f.path ? <Typography.Text code copyable={{ text: f.path }}>{f.path}</Typography.Text> : "—"}</dd>
      </div>
      <div>
        <dt>{t("files.column.source")}</dt>
        <dd>
          {f.acquisition ? (acquisitionLabel[f.acquisition] ? tModels(acquisitionLabel[f.acquisition]) : f.acquisition) : "—"}
          {f.writer_node && <div>{tModels("files.writerNode", { node: f.writer_node })}</div>}
        </dd>
      </div>
      {showUsed && (
        <div>
          <dt>{t("files.column.lastUsed")}</dt>
          <dd>{f.cache ? (f.last_used_at ? formatTime(f.last_used_at) : tModels("files.neverUsed")) : "—"}</dd>
        </div>
      )}
      <div>
        <dt>{t("files.column.deployments")}</dt>
        <dd>
          {f.deployment_ids.length > 0
            ? <Space size={4}>{f.deployment_ids.map((id) => <Link key={id} to={`/deployments?deployment=${id}`}>#{id}</Link>)}</Space>
            : "—"}
        </dd>
      </div>
      {f.message && (
        <div style={{ gridColumn: "1 / -1" }}>
          <dt>{t("files.column.state")}</dt>
          <dd>{f.message}</dd>
        </div>
      )}
    </dl>
  );
  const empty = all.length === 0
    ? { text: t(shared ? "files.emptyShared" : "files.emptyLocal"), action: shared && isAdmin ? <Button type="primary" onClick={onScan}>{t("actions.scan")}</Button> : undefined }
    : { text: t("files.noMatch"), action: undefined };
  return (
    <ResourceTable<StorageModelFile>
      tableId="storage-files"
      rowKey={fileKey}
      columns={columns}
      data={rows}
      loading={loading && !files}
      error={error && !files ? error : null}
      onRefresh={onRefresh}
      emptyDescription={empty.text}
      emptyAction={empty.action}
      columnSettings={false}
      expandable={detailExpandable<StorageModelFile>(extra, { expand: t("detail.expand"), collapse: t("detail.collapse") })}
      toolbar={
        <>
          <Select
            aria-label={t("files.filterPlace")}
            value={scope}
            onChange={setScope}
            style={{ minWidth: 160 }}
            options={[
              { value: "all", label: t("files.placeAll") },
              ...(shared ? [{ value: "shared", label: t("files.shared") }] : []),
              ...nodeOptions,
            ]}
          />
          <Input.Search allowClear aria-label={t("files.search")} placeholder={t("files.search")} value={search} onChange={(e) => setSearch(e.target.value)} style={{ width: 220 }} />
        </>
      }
    />
  );
}
