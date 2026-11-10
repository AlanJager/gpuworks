import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { api, type ProviderCluster, type ProviderClusterPool } from "../api";
import KubeClusterDrawer, { type KubeClusterPrefill } from "../pages/kubernetes/KubeClusterDrawer";
import { Badge } from "./ui/badge";
import { Button } from "./ui/button";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "./ui/table";
import { Tooltip } from "./ui/tooltip";
import { Notice } from "../ui-kit/Notice";
import { Spinner } from "../ui-kit/Spinner";

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

  const poolText = (p: ProviderClusterPool) => t("clusters.pool", { name: p.name, model: p.gpu_model || t("clusters.mixedModels"), count: p.gpu_count, nodes: p.nodes });

  const pools = (c: ProviderCluster) => {
    if (c.entry) {
      return (
        <div className="flex flex-col gap-1">
          {c.gpu_pools.map((p) => <span key={p.id}>{poolText(p)}</span>)}
          <div className="flex flex-wrap items-center gap-1.5">
            <Badge status="success">{t("clusters.connected")}</Badge>
            <Link to="/kubernetes">{c.entry.name}</Link>
          </div>
        </div>
      );
    }
    if (!c.running) return <span className="text-fg-muted">{t("clusters.notRunning")}</span>;
    if (c.gpu_pools.length === 0) return <span className="text-fg-muted">{t("clusters.noGPUPools")}</span>;
    return (
      <div className="flex flex-col gap-1">
        {c.gpu_pools.map((p) => (
          <div key={p.id} className="flex flex-wrap items-center gap-2">
            <span>{poolText(p)}</span>
            <Button size="sm" variant="primary" onClick={() => connect(c, p)}>{t("clusters.connect")}</Button>
          </div>
        ))}
      </div>
    );
  };

  const items = rows ?? [];
  return (
    <>
      <div className="mb-3 flex flex-wrap items-center gap-2">
        <span className="text-fg-muted">{t("clusters.intro")}</span>
        <Button size="sm" variant="secondary" onClick={load} loading={rows === null}>{t("clusters.refresh")}</Button>
      </div>
      {loadError && <div className="mb-3"><Notice tone="danger" title={t("clusters.loadFailed")} children={loadError} /></div>}
      {rows === null && <Spinner label={t("clusters.refresh")} />}
      <Table variant="bordered" size="sm" aria-busy={rows === null}>
        <TableHeader>
          <TableRow>
            <TableHead style={{ width: 240 }}>{t("clusters.column.name")}</TableHead>
            <TableHead style={{ width: 130 }}>{t("clusters.column.region")}</TableHead>
            <TableHead style={{ width: 150 }}>{t("clusters.column.version")}</TableHead>
            <TableHead style={{ width: 170 }}>{t("clusters.column.state")}</TableHead>
            <TableHead>{t("clusters.column.gpuPools")}</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {items.map((c, index) => (
            <TableRow key={c.external_id} index={index}>
              <TableCell>
                <div className="flex min-w-0 flex-col">
                  <span>{c.name}</span>
                  <span className="text-label text-fg-muted">{c.external_id}</span>
                </div>
              </TableCell>
              <TableCell>{c.region}</TableCell>
              <TableCell>{c.version || "—"}</TableCell>
              <TableCell>
                <div className="flex flex-wrap items-center gap-1">
                  <Badge status={c.running ? "success" : "neutral"}>{c.running ? t("clusters.running") : c.state}</Badge>
                  {c.public_api
                    ? <Badge status="info">{t("clusters.publicAPI")}</Badge>
                    : <Tooltip content={t("clusters.privateAPIHint")}><Badge status="warning">{t("clusters.privateAPI")}</Badge></Tooltip>}
                </div>
              </TableCell>
              <TableCell>{pools(c)}</TableCell>
            </TableRow>
          ))}
          {rows !== null && items.length === 0 && (
            <TableRow><TableCell colSpan={5} className="text-center text-fg-muted">{t("clusters.empty")}</TableCell></TableRow>
          )}
        </TableBody>
      </Table>
      <KubeClusterDrawer
        open={connecting !== null}
        prefill={connecting ?? undefined}
        onClose={() => setConnecting(null)}
        onCreated={load}
      />
    </>
  );
}
