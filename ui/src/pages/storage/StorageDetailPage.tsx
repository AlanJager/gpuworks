import { useState } from "react";
import { Alert, Button, Card, Space, Tabs, Tag } from "antd";
import { Link, useNavigate, useParams, useSearchParams } from "react-router-dom";
import { useTranslation } from "react-i18next";
import PageHeader from "../../components/PageHeader";
import { useIsAdmin } from "../../components/useIsAdmin";
import { api, usePolling, type List, type Node } from "../../api";
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

/** One storage: configuration and capacity, its nodes, the model files in it, and (for admins) its whole tree. Polls every 15 s. */
export default function StorageDetailPage() {
  const { t } = useTranslation("storage");
  const navigate = useNavigate();
  const id = Number(useParams().id);
  const [params, setParams] = useSearchParams();
  const isAdmin = useIsAdmin();
  const storage = usePolling<Storage>(() => api.storage(id, true), 15000, [id]);
  const files = usePolling<{ storageID: number; items: StorageModelFile[] }>(async () => ({ storageID: id, items: (await api.storageModelFiles(id)).items }), 15000, [id]);
  const nodes = usePolling<List<Node>>(api.nodes, 0);
  const [editing, setEditing] = useState(false);
  const [scanning, setScanning] = useState(false);
  const refresh = () => { void storage.refresh(); void files.refresh(); };
  const runner = useStorageRunner(refresh);
  const requested = params.get("tab");
  const tab: TabKey = tabKeys.find((key) => key === requested) ?? "overview";
  const selectTab = (key: string) => setParams((current) => { const next = new URLSearchParams(current); next.set("tab", key); return next; }, { replace: true });

  // Polling keeps the previous storage's data until the new one arrives.
  const current = storage.data?.id === id ? storage.data : null;
  if (!current) {
    if (storage.error) {
      return (
        <Alert
          type="error"
          showIcon
          message={t("detail.loadFailed")}
          description={storage.error.message}
          action={<Space><Button size="small" onClick={() => void storage.refetch()}>{t("detail.retry")}</Button><Link to="/storage">{t("detail.back")}</Link></Space>}
        />
      );
    }
    return <Card loading />;
  }
  // Offline nodes say nothing about the storage; only online nodes where it
  // is not ready are a warning.
  const failing = splitProblems(current.nodes.problems).failing;
  const notReady = failing.length;
  const currentFiles = files.data?.storageID === id ? files.data.items : null;
  const fileRows = currentFiles ?? [];
  return (
    <div className="resource-page">
      <PageHeader
        title={<Space size={8} wrap>{current.name}<Tag color="blue">{storageTypeLabel(t, current.type)}</Tag><StorageTags storage={current} /></Space>}
        breadcrumb={[t("page.breadcrumb.platform"), t("page.breadcrumb.resources"), <Link key="list" to="/storage">{t("page.breadcrumb.storage")}</Link>, current.name]}
        extra={isAdmin ? <StorageDetailActions storage={current} runner={runner} onEdit={() => setEditing(true)} onScan={() => setScanning(true)} onDeleted={() => navigate("/storage")} /> : undefined}
      />
      {runner.error && <Alert type="error" showIcon closable onClose={runner.clearError} message={t("actions.failed")} description={runner.error} style={{ marginBottom: 12 }} />}
      {storage.error && <Alert type="warning" showIcon message={t("detail.refreshFailed")} description={storage.error.message}
        action={<Button size="small" onClick={() => void storage.refetch()}>{t("detail.retry")}</Button>} style={{ marginBottom: 12 }} />}
      {notReady > 0 && (
        <Alert
          type="warning"
          showIcon
          style={{ marginBottom: 12 }}
          message={t("detail.notReady", { count: notReady, total: current.nodes.total })}
          description={<div style={{ whiteSpace: "pre-line" }}>{problemLines(t, failing.slice(0, 3)).join("\n")}</div>}
          action={<Button size="small" onClick={() => selectTab("nodes")}>{t("detail.showNodes")}</Button>}
        />
      )}
      <Tabs
        activeKey={tab}
        onChange={selectTab}
        items={[
          { key: "overview", label: t("detail.tab.overview"), children: <StorageOverviewTab storage={current} onShowNodes={() => selectTab("nodes")} /> },
          { key: "nodes", label: t("detail.tab.nodes", { count: current.nodes.total }), children: current.type === "nfs_cache" ? <StorageCacheNodes key={current.id} storage={current} nodes={nodes.data?.items ?? []} files={files.error ? null : currentFiles} isAdmin={isAdmin} onChanged={refresh} onEditScope={() => setEditing(true)} /> : <StorageNodesTab storage={current} files={fileRows} /> },
          {
            key: "files",
            label: t("detail.tab.files", { count: currentFiles ? fileRows.length : current.model_count }),
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
      />
      <StorageFormModal open={editing} storage={current} nodes={nodes.data?.items ?? []} onClose={() => setEditing(false)} onSaved={() => { setEditing(false); refresh(); }} />
      {scanning && <StorageScanModal storage={current} onClose={() => setScanning(false)} onImported={refresh} />}
    </div>
  );
}
