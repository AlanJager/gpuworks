import { useCallback, useEffect, useState } from "react";
import { Alert, Button, Space, Table, Tag, Tooltip, Typography } from "antd";
import type { ColumnsType } from "antd/es/table";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { api, type ProviderCluster, type ProviderClusterPool } from "../api";
import KubeClusterDrawer, { type KubeClusterPrefill } from "../pages/kubernetes/KubeClusterDrawer";

/**
 * A cloud account's managed Kubernetes clusters, read live from the cloud.
 * Each GPU node pool can be connected as a cluster entry; the entry's
 * manifest is then applied by gpuworks on confirmation, or with kubectl.
 */
export default function ProviderClustersTable({ providerId, providerName }: { providerId: number; providerName: string }) {
  const { t } = useTranslation("providers");
  const [rows, setRows] = useState<ProviderCluster[] | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [connecting, setConnecting] = useState<KubeClusterPrefill | null>(null);

  const load = useCallback(() => {
    setRows(null);
    setLoadError(null);
    api.providerClusters(providerId)
      .then((list) => setRows(list.items ?? []))
      .catch((e: Error) => { setRows([]); setLoadError(e.message); });
  }, [providerId]);
  useEffect(load, [load]);

  const connect = (c: ProviderCluster, pool: ProviderClusterPool) => setConnecting({
    name: c.gpu_pools.length > 1 ? `${c.name}-${pool.name}` : c.name,
    vendor: pool.vendor,
    node_selector: pool.node_selector,
    provider_id: providerId,
    provider_cluster_id: c.external_id,
    source_label: `${providerName} · ${c.name} (${c.external_id})`,
  });

  const columns: ColumnsType<ProviderCluster> = [
    {
      title: t("clusters.column.name"),
      key: "name",
      width: 240,
      render: (_, c) => (
        <Space direction="vertical" size={0}>
          <Typography.Text strong>{c.name}</Typography.Text>
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>{c.external_id}</Typography.Text>
        </Space>
      ),
    },
    { title: t("clusters.column.region"), dataIndex: "region", width: 130 },
    { title: t("clusters.column.version"), dataIndex: "version", width: 150, render: (v: string) => v || "—" },
    {
      title: t("clusters.column.state"),
      key: "state",
      width: 170,
      render: (_, c) => (
        <Space size={4} wrap>
          <Tag color={c.running ? "green" : "default"}>{c.running ? t("clusters.running") : c.state}</Tag>
          <Tooltip title={c.public_api ? undefined : t("clusters.privateAPIHint")}>
            <Tag color={c.public_api ? "blue" : "gold"}>{c.public_api ? t("clusters.publicAPI") : t("clusters.privateAPI")}</Tag>
          </Tooltip>
        </Space>
      ),
    },
    {
      title: t("clusters.column.gpuPools"),
      key: "pools",
      render: (_, c) => {
        const pools = c.gpu_pools.map((p) => (
          <Typography.Text key={p.id}>
            {t("clusters.pool", { name: p.name, model: p.gpu_model || t("clusters.mixedModels"), count: p.gpu_count, nodes: p.nodes })}
          </Typography.Text>
        ));
        if (c.entry) {
          return (
            <Space direction="vertical" size={4}>
              {pools}
              <Space size={6} wrap>
                <Tag color="green">{t("clusters.connected")}</Tag>
                <Link to="/kubernetes">{c.entry.name}</Link>
              </Space>
            </Space>
          );
        }
        if (!c.running) return <Typography.Text type="secondary">{t("clusters.notRunning")}</Typography.Text>;
        if (c.gpu_pools.length === 0) return <Typography.Text type="secondary">{t("clusters.noGPUPools")}</Typography.Text>;
        return (
          <Space direction="vertical" size={4}>
            {c.gpu_pools.map((p) => (
              <Space key={p.id} size={8} wrap>
                <Typography.Text>
                  {t("clusters.pool", { name: p.name, model: p.gpu_model || t("clusters.mixedModels"), count: p.gpu_count, nodes: p.nodes })}
                </Typography.Text>
                <Button size="small" type="primary" onClick={() => connect(c, p)}>{t("clusters.connect")}</Button>
              </Space>
            ))}
          </Space>
        );
      },
    },
  ];

  return (
    <>
      <Space style={{ marginBottom: 12 }} wrap>
        <Typography.Text type="secondary">{t("clusters.intro")}</Typography.Text>
        <Button size="small" onClick={load} loading={rows === null}>{t("clusters.refresh")}</Button>
      </Space>
      {loadError && <Alert type="error" showIcon style={{ marginBottom: 12 }} message={t("clusters.loadFailed")} description={loadError} />}
      <Table<ProviderCluster>
        rowKey="external_id"
        size="small"
        columns={columns}
        dataSource={rows ?? []}
        loading={rows === null}
        pagination={false}
        locale={{ emptyText: t("clusters.empty") }}
      />
      <KubeClusterDrawer
        open={connecting !== null}
        prefill={connecting ?? undefined}
        onClose={() => setConnecting(null)}
        onCreated={load}
      />
    </>
  );
}
