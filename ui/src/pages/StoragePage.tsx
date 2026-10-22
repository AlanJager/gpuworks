import { useState } from "react";
import { Alert, Button, Card, Space, Tooltip, Typography } from "antd";
import { EditOutlined, PlusOutlined, ProfileOutlined, SettingOutlined } from "@ant-design/icons";
import { Link, useNavigate } from "react-router-dom";
import { useTranslation } from "react-i18next";
import PageHeader from "../components/PageHeader";
import ResourceTable, { type ResourceColumn } from "../components/resourceTable/ResourceTable";
import { useIsAdmin } from "../components/useIsAdmin";
import { api, usePolling, type List, type Node } from "../api";
import { storageTypeInfo, type Storage } from "../storageTypes";
import { storageTypeLabel } from "../storageDisplay";
import StorageCapacity from "./storage/StorageCapacity";
import StorageDeleteButton from "./storage/StorageDeleteButton";
import StorageFormModal from "./storage/StorageFormModal";
import StorageNodeHealth from "./storage/StorageNodeHealth";
import StorageSettingsModal from "./storage/StorageSettingsModal";
import StorageTags from "./storage/StorageTags";
import { scopeText } from "./storage/storageScope";
import { useStorageRunner } from "./storage/useStorageRunner";

/** Where a storage points, in one or two lines: the export and mount point, or the directory. */
function Location({ storage }: { storage: Storage }) {
  const { t } = useTranslation("storage");
  const nfs = storageTypeInfo(storage.type).nfs;
  if (nfs) {
    return (
      <Space direction="vertical" size={0}>
        <Typography.Text code>{storage.export}</Typography.Text>
        {storage.path && <Typography.Text type="secondary" style={{ fontSize: 12 }}>{t("list.mountedAt", { path: storage.path })}</Typography.Text>}
      </Space>
    );
  }
  return storage.path ? <Typography.Text code>{storage.path}</Typography.Text> : <Typography.Text type="secondary">—</Typography.Text>;
}

/** The storages of this gpuworks, one line each; everything about one is on its own page. */
export default function StoragePage() {
  const { t } = useTranslation("storage");
  const navigate = useNavigate();
  const isAdmin = useIsAdmin();
  const storages = usePolling<List<Storage>>(api.storages, 15000);
  const nodes = usePolling<List<Node>>(api.nodes, 0);
  const [adding, setAdding] = useState(false);
  const [editing, setEditing] = useState<Storage | null>(null);
  const [settingsOpen, setSettingsOpen] = useState(false);
  const runner = useStorageRunner(() => void storages.refresh());

  const columns: ResourceColumn<Storage>[] = [
    {
      key: "name",
      role: "name",
      title: t("list.column.name"),
      width: 220,
      sortValue: (s) => s.name,
      render: (_, s) => <Space size={4}><Link to={`/storage/${s.id}`}>{s.name}</Link><StorageTags storage={s} /></Space>,
    },
    {
      key: "type",
      title: t("list.column.type"),
      width: 130,
      sortValue: (s) => s.type,
      filters: [...new Set((storages.data?.items ?? []).map((s) => s.type))].map((type) => ({ value: type, text: storageTypeLabel(t, type) })),
      onFilter: (value, s) => s.type === value,
      render: (_, s) => storageTypeLabel(t, s.type),
    },
    { key: "location", title: t("list.column.location"), width: 260, render: (_, s) => <Location storage={s} /> },
    { key: "scope", title: t("list.column.scope"), width: 160, render: (_, s) => scopeText(t, s) },
    { key: "nodes", title: t("list.column.nodes"), width: 180, render: (_, s) => <StorageNodeHealth storage={s} /> },
    {
      key: "capacity",
      title: t("list.column.capacity"),
      width: 260,
      ellipsis: false,
      sortValue: (s) => (storageTypeInfo(s.type).shared ? s.available_bytes : undefined),
      render: (_, s) => (storageTypeInfo(s.type).shared
        ? (s.total_bytes ? <StorageCapacity compact total={s.total_bytes} available={s.available_bytes} label={t("list.capacityAria", { name: s.name })} /> : <Typography.Text type="secondary">{t("capacity.unknown")}</Typography.Text>)
        : <Typography.Text type="secondary">{t("capacity.perNode")}</Typography.Text>),
    },
    { key: "model_count", title: t("list.column.models"), width: 100, align: "right", sortValue: (s) => s.model_count, render: (_, s) => s.model_count },
    { key: "deployments", title: t("list.column.deployments"), width: 100, align: "right", sortValue: (s) => s.deployment_ids.length, render: (_, s) => s.deployment_ids.length },
    {
      key: "actions",
      role: "actions",
      title: t("list.column.actions"),
      width: 110,
      render: (_, s) => (
        <Space size={2}>
          <Tooltip title={t("actions.detail")}><Button type="text" size="small" icon={<ProfileOutlined />} aria-label={t("actions.detail")} onClick={() => navigate(`/storage/${s.id}`)} /></Tooltip>
          {isAdmin && <Tooltip title={t("actions.edit")}><Button type="text" size="small" icon={<EditOutlined />} aria-label={t("actions.edit")} onClick={() => setEditing(s)} /></Tooltip>}
          {isAdmin && <StorageDeleteButton link storage={s} loading={runner.busy === `delete-${s.id}`} onConfirm={() => void runner.run(`delete-${s.id}`, () => api.deleteStorage(s.id), t("actions.deleted", { name: s.name }))} />}
        </Space>
      ),
    },
  ];
  const addButton = isAdmin ? <Button type="primary" icon={<PlusOutlined />} onClick={() => setAdding(true)}>{t("page.add")}</Button> : undefined;
  return (
    <div className="resource-page">
      <PageHeader
        title={t("page.title")}
        description={t("page.description")}
        breadcrumb={[t("page.breadcrumb.platform"), t("page.breadcrumb.resources"), t("page.breadcrumb.storage")]}
        extra={<Space>{addButton}<Button icon={<SettingOutlined />} onClick={() => setSettingsOpen(true)}>{t("page.settings")}</Button></Space>}
      />
      {runner.error && <Alert type="error" showIcon closable onClose={runner.clearError} message={t("actions.failed")} description={runner.error} style={{ marginBottom: 12 }} />}
      <Card>
        <ResourceTable<Storage>
          tableId="storages"
          rowKey="id"
          columns={columns}
          data={storages.data?.items ?? []}
          loading={storages.loading && !storages.data}
          error={storages.error}
          onRefresh={() => void storages.refetch()}
          updatedAt={storages.updatedAt}
          emptyDescription={t("page.empty")}
          emptyAction={addButton}
        />
      </Card>
      <StorageSettingsModal open={settingsOpen} isAdmin={isAdmin} onClose={() => setSettingsOpen(false)} />
      <StorageFormModal
        open={adding || !!editing}
        storage={editing}
        nodes={nodes.data?.items ?? []}
        onClose={() => { setAdding(false); setEditing(null); }}
        onSaved={(saved) => {
          const created = adding;
          setAdding(false);
          setEditing(null);
          void storages.refresh();
          if (created) navigate(`/storage/${saved.id}`);
        }}
      />
    </div>
  );
}
