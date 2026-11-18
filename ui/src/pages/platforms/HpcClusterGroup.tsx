import { useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import type { ProviderHPCCluster } from "../../api";
import type { SlurmCluster } from "../../slurmApi";
import { Badge } from "../../components/ui/badge";
import { Button } from "../../components/ui/button";
import SlurmClusterDrawer from "../slurm/SlurmClusterDrawer";
import SlurmEntryActions from "../slurm/SlurmEntryActions";
import SlurmEntryDetail, { slurmCountsText } from "../slurm/SlurmEntryDetail";
import SlurmEntryStatus from "../slurm/SlurmEntryStatus";
import FactList from "./FactList";
import { SpanRow, TableGroup } from "./GroupRows";

/**
 * One HPC cluster (Alibaba Cloud E-HPC) of a cloud account, as a group of the
 * account's table, laid out like a managed Kubernetes cluster's
 * (ClusterGroup): the header names the cluster and says where its Slurm entry
 * stands, with what can be done to it (connecting it, or an administrator's
 * actions on its entry), and under it what the cluster is as "label value"
 * pairs, with the entry's own facts once there is one. The compute nodes are
 * not listed: they are queue-managed and join through the entry by
 * themselves, so what is shown is how many of them have a GPU and, once
 * joined, the nodes themselves, which the node count opens.
 *
 * `cluster` is the cloud's live listing of it and `entry` the Slurm entry made
 * from it. An entry the listing does not account for (its cluster is gone
 * from the account, or the listing failed) has no `cluster`: it is still
 * listed, so it can be revoked, uninstalled or deleted.
 */
export default function HpcClusterGroup({ cluster, entry, unread = false, provider, colSpan, open, onToggle, admin, onChanged }: {
  cluster?: ProviderHPCCluster;
  /** The Slurm entry made from this cluster, if any. */
  entry?: SlurmCluster;
  /** The listing failed, so an entry without a cluster may still have one in the cloud. */
  unread?: boolean;
  provider: { id: number; name: string };
  colSpan: number;
  open: boolean;
  onToggle: () => void;
  admin: boolean;
  /** An entry was made, changed or deleted. */
  onChanged: () => void;
}) {
  const { t } = useTranslation("providers");
  const { t: tSlurm } = useTranslation("slurm");
  const [connecting, setConnecting] = useState(false);
  const name = cluster?.name ?? entry?.name ?? "";
  const id = cluster?.external_id ?? entry?.source?.provider_cluster_id;
  const schedulerName = cluster ? t(`hpc.scheduler.${cluster.scheduler}`, { defaultValue: cluster.scheduler || t("hpc.scheduler.unknown") }) : "";
  const schedulerText = [schedulerName, cluster?.scheduler_version].filter(Boolean).join(" ");

  // The cloud's state of the cluster, in the server's code and tone; it is said once, here, and not again among the facts.
  const stateBadge = cluster && (
    <Badge status={cluster.state_tone}>{t(`hpc.state.${cluster.state}`, { state: cluster.vendor_state || "—" })}</Badge>
  );
  // Where the cluster stands: its entry's state (and the cluster's own when it is not running), or that it has none yet.
  let status: ReactNode = null;
  if (entry) {
    status = (
      <>
        <SlurmEntryStatus cluster={entry} />
        {cluster && !cluster.running && stateBadge}
        <span className="text-body text-fg-muted">{slurmCountsText(tSlurm, entry)}</span>
      </>
    );
  } else if (cluster?.entry) status = <Badge status="success">{t("clusters.connected")}</Badge>;
  else if (cluster?.connectable) status = <Badge status="neutral">{tSlurm("status.pending")}</Badge>;
  else status = stateBadge;

  const facts: Array<[string, ReactNode]> = [];
  if (id) facts.push([t("clusters.field.id"), <span className="break-all">{id}</span>]);
  if (cluster) {
    if (cluster.region) facts.push([t("clusters.field.region"), cluster.region]);
    facts.push([t("hpc.field.scheduler"), schedulerText]);
    // Once an entry exists its header counts what the agents report. Before
    // that, the server gives the cloud's live count of compute nodes, or, when
    // the listing has none, the last sync's GPU nodes; an unknown count is not
    // shown as zero.
    if (!entry && cluster.compute_nodes != null) facts.push([t("hpc.field.computeNodes"), cluster.compute_nodes]);
    else if (!entry && cluster.gpu_nodes != null) facts.push([t("hpc.field.gpuNodes"), cluster.gpu_nodes]);
  }
  // Said once where it applies: the cluster is not one the platform can connect, or has gone from the account.
  let note: ReactNode = null;
  if (!cluster) note = t(unread ? "hpc.entryUnread" : "hpc.entryGone");
  else if (cluster.unsupported_reason === "scheduler_not_slurm") note = t("hpc.unsupported", { scheduler: schedulerName });
  else if (cluster.not_connectable_reason) note = t(`hpc.notConnectable.${cluster.not_connectable_reason}`);

  return (
    <TableGroup
      name={name}
      colSpan={colSpan}
      open={open}
      onToggle={onToggle}
      data-cluster-entry={entry?.uuid}
      title={
        <>
          <strong className="min-w-0 truncate font-semibold text-fg-default">{name}</strong>
          {cluster?.scheduler && <Badge size="sm" color="blue">{schedulerText}</Badge>}
        </>
      }
      extra={
        <>
          {status}
          {admin && !entry && cluster?.connectable && <Button size="sm" variant="primary" onClick={() => setConnecting(true)}>{tSlurm("hpc.connect")}</Button>}
          {admin && entry && <SlurmEntryActions cluster={entry} onChanged={onChanged} labeled />}
        </>
      }
    >
      <SpanRow colSpan={colSpan} className="ps-11">
        {entry
          ? <SlurmEntryDetail cluster={entry} leadFacts={facts} admin={admin} onChanged={onChanged} />
          // The facts' own box is the size container: a table cell cannot be one.
          : <div className="[container-type:inline-size]"><FactList facts={facts} /></div>}
        {note && <p className="m-0 mt-2 text-body text-fg-muted">{note}</p>}
      </SpanRow>
      {connecting && cluster && (
        <SlurmClusterDrawer open onClose={() => setConnecting(false)} onCreated={onChanged}
          prefill={{ name, provider_id: provider.id, provider_cluster_id: cluster.external_id, source_label: `${provider.name} · ${name}` }} />
      )}
    </TableGroup>
  );
}
