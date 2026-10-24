import { useEffect, useState } from "react";
import { Alert, Button, Space, Spin, Tag } from "antd";
import { useTranslation } from "react-i18next";
import { useIsAdmin } from "../../components/useIsAdmin";
import DetailDrawer from "../../components/detail/DetailDrawer";
import { api, usePolling, type Node } from "../../api";
import type { Storage, StorageModelFile } from "../../storageTypes";
import { storageTypeLabel } from "../../storageDisplay";
import StorageBrowseTab from "./StorageBrowseTab";
import StorageDetailActions from "./StorageDetailActions";
import StorageFilesTab from "./StorageFilesTab";
import StorageFormModal from "./StorageFormModal";
import { problemLines, splitProblems } from "./StorageNodeHealth";
import StorageNodesTab from "./StorageNodesTab";
import StorageCacheNodes from "./StorageCacheNodes";
import StorageOverviewTab from "./StorageOverviewTab";
import StorageScanModal from "./StorageScanModal";
import StorageTags from "./StorageTags";
import { useStorageRunner } from "./useStorageRunner";

const tabKeys = ["overview", "nodes", "files", "browse"] as const;
type TabKey = typeof tabKeys[number];

type Props = {
  // The storage whose detail is open, if any (the list page's ?storage=).
  id: number | null;
  // The tab the URL names (?tab=); anything else opens the overview.
  tab?: string;
  onTab: (key: string) => void;
  onClose: () => void;
  // The storage is gone: the list reloads and the drawer closes.
  onDeleted: () => void;
  // The cluster's nodes, which the list page already polls.
  nodes: Node[];
};

/** One storage in a drawer over the list: configuration and capacity, its nodes, the model files in it, and (for admins) its whole tree. */
// antd's drawer slides out in 0.3 s (motionDurationSlow).
const SLIDE_OUT_MS = 400;

export default function StorageDetailDrawer(props: Props) {
  // The polling lives with a shown drawer only. A closing drawer keeps the
  // last storage while it slides out, then unmounts and reads nothing.
  const [shown, setShown] = useState<number | null>(props.id);
  if (props.id != null && props.id !== shown) setShown(props.id);
  useEffect(() => {
    if (props.id != null) return;
    const timer = setTimeout(() => setShown(null), SLIDE_OUT_MS);
    return () => clearTimeout(timer);
  }, [props.id]);
  const id = props.id ?? shown;
  if (id == null) return <DetailDrawer open={false} onClose={props.onClose} title="" />;
  return <OpenStorageDetail {...props} id={id} open={props.id != null} />;
}

/** Polls the storage and its files every 15 s while shown. */
function OpenStorageDetail({ id, open, tab: requested, onTab, onClose, onDeleted, nodes }: Omit<Props, "id"> & { id: number; open: boolean }) {
  const { t } = useTranslation("storage");
  const isAdmin = useIsAdmin();
  const storage = usePolling<Storage>(() => api.storage(id, true), 15000, [id]);
  const files = usePolling<{ storageID: number; items: StorageModelFile[] }>(async () => ({ storageID: id, items: (await api.storageModelFiles(id)).items }), 15000, [id]);
  const [editing, setEditing] = useState(false);
  const [scanning, setScanning] = useState(false);
  const refresh = () => { void storage.refresh(); void files.refresh(); };
  const runner = useStorageRunner(refresh);
  // Browse is admin-only: for anyone else ?tab=browse opens the overview.
  const tab: TabKey = tabKeys.find((key) => key === requested && (key !== "browse" || isAdmin)) ?? "overview";

  // Polling keeps the previous storage's data until the new one arrives.
  const current = storage.data?.id === id ? storage.data : null;
  if (!current) {
    return (
      <DetailDrawer open={open} onClose={onClose} title={t("detail.title")}>
        {storage.error ? (
          <Alert
            type="error"
            showIcon
            message={t("detail.loadFailed")}
            description={storage.error.message}
            action={<Button size="small" onClick={() => void storage.refetch()}>{t("detail.retry")}</Button>}
          />
        ) : (
          <Spin />
        )}
      </DetailDrawer>
    );
  }
  // Offline nodes say nothing about the storage; only online nodes where it
  // is not ready are a warning.
  const failing = splitProblems(current.nodes.problems).failing;
  const notReady = failing.length;
  const currentFiles = files.data?.storageID === id ? files.data.items : null;
  const fileRows = currentFiles ?? [];
  return (
    <DetailDrawer
      open={open}
      onClose={onClose}
      title={current.name}
      subtitle={<Space size={4} wrap><Tag color="blue">{storageTypeLabel(t, current.type)}</Tag><StorageTags storage={current} /></Space>}
      extra={isAdmin ? <StorageDetailActions storage={current} runner={runner} onEdit={() => setEditing(true)} onScan={() => setScanning(true)} onDeleted={onDeleted} /> : undefined}
      banner={
        <>
          {runner.error && <Alert type="error" showIcon closable onClose={runner.clearError} message={t("actions.failed")} description={runner.error} style={{ marginTop: 12 }} />}
          {storage.error && <Alert type="warning" showIcon message={t("detail.refreshFailed")} description={storage.error.message}
            action={<Button size="small" onClick={() => void storage.refetch()}>{t("detail.retry")}</Button>} style={{ marginTop: 12 }} />}
          {notReady > 0 && (
            <Alert
              type="warning"
              showIcon
              style={{ marginTop: 12 }}
              message={t("detail.notReady", { count: notReady, total: current.nodes.total })}
              description={<div style={{ whiteSpace: "pre-line" }}>{problemLines(t, failing.slice(0, 3)).join("\n")}</div>}
              action={<Button size="small" onClick={() => onTab("nodes")}>{t("detail.showNodes")}</Button>}
            />
          )}
        </>
      }
      activeTab={tab}
      onTabChange={onTab}
      tabs={[
        { key: "overview", label: t("detail.tab.overview"), children: <StorageOverviewTab storage={current} onShowNodes={() => onTab("nodes")} /> },
        {
          key: "nodes",
          label: t("detail.tab.nodes"),
          count: current.nodes.total,
          children: current.type === "nfs_cache"
            ? <StorageCacheNodes key={current.id} storage={current} nodes={nodes} files={files.error ? null : currentFiles} isAdmin={isAdmin} onChanged={refresh} onEditScope={() => setEditing(true)} />
            : <StorageNodesTab storage={current} files={fileRows} />,
        },
        {
          key: "files",
          label: t("detail.tab.files"),
          count: currentFiles ? fileRows.length : current.model_count,
          children: (
            <StorageFilesTab
              storage={current}
              files={currentFiles}
              loading={files.loading || (!currentFiles && !files.error)}
              error={files.error}
              isAdmin={isAdmin}
              onRefresh={() => void files.refetch()}
              onChanged={refresh}
              onScan={() => setScanning(true)}
            />
          ),
        },
        // Browsing is admin-only on the server. It renders only while open:
        // leaving the tab unmounts it, which cancels a running upload.
        ...(isAdmin ? [{ key: "browse", label: t("detail.tab.browse"), children: tab === "browse" ? <StorageBrowseTab key={current.id} storage={current} /> : null }] : []),
      ]}
    >
      <StorageFormModal open={editing} storage={current} nodes={nodes} onClose={() => setEditing(false)} onSaved={() => { setEditing(false); refresh(); }} />
      {scanning && <StorageScanModal storage={current} onClose={() => setScanning(false)} onImported={refresh} />}
    </DetailDrawer>
  );
}
