import { useState } from "react";
import { Alert, Button, Card, Popconfirm, Popover, Space, Tag, Tooltip, Typography, message } from "antd";
import { CloudUploadOutlined, DeleteOutlined, DisconnectOutlined, FileTextOutlined, PlusOutlined, StopOutlined } from "@ant-design/icons";
import { useTranslation } from "react-i18next";
import PageHeader from "../components/PageHeader";
import CopyButton from "../components/CopyButton";
import ResourceTable, { type ResourceColumn } from "../components/resourceTable/ResourceTable";
import { useIsAdmin } from "../components/useIsAdmin";
import { api, usePolling, type KubeCluster, type List } from "../api";
import { apiErrorText } from "../apiError";
import { formatTime } from "../i18n/datetime";
import { KUBE_STALLED_MINUTES, cardReturnText, kubeClusterStalled, kubeTroubleshootCommands } from "../kubeClusters";
import KubeClusterDrawer from "./kubernetes/KubeClusterDrawer";
import KubeManifestModal from "./kubernetes/KubeManifestModal";
import KubeClusterOperationModal from "./kubernetes/KubeClusterOperationModal";

/** Why a cluster's agents may not have joined, and the commands that tell which. */
function StalledHelp({ cluster }: { cluster: KubeCluster }) {
  const { t } = useTranslation("kubernetes");
  const commands = kubeTroubleshootCommands(cluster.namespace);
  return (
    <Space direction="vertical" size={8} style={{ maxWidth: 520 }}>
      <Typography.Text>{t("stalled.intro", { minutes: KUBE_STALLED_MINUTES })}</Typography.Text>
      <div style={{ display: "flex", gap: 4, alignItems: "flex-start" }}>
        <pre aria-label={t("stalled.copy")} style={{ margin: 0, flex: 1, minWidth: 0, overflowX: "auto", fontSize: 12 }}>{commands}</pre>
        <CopyButton size="small" label={t("stalled.copy")} text={commands} />
      </div>
      <Typography.Text strong>{t("stalled.causes")}</Typography.Text>
      <ul style={{ margin: 0, paddingInlineStart: 20 }}>
        {(["pull", "runtimeClass", "selector", "connect"] as const).map((key) => <li key={key}>{t(`stalled.${key}`)}</li>)}
      </ul>
    </Space>
  );
}

/** The Kubernetes clusters whose GPU nodes join this gpuworks, one line each. */
export default function KubernetesPage() {
  const { t } = useTranslation("kubernetes");
  const { t: tCommon } = useTranslation("common");
  const { t: tProviders } = useTranslation("providers");
  const isAdmin = useIsAdmin();
  const clusters = usePolling<List<KubeCluster>>(api.kubeClusters, 15000);
  const [adding, setAdding] = useState(false);
  const [manifestOf, setManifestOf] = useState<KubeCluster | null>(null);
  const [operation, setOperation] = useState<{ cluster: KubeCluster; op: "apply" | "remove" } | null>(null);
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
    { key: "vendor", title: t("column.vendor"), width: 110, sortValue: (c) => c.vendor, render: (_, c) => c.vendor ? tCommon(`vendor.${c.vendor}`, { defaultValue: c.vendor }) : "-" },
    {
      key: "source",
      title: t("column.source"),
      width: 220,
      render: (_, c) => c.source
        ? (
          <Space direction="vertical" size={0}>
            <Typography.Text>{c.source.provider_name || tProviders(`kind.${c.source.provider_kind}`, { defaultValue: c.source.provider_kind })}</Typography.Text>
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>{c.source.cluster_id}</Typography.Text>
          </Space>
        )
        : <Typography.Text type="secondary">{t("source.byHand")}</Typography.Text>,
    },
    { key: "namespace", title: t("column.namespace"), width: 160, sortValue: (c) => c.namespace, render: (_, c) => <Typography.Text code>{c.namespace}</Typography.Text> },
    { key: "nodes", title: t("column.nodes"), width: 90, align: "right", sortValue: (c) => c.node_count, render: (_, c) => c.node_count },
    {
      key: "cluster_uid",
      title: t("column.clusterUid"),
      width: 300,
      render: (_, c) => c.cluster_uid
        ? <Typography.Text code>{c.cluster_uid}</Typography.Text>
        : kubeClusterStalled(c) ? (
          <Space size={4}>
            <Tag color="warning">{t("clusterUid.notEnrolled")}</Tag>
            <Popover trigger="click" title={t("stalled.title")} content={<StalledHelp cluster={c} />}>
              <Button type="link" size="small" style={{ padding: 0 }}>{t("clusterUid.troubleshoot")}</Button>
            </Popover>
          </Space>
        ) : <Tooltip title={t("clusterUid.notEnrolledHint")}><Tag>{t("clusterUid.notEnrolled")}</Tag></Tooltip>,
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
      width: 230,
      render: (_, c) => (
        <Space size={2}>
          {c.source && (
            <>
              <Tooltip title={t("actions.apply")}>
                <Button type="text" size="small" icon={<CloudUploadOutlined />} aria-label={t("actions.apply")} disabled={c.revoked} onClick={() => setOperation({ cluster: c, op: "apply" })} />
              </Tooltip>
              <Tooltip title={t("actions.remove")}>
                <Button type="text" size="small" danger icon={<DisconnectOutlined />} aria-label={t("actions.remove")} onClick={() => setOperation({ cluster: c, op: "remove" })} />
              </Tooltip>
            </>
          )}
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
      {operation && <KubeClusterOperationModal cluster={operation.cluster} op={operation.op} onClose={() => setOperation(null)} onDone={() => void clusters.refetch()} />}
    </div>
  );
}
