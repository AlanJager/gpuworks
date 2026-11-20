import { useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { api, type CloudProvider, type KubeCluster, type ProviderInstance, type ProviderKindInfo } from "../../api";
import ProviderInstallModal from "../../components/ProviderInstallModal";
import { Badge } from "../../components/ui/badge";
import { Button } from "../../components/ui/button";
import { Table, TableBody, TableHead, TableHeader, TableRow } from "../../components/ui/table";
import { message } from "../../ui-kit/feedback";
import { Notice } from "../../ui-kit/Notice";
import { ConsoleIcon } from "../../ui-kit/consoleIcons";
import { formatTime } from "../../i18n/datetime";
import { Spinner } from "../../ui-kit/Spinner";
import KubeClusterDrawer from "../kubernetes/KubeClusterDrawer";
import type { SlurmCluster } from "../../slurmApi";
import { slurmEntryOf } from "../../slurmClusters";
import ClusterGroup from "./ClusterGroup";
import HpcClusterGroup from "./HpcClusterGroup";
import { SpanRow, TableGroup } from "./GroupRows";
import InstanceRow, { WIDE_ONLY, type InstanceColumns } from "./InstanceRow";
import { PlatformResourceBadges } from "./PlatformResourceBadge";
import { useAccountClusters } from "./useAccountClusters";
import { useAccountHPCClusters } from "./useAccountHPCClusters";

// An account can hold hundreds of instances: its group shows this many, then more on request.
const INSTANCE_STEP = 10;
const INSTANCES_GROUP = "instances";

/**
 * What an account holds, under its row, in one table grouped by what the
 * machines belong to: the account's own instances, then each HPC cluster
 * (E-HPC) with its Slurm entry, then each of its managed Kubernetes clusters
 * with the GPU node pools to connect. An account with neither has its
 * instances alone, ungrouped. Instances are as the last sync found them;
 * both kinds of cluster are read from the cloud when the row opens and after
 * each sync, each with its own loading, empty and failure states: they are
 * separate cloud products, so one failing leaves the other.
 */
export default function ProviderResources({ provider, kindInfo, refreshKey, entries, slurmEntries = [], onEntriesChanged, admin, focusCluster, onSync, syncing = false }: {
  provider: CloudProvider;
  kindInfo?: ProviderKindInfo;
  /** Changes when the account's instances were synced: they are read again. */
  refreshKey: string;
  /** The cluster entries the page polls, by UUID. */
  entries: Record<string, KubeCluster>;
  /** The Slurm entries the page polls: the ones made from this account's E-HPC clusters sit in their cluster's group. */
  slurmEntries?: SlurmCluster[];
  onEntriesChanged: () => void;
  admin: boolean;
  /** The cluster entry (UUID) to bring into view once the clusters are read, as a compute node's source asks. */
  focusCluster?: string | null;
  /** Syncs the account's instances from the vendor, as the row's own sync does; absent for a kind that cannot list them. */
  onSync?: () => void;
  syncing?: boolean;
}) {
  const { t } = useTranslation("providers");
  const { t: tCommon } = useTranslation("common");
  const root = useRef<HTMLDivElement>(null);
  const [instances, setInstances] = useState<ProviderInstance[] | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [shown, setShown] = useState(INSTANCE_STEP);
  const [installFor, setInstallFor] = useState<ProviderInstance | null>(null);
  const [powering, setPowering] = useState<string | null>(null);
  const [folded, setFolded] = useState<ReadonlySet<string>>(new Set());

  const resources = provider.resources ?? [];
  // Clusters (ACK, E-HPC) are the groups of their own, not a kind of instance.
  const instanceResources = resources.filter((resource) => resource.type !== "kubernetes" && resource.type !== "slurm");
  // The account has clusters when the server says it connects them (its row's resources); an account that already
  // holds entries always shows them.
  const hasClusters = resources.some((resource) => resource.type === "kubernetes")
    || Object.values(entries).some((entry) => entry.source?.provider_id === provider.id);
  const hasHPC = resources.some((resource) => resource.type === "slurm")
    || slurmEntries.some((entry) => entry.source?.provider_id === provider.id);
  const clusters = useAccountClusters({ providerId: provider.id, providerName: provider.name, enabled: hasClusters, onChanged: onEntriesChanged });
  const hpc = useAccountHPCClusters({ providerId: provider.id, enabled: hasHPC });

  // Only the latest read lands: one that answers after a newer one started, or after the row closed, is dropped.
  const readRef = useRef(0);
  const load = () => {
    const read = ++readRef.current;
    api.providerInstances(provider.id)
      .then((list) => { if (read !== readRef.current) return; setInstances(list.items); setLoadError(null); })
      .catch((error: Error) => { if (read !== readRef.current) return; setInstances((current) => current ?? []); setLoadError(error.message); });
  };
  useEffect(() => {
    load();
    return () => { readRef.current++; };
  }, [provider.id, refreshKey]);
  // A sync changes the refresh key: the clusters are read from the cloud again with it, so one sync brings both up to date.
  const firstKey = useRef(refreshKey);
  useEffect(() => {
    if (refreshKey !== firstKey.current) { clusters.load(); hpc.load({ fresh: true }); }
  }, [refreshKey]);

  const power = async (inst: ProviderInstance, action: "on" | "off") => {
    setPowering(`${inst.external_id}:${action}`);
    try {
      await api.powerProviderInstance(provider.id, inst.external_id, action);
      message.success(t(action === "on" ? "instances.message.powerOnSent" : "instances.message.powerOffSent"));
      load();
    } catch (e) {
      message.error(t("instances.message.actionFailed", { detail: (e as Error).message }));
    } finally {
      setPowering(null);
    }
  };

  // The cluster a compute node's source points at is brought into view once its group is drawn: the groups come
  // from the cloud's two listings and the page's entries, which answer in any order, so it is looked for again
  // whenever one of them changes and only counts as done when the group was found.
  const focused = useRef(false);
  useEffect(() => {
    if (!focusCluster || focused.current) return;
    const group = root.current?.querySelector(`[data-cluster-entry="${CSS.escape(focusCluster)}"]`);
    if (!group) return;
    focused.current = true;
    group.scrollIntoView?.({ block: "center" });
  }, [focusCluster, clusters.clusters, hpc.clusters, entries, slurmEntries, folded]);

  const columns: InstanceColumns = {
    price: kindInfo?.capabilities.has_pricing === true,
    region: provider.category !== "private_cloud",
    // Said per instance only where the account's instances are not all of one kind.
    type: instanceResources.length !== 1,
  };
  const titles: Array<{ title: string; className?: string }> = [
    { title: t("instances.column.instance") }, { title: t("instances.column.gpu") }, { title: t("instances.column.status") },
    ...(columns.price ? [{ title: t("instances.column.price") }] : []),
    ...(columns.region ? [{ title: t("instances.column.region"), className: WIDE_ONLY }] : []),
    ...(columns.type ? [{ title: t("instances.column.type") }] : []),
    { title: t("instances.column.sshConnection") }, { title: tCommon("resourceTable.actions") },
  ];
  const colSpan = titles.length;
  const toggle = (group: string) => setFolded((current) => {
    const next = new Set(current);
    if (!next.delete(group)) next.add(group);
    return next;
  });

  // The server's instance list is the account's own: a cluster's nodes are the cluster's, not instances.
  const items = instances ?? [];
  const grouped = hasClusters || hasHPC;
  const row = (inst: ProviderInstance, index: number) => (
    <InstanceRow key={inst.external_id} inst={inst} index={index} columns={columns} indent={grouped}
      powering={powering === `${inst.external_id}:on` ? "on" : powering === `${inst.external_id}:off` ? "off" : null}
      onPower={(action) => power(inst, action)} onInstall={() => setInstallFor(inst)} />
  );
  const instanceRows = (
    <>
      {instances === null && <SpanRow colSpan={colSpan}><Spinner label={t("detail.loadingInstances")} /></SpanRow>}
      {instances !== null && items.length === 0 && !loadError && <SpanRow colSpan={colSpan} className="text-center text-fg-muted">{t(provider.synced_at ? "instances.emptyAfterSync" : "instances.empty")}</SpanRow>}
      {items.slice(0, shown).map(row)}
      {items.length > shown && (
        <SpanRow colSpan={colSpan} className={grouped ? "ps-11" : undefined}>
          <Button variant="link" size="sm" onClick={() => setShown(shown + INSTANCE_STEP)}>{t("detail.showMore", { count: Math.min(INSTANCE_STEP, items.length - shown), rest: items.length - shown })}</Button>
        </SpanRow>
      )}
    </>
  );

  // The account's entries the cloud's listing does not account for: their cluster is gone from the account, or the
  // listing failed. They and their nodes are still here, so they are listed and can still be revoked, removed or deleted.
  const listed = new Set((clusters.clusters ?? []).map((c) => c.external_id));
  const unlisted = clusters.clusters === null ? [] : Object.values(entries)
    .filter((entry) => entry.source?.provider_id === provider.id && !listed.has(entry.source.cluster_id))
    .sort((a, b) => a.name.localeCompare(b.name));
  // The same for the Slurm entries made from this account's HPC clusters.
  const hpcListed = new Set((hpc.clusters ?? []).map((c) => c.external_id));
  const hpcUnlisted = hpc.clusters === null ? [] : slurmEntries
    .filter((entry) => entry.source?.provider_id === provider.id && !hpcListed.has(entry.source.provider_cluster_id))
    .sort((a, b) => a.name.localeCompare(b.name));
  const joined = items.filter((inst) => inst.node_id !== 0).length;

  return (
    // The detail steps in under its row, behind a rule, so it reads as the row's (ui-guidelines 4.1).
    <div ref={root} className="ms-6 flex flex-col gap-2 border-s-2 border-border ps-3 [container-type:inline-size]">
      {/* Syncing lists the account's instances from the vendor again, and with them its clusters. */}
      <div className="flex min-h-7 flex-wrap items-center gap-2 text-body text-fg-muted">
        {onSync && <Button size="sm" variant="secondary" leadingIcon={ConsoleIcon.refresh} onClick={onSync} loading={syncing}>{t("table.sync")}</Button>}
        {/* A kind that cannot list its instances is never synced: it says nothing about it. */}
        {onSync && <span>{provider.synced_at ? t("detail.syncedAt", { time: formatTime(provider.synced_at) }) : t("detail.neverSynced")}</span>}
      </div>
      {/* Said where it applies: for a kind that connects container instances, or one whose resources are not known. */}
      {(!kindInfo?.resources?.length || kindInfo.resources.some((resource) => resource.type === "container")) && <Notice tone="info" title={t("instances.containerRuntimeNote")} />}
      {loadError && <Notice tone="danger" title={t("instances.loadFailed")} children={loadError} action={<Button variant="secondary" size="sm" onClick={load}>{t("instances.retry")}</Button>} />}
      {clusters.loadError && <Notice tone="danger" title={t("clusters.loadFailed")} children={clusters.loadError} action={<Button variant="secondary" size="sm" onClick={clusters.load}>{t("instances.retry")}</Button>} />}
      {/* What the cloud lets this account list is said quietly: no permission or no activation is not a failure, only an empty list. */}
      {hpc.listing?.access === "permission_missing" && <Notice tone="info" title={t("hpc.noPermission", { permission: hpc.listing.permission })} />}
      {hpc.listing?.access === "not_activated" && <Notice tone="info" title={t("hpc.notActivated")}>{hpc.listing.code ? t("hpc.notActivatedCode", { code: hpc.listing.code }) : undefined}</Notice>}
      {hpc.listing?.partial && (
        <Notice tone="warning" title={t("hpc.partial")}>
          <ul className="m-0 list-disc ps-5">
            {hpc.listing.failed_regions.map((f) => (
              <li key={f.region}>{f.permission ? t("hpc.failedRegionPermission", { region: f.region, permission: f.permission }) : t("hpc.failedRegion", { region: f.region, code: f.code || f.message })}</li>
            ))}
          </ul>
        </Notice>
      )}
      {hpc.loadError && <Notice tone="danger" title={t("hpc.loadFailed")} children={hpc.loadError} action={<Button variant="secondary" size="sm" onClick={() => hpc.load({ fresh: true })}>{t("instances.retry")}</Button>} />}
      <Table variant="bordered" size="md" aria-busy={instances === null || (hasClusters && clusters.clusters === null) || (hasHPC && hpc.clusters === null)}>
        <TableHeader>
          <TableRow>{titles.map(({ title, className }) => <TableHead key={title} className={`whitespace-nowrap ${className ?? ""}`}>{title}</TableHead>)}</TableRow>
        </TableHeader>
        {!grouped ? <TableBody>{instanceRows}</TableBody> : (
          <>
            <TableGroup
              name={t("detail.standaloneInstances")}
              colSpan={colSpan}
              open={!folded.has(INSTANCES_GROUP)}
              onToggle={() => toggle(INSTANCES_GROUP)}
              title={
                <>
                  <strong className="font-semibold text-fg-default">{t("detail.standaloneInstances")}</strong>
                  {instanceResources.length > 0 && <PlatformResourceBadges resources={instanceResources} />}
                  {instances !== null && <Badge size="sm">{items.length}</Badge>}
                </>
              }
              extra={joined > 0 && <span className="text-body text-fg-muted">{t("clusters.field.nodes")} {joined}</span>}
            >
              {instanceRows}
            </TableGroup>
            {hasHPC && hpc.clusters === null && <TableBody><SpanRow colSpan={colSpan}><Spinner label={t("detail.loadingHPCClusters")} /></SpanRow></TableBody>}
            {hasHPC && hpc.clusters !== null && hpc.clusters.length === 0 && hpcUnlisted.length === 0 && !hpc.loadError && hpc.listing?.access === "ok" && (
              <TableBody><SpanRow colSpan={colSpan} className="text-center text-fg-muted">{t("hpc.empty")}</SpanRow></TableBody>
            )}
            {(hpc.clusters ?? []).map((cluster) => (
              <HpcClusterGroup key={`hpc:${cluster.external_id}`} cluster={cluster} entry={slurmEntryOf(slurmEntries, provider.id, cluster.external_id)}
                provider={provider} colSpan={colSpan} open={!folded.has(`hpc:${cluster.external_id}`)} onToggle={() => toggle(`hpc:${cluster.external_id}`)}
                admin={admin} onChanged={onEntriesChanged} />
            ))}
            {hpcUnlisted.map((entry) => (
              <HpcClusterGroup key={`hpc:${entry.uuid}`} entry={entry} unread={!!hpc.loadError || hpc.listing?.access !== "ok" || !!hpc.listing?.partial} provider={provider}
                colSpan={colSpan} open={!folded.has(`hpc:${entry.uuid}`)} onToggle={() => toggle(`hpc:${entry.uuid}`)} admin={admin} onChanged={onEntriesChanged} />
            ))}
            {hasClusters && clusters.clusters === null && <TableBody><SpanRow colSpan={colSpan}><Spinner label={t("detail.loadingClusters")} /></SpanRow></TableBody>}
            {hasClusters && clusters.clusters !== null && clusters.clusters.length === 0 && unlisted.length === 0 && !clusters.loadError && (
              <TableBody><SpanRow colSpan={colSpan} className="text-center text-fg-muted">{t("clusters.empty")}</SpanRow></TableBody>
            )}
            {(clusters.clusters ?? []).map((cluster) => (
              <ClusterGroup key={cluster.external_id} cluster={cluster} entry={cluster.entry ? entries[cluster.entry.uuid] : undefined}
                colSpan={colSpan} open={!folded.has(cluster.external_id)} onToggle={() => toggle(cluster.external_id)} clusters={clusters} admin={admin} />
            ))}
            {unlisted.map((entry) => (
              <ClusterGroup key={entry.uuid} entry={entry} unread={!!clusters.loadError}
                colSpan={colSpan} open={!folded.has(entry.uuid)} onToggle={() => toggle(entry.uuid)} clusters={clusters} admin={admin} />
            ))}
          </>
        )}
      </Table>
      <ProviderInstallModal
        providerId={provider.id}
        providerKind={kindInfo?.kind}
        defaultServerDialListen={kindInfo?.default_server_dial_listen}
        serverDialSupported={kindInfo?.server_dial_supported ?? true}
        serverDialAutoPort={kindInfo?.server_dial_auto_port ?? false}
        inst={installFor}
        onClose={() => setInstallFor(null)}
        onDone={() => { setInstallFor(null); load(); }}
      />
      <KubeClusterDrawer open={clusters.connecting !== null} prefill={clusters.connecting ?? undefined} onClose={clusters.closeConnect} onCreated={clusters.reload} />
    </div>
  );
}
