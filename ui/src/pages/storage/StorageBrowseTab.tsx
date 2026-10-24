import { cloneElement, useEffect, useMemo, useState, type ReactElement } from "react";
import { Breadcrumb, Button, Select, Space, Tag, Tooltip, Typography, message } from "antd";
import { FileOutlined, FolderOutlined, FolderAddOutlined, LinkOutlined, UploadOutlined } from "@ant-design/icons";
import { useTranslation } from "react-i18next";
import { api } from "../../api";
import EmptyState from "../../components/EmptyState";
import ResourceTable, { type ResourceColumn } from "../../components/resourceTable/ResourceTable";
import { formatBytes } from "../../format";
import { formatTime } from "../../i18n/datetime";
import { storageErrorText, storageStateLabel } from "../../storageDisplay";
import {
  joinStorageRel, storageRelParent, storageTypeInfo, type Storage, type StorageFileItem,
} from "../../storageTypes";
import StorageBrowseActions, { WithReason } from "./StorageBrowseActions";
import StorageNameDialog from "./StorageNameDialog";
import StorageUploadDialog from "./StorageUploadDialog";
import { dirReadonlyReason, pathSteps } from "./storageBrowse";

interface Listing {
  /** node|path the listing is of; a listing of another place is not shown. */
  key: string;
  items: StorageFileItem[];
  next?: string;
  writable: boolean;
  node?: string;
}

type Dialog = { kind: "upload" } | { kind: "mkdir" } | { kind: "rename"; item: StorageFileItem } | null;

/**
 * Browse a storage's files (ADR 0053): a local storage on the node chosen
 * first, a shared one through whichever node the server picks. Every place can
 * be listed and downloaded; uploads, new directories, renames and deletes are
 * offered where the server says the place is writable, and explained where it
 * is not. The page mounts it only while the tab is open, so leaving the tab
 * cancels a running upload.
 */
export default function StorageBrowseTab({ storage }: { storage: Storage }) {
  const { t } = useTranslation("storage");
  const shared = storageTypeInfo(storage.type).shared;
  const nodeChoices = useMemo(() => (shared ? [] : storage.nodes.all ?? []), [shared, storage.nodes.all]);
  const readyNodes = nodeChoices.filter((n) => n.state === "ready");
  const [node, setNode] = useState<string | undefined>(() => (readyNodes.length === 1 ? readyNodes[0].node_uuid : undefined));
  const [dir, setDir] = useState("");
  const [listing, setListing] = useState<Listing | null>(null);
  const [loading, setLoading] = useState(false);
  const [loadingMore, setLoadingMore] = useState(false);
  const [error, setError] = useState<Error | null>(null);
  const [reload, setReload] = useState(0);
  const [dialog, setDialog] = useState<Dialog>(null);
  const ready = shared || !!node;
  const key = `${node ?? ""}|${dir}`;

  useEffect(() => {
    if (!ready) return;
    const abort = new AbortController();
    setLoading(true);
    setError(null);
    api.storageFiles(storage.id, { node, path: dir }, abort.signal).then(
      (page) => {
        if (abort.signal.aborted) return;
        setListing({ key, items: page.items ?? [], next: page.next_cursor || undefined, writable: page.writable, node: page.node_uuid });
        setLoading(false);
      },
      (e: unknown) => {
        if (abort.signal.aborted) return;
        setError(e instanceof Error ? e : new Error(String(e)));
        setLoading(false);
      },
    );
    return () => abort.abort();
  }, [storage.id, node, dir, reload, ready]);

  const current = listing?.key === key ? listing : null;
  const refresh = () => setReload((n) => n + 1);
  const chooseNode = (next: string) => {
    // Unmounting the dialog cancels an upload to the node left behind.
    setDialog(null);
    setNode(next);
  };

  const loadMore = async () => {
    if (!current?.next || loadingMore) return;
    setLoadingMore(true);
    try {
      const page = await api.storageFiles(storage.id, { node, path: dir, cursor: current.next });
      setListing((was) => (was && was.key === current.key
        ? { ...was, items: [...was.items, ...(page.items ?? [])], next: page.next_cursor || undefined, writable: page.writable }
        : was));
    } catch (e) {
      message.error(storageErrorText(t, e));
    } finally {
      setLoadingMore(false);
    }
  };

  const remove = async (item: StorageFileItem) => {
    try {
      await api.deleteStorageFile(storage.id, node, item.path);
      message.success(t("browse.deleted", { name: item.name }));
      refresh();
    } catch (e) {
      message.error(storageErrorText(t, e));
    }
  };

  const nodeSelect = !shared && (
    <Select
      aria-label={t("browse.nodeLabel")}
      placeholder={t("browse.chooseNode")}
      value={node}
      onChange={chooseNode}
      style={{ minWidth: 220 }}
      options={nodeChoices.map((n) => ({
        value: n.node_uuid,
        label: n.state === "ready" ? n.node_name : t("browse.nodeState", { node: n.node_name, state: storageStateLabel(t, n.state) }),
        disabled: n.state !== "ready",
      }))}
    />
  );

  if (!ready) {
    return (
      <Space direction="vertical" size={12} style={{ width: "100%" }}>
        {nodeSelect}
        <EmptyState description={t("browse.chooseNodeHint")} />
      </Space>
    );
  }

  const dirReason = current ? dirReadonlyReason(storage, dir, current.writable) : null;
  const dirReasonText = dirReason ? t(`browse.readonly.${dirReason}`) : null;
  const readerName = shared && current?.node ? (storage.nodes.all ?? []).find((n) => n.node_uuid === current.node)?.node_name ?? current.node : null;

  const columns: ResourceColumn<StorageFileItem>[] = [
    {
      key: "name",
      role: "name",
      title: t("browse.column.name"),
      width: 250,
      render: (_, item) => {
        if (item.dir && !item.symlink) {
          return (
            <Button type="link" size="small" icon={<FolderOutlined />} style={{ paddingInline: 0 }} aria-label={t("browse.open", { name: item.name })} onClick={() => setDir(item.path)}>
              {item.name}
            </Button>
          );
        }
        return (
          <Space size={6}>
            {item.symlink ? <LinkOutlined /> : <FileOutlined />}
            <Typography.Text>{item.name}</Typography.Text>
            {item.symlink && <Tooltip title={t("browse.symlinkHint")}><Tag>{t("browse.symlink")}</Tag></Tooltip>}
          </Space>
        );
      },
    },
    { key: "size", title: t("browse.column.size"), width: 100, align: "right", render: (_, item) => (item.dir ? "—" : formatBytes(item.size)) },
    // The region directories the agent adds itself (files/, .gpuworks/apps/)
    // carry no time and arrive as Go's zero time, which would read "1-01-01".
    { key: "mtime", title: t("browse.column.mtime"), width: 150, render: (_, item) => (!item.mtime || item.mtime.startsWith("0001-") ? "—" : formatTime(item.mtime)) },
    {
      key: "actions",
      role: "actions",
      title: t("browse.column.actions"),
      width: 150,
      render: (_, item) => (
        <StorageBrowseActions storage={storage} node={node} item={item} onRename={(target) => setDialog({ kind: "rename", item: target })} onDelete={remove} />
      ),
    },
  ];

  const uploadButton = <Button type="primary" icon={<UploadOutlined />} onClick={() => setDialog({ kind: "upload" })}>{t("browse.upload")}</Button>;
  const mkdirButton = <Button icon={<FolderAddOutlined />} onClick={() => setDialog({ kind: "mkdir" })}>{t("browse.mkdir")}</Button>;
  // Until the directory is listed nothing says whether it is writable.
  const gate = (button: ReactElement) => (!current ? cloneElement(button, { disabled: true }) : <WithReason reason={dirReasonText}>{button}</WithReason>);

  return (
    <Space direction="vertical" size={12} style={{ width: "100%" }}>
      <nav aria-label={t("browse.location")}>
        <Breadcrumb
          items={[
            { title: dir ? <Button type="link" size="small" style={{ padding: 0, height: "auto" }} onClick={() => setDir("")}>{storage.name}</Button> : <span aria-current="page">{storage.name}</span> },
            ...pathSteps(dir).map((step, index, all) => ({
              title: index === all.length - 1
                ? <span aria-current="page">{step.name}</span>
                : <Button type="link" size="small" style={{ padding: 0, height: "auto" }} onClick={() => setDir(step.path)}>{step.name}</Button>,
            })),
          ]}
        />
      </nav>
      {/* The columns add up to the drawer's width: no settings gear, no sideways scroll. */}
      <ResourceTable<StorageFileItem>
        tableId="storage-browse"
        columnSettings={false}
        rowKey="path"
        columns={columns}
        data={current?.items ?? []}
        loading={loading && !current}
        error={error}
        onRefresh={refresh}
        pagination={false}
        emptyDescription={t("browse.empty")}
        toolbar={
          <Space wrap size={8}>
            {nodeSelect}
            {gate(uploadButton)}
            {gate(mkdirButton)}
            {dirReasonText && <Typography.Text type="secondary">{dirReasonText}</Typography.Text>}
            {readerName && <Typography.Text type="secondary">{t("browse.readBy", { node: readerName })}</Typography.Text>}
          </Space>
        }
      />
      {current?.next && (
        <Space>
          <Button loading={loadingMore} onClick={() => void loadMore()}>{t("browse.loadMore")}</Button>
          <Typography.Text type="secondary">{t("browse.loaded", { count: current.items.length })}</Typography.Text>
        </Space>
      )}
      {/* A read-only place already says where one can write, next to its disabled buttons. */}
      {!dirReasonText && <Typography.Text type="secondary">{t("browse.writableAreas")}</Typography.Text>}
      {dialog?.kind === "upload" && (
        <StorageUploadDialog
          storageId={storage.id}
          node={shared ? undefined : node}
          dir={dir}
          onClose={() => setDialog(null)}
          onUploaded={() => { setDialog(null); refresh(); }}
        />
      )}
      {dialog?.kind === "mkdir" && (
        <StorageNameDialog
          title={t("browse.mkdirTitle", { path: dir ? `/${dir}` : "/" })}
          okText={t("browse.mkdirOk")}
          onClose={() => setDialog(null)}
          onSubmit={async (name) => {
            await api.storageFilesMkdir(storage.id, node, joinStorageRel(dir, name));
            message.success(t("browse.mkdirDone", { name }));
            setDialog(null);
            refresh();
          }}
        />
      )}
      {dialog?.kind === "rename" && (
        <StorageNameDialog
          title={t("browse.renameTitle", { name: dialog.item.name })}
          okText={t("browse.renameOk")}
          initial={dialog.item.name}
          onClose={() => setDialog(null)}
          onSubmit={async (name) => {
            await api.storageFilesRename(storage.id, node, dialog.item.path, joinStorageRel(storageRelParent(dialog.item.path), name));
            message.success(t("browse.renameDone", { name }));
            setDialog(null);
            refresh();
          }}
        />
      )}
    </Space>
  );
}
