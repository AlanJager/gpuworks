import { useState } from "react";
import { Alert, Button, Card, Popconfirm, Space, Tag, Tooltip, Typography, message } from "antd";
import { DeleteOutlined, FileTextOutlined, PlusOutlined, StopOutlined } from "@ant-design/icons";
import { useTranslation } from "react-i18next";
import PageHeader from "../components/PageHeader";
import CopyButton from "../components/CopyButton";
import ResourceTable, { type ResourceColumn } from "../components/resourceTable/ResourceTable";
import { useIsAdmin } from "../components/useIsAdmin";
import { api, usePolling, type KubeCluster, type List } from "../api";
import { apiErrorText } from "../apiError";
import { formatTime } from "../i18n/datetime";
import { cardReturnText } from "../kubeClusters";
import KubeClusterDrawer from "./kubernetes/KubeClusterDrawer";
import KubeManifestModal from "./kubernetes/KubeManifestModal";

/** The Kubernetes clusters whose GPU nodes join this gpuworks, one line each. */
export default function KubernetesPage() {
  const { t } = useTranslation("kubernetes");
  const { t: tCommon } = useTranslation("common");
  const isAdmin = useIsAdmin();
  const clusters = usePolling<List<KubeCluster>>(api.kubeClusters, 15000);
  const [adding, setAdding] = useState(false);
  const [manifestOf, setManifestOf] = useState<KubeCluster | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);

  const run = async (name: string, action: () => Promise<unknown>, success: string) => {
    setBusy(name);
    setError(null);
    try {
      await action();
      message.success(success);
      void clusters.refetch();
    } catch (e) {
      setError(apiErrorText(tCommon, e));
    } finally {
      setBusy(null);
    }
  };

  const columns: ResourceColumn<KubeCluster>[] = [
    { key: "name", role: "name", title: t("column.name"), width: 200, sortValue: (c) => c.name, render: (_, c) => <Typography.Text strong>{c.name}</Typography.Text> },
    { key: "namespace", title: t("column.namespace"), width: 160, sortValue: (c) => c.namespace, render: (_, c) => <Typography.Text code>{c.namespace}</Typography.Text> },
    { key: "nodes", title: t("column.nodes"), width: 90, align: "right", sortValue: (c) => c.node_count, render: (_, c) => c.node_count },
    {
      key: "cluster_uid",
      title: t("column.clusterUid"),
      width: 300,
      render: (_, c) => c.cluster_uid
        ? <Typography.Text code>{c.cluster_uid}</Typography.Text>
        : <Tooltip title={t("clusterUid.notEnrolledHint")}><Tag>{t("clusterUid.notEnrolled")}</Tag></Tooltip>,
    },
    { key: "card_return", title: t("column.cardReturn"), width: 200, sortValue: (c) => c.idle_return_seconds, render: (_, c) => cardReturnText(t, c.idle_return_seconds) },
    {
      key: "status",
      title: t("column.status"),
      width: 110,
      sortValue: (c) => (c.revoked ? 1 : 0),
      render: (_, c) => c.revoked ? <Tag color="red">{t("status.revoked")}</Tag> : <Tag color="green">{t("status.active")}</Tag>,
    },
    { key: "created_at", title: t("column.createdAt"), width: 170, sortValue: (c) => c.created_at, render: (_, c) => formatTime(c.created_at) },
  ];
  if (isAdmin) {
    columns.push({
      key: "actions",
      role: "actions",
      title: t("column.actions"),
      width: 170,
      render: (_, c) => (
        <Space size={2}>
          {c.apply_command && <CopyButton size="small" label={t("actions.copyCommand")} text={c.apply_command} disabled={c.revoked} />}
          <Tooltip title={t("actions.manifest")}>
            <Button type="text" size="small" icon={<FileTextOutlined />} aria-label={t("actions.manifest")} onClick={() => setManifestOf(c)} />
          </Tooltip>
          <Popconfirm
            title={t("actions.revokeConfirm", { name: c.name })}
            description={t("actions.revokeHelp")}
            okText={t("actions.revoke")}
            cancelText={t("actions.cancel")}
            okButtonProps={{ danger: true }}
            disabled={c.revoked}
            onConfirm={() => run(`revoke-${c.uuid}`, () => api.revokeKubeCluster(c.uuid), t("actions.revoked", { name: c.name }))}
          >
            <Tooltip title={t("actions.revoke")}>
              <Button type="text" size="small" danger icon={<StopOutlined />} aria-label={t("actions.revoke")} disabled={c.revoked} loading={busy === `revoke-${c.uuid}`} />
            </Tooltip>
          </Popconfirm>
          <Popconfirm
            title={t("actions.deleteConfirm", { name: c.name })}
            description={t("actions.deleteHelp")}
            okText={t("actions.delete")}
            cancelText={t("actions.cancel")}
            okButtonProps={{ danger: true }}
            onConfirm={() => run(`delete-${c.uuid}`, () => api.deleteKubeCluster(c.uuid), t("actions.deleted", { name: c.name }))}
          >
            <Tooltip title={t("actions.delete")}>
              <Button type="text" size="small" danger icon={<DeleteOutlined />} aria-label={t("actions.delete")} loading={busy === `delete-${c.uuid}`} />
            </Tooltip>
          </Popconfirm>
        </Space>
      ),
    });
  }
  const addButton = isAdmin ? <Button type="primary" icon={<PlusOutlined />} onClick={() => setAdding(true)}>{t("page.add")}</Button> : undefined;

  return (
    <div className="resource-page">
      <PageHeader
        title={t("page.title")}
        description={t("page.description")}
        extra={addButton}
      />
      {error && <Alert type="error" showIcon closable onClose={() => setError(null)} message={t("actions.failed")} description={error} style={{ marginBottom: 12 }} />}
      <Card>
        <ResourceTable<KubeCluster>
          tableId="kube-clusters"
          rowKey="uuid"
          columns={columns}
          data={clusters.data?.items ?? []}
          loading={clusters.loading && !clusters.data}
          error={clusters.error}
          onRefresh={() => void clusters.refetch()}
          updatedAt={clusters.updatedAt}
          emptyDescription={t("page.empty")}
          emptyAction={addButton}
        />
      </Card>
      <KubeClusterDrawer open={adding} onClose={() => setAdding(false)} onCreated={() => void clusters.refetch()} />
      {manifestOf && <KubeManifestModal cluster={manifestOf} onClose={() => setManifestOf(null)} />}
    </div>
  );
}
