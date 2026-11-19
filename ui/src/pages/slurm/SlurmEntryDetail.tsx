import { useState, type ReactNode } from "react";
import type { TFunction } from "i18next";
import { Link, useNavigate } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { formatTime } from "../../i18n/datetime";
import { slurmClusterNodesPath } from "../../nodeSources";
import type { SlurmCluster } from "../../slurmApi";
import { slurmDurationText } from "../../slurmClusters";
import FactList from "../platforms/FactList";
import SlurmApplyLead from "./SlurmApplyLead";
import SlurmMissingNodes from "./SlurmMissingNodes";
import SlurmSetupCommand from "./SlurmSetupCommand";
import { Button } from "../../components/ui/button";
import { storageAddState } from "../storage/storagePreset";
import { Accordion, AccordionContent, AccordionItem, AccordionTrigger } from "../../components/ui/accordion";
import { slurmWaitingText } from "./slurmReach";

/**
 * How many of the cluster's GPU nodes have an agent online: "GPU nodes 8, 6
 * online, 1 offline", with how old the list is while no agent is online to
 * update it; before the first report only the joined nodes.
 */
export function slurmCountsText(t: TFunction<"slurm">, cluster: SlurmCluster): string {
  const offline = cluster.node_count - cluster.online_count;
  const tail = offline > 0 ? t("counts.offline", { count: offline }) : "";
  if (cluster.slurm_gpu_nodes > 0) {
    return cluster.list_as_of
      ? t("counts.lineAsOf", { gpu: cluster.slurm_gpu_nodes, online: cluster.online_count, time: formatTime(cluster.list_as_of) }) + tail
      : t("counts.line", { gpu: cluster.slurm_gpu_nodes, online: cluster.online_count }) + tail;
  }
  return t("counts.onlineOnly", { count: cluster.online_count }) + tail;
}

/**
 * A Slurm entry under its row (or its cloud cluster's group): what it is, laid
 * out flat as "label value" pairs, the GPU nodes Slurm lists that have no
 * agent online, and, until a node has joined, why none has and how to connect
 * it: for an entry the platform can set up itself the labelled Check and apply
 * button, with the commands folded under "Run it by hand"; for any other the
 * commands. `connectedBy` adds how a cluster of its own was connected;
 * `leadFacts` are the facts of the cloud cluster it was made from, which come
 * first; `admin` offers to remove a joined node that is offline; `onChanged`
 * runs after an apply or a removal changed the entry.
 */
export default function SlurmEntryDetail({ cluster, connectedBy, leadFacts = [], admin = false, onChanged }: { cluster: SlurmCluster; connectedBy?: ReactNode; leadFacts?: Array<[string, ReactNode]>; admin?: boolean; onChanged?: () => void }) {
  const { t } = useTranslation("slurm");
  const navigate = useNavigate();
  const facts: Array<[string, ReactNode]> = [...leadFacts];
  if (connectedBy) facts.push([t("fact.connectedBy"), connectedBy]);
  facts.push(
    [t("fact.clusterName"), cluster.slurm_cluster_name ? <code>{cluster.slurm_cluster_name}</code> : t("fact.notBound")],
    [t("fact.nodes"), cluster.node_count > 0
      ? <Link className="text-fg-brand hover:underline" to={slurmClusterNodesPath(cluster.uuid)}>{slurmCountsText(t, cluster)}</Link>
      : slurmCountsText(t, cluster)],
    [t("fact.serverUrl"), <span className="break-all">{cluster.server_url}</span>],
    [t("fact.runAs"), <code>{cluster.run_as}</code>],
    [t("fact.accountUser"), cluster.account_user ? <code>{cluster.account_user}</code> : t("fact.ownAccount")],
    [t("fact.installDir"), <code>{cluster.install_dir}</code>],
    [t("fact.scriptDir"), <code>{cluster.script_dir}</code>],
    [t("fact.sbatchArgs"), cluster.sbatch_args ? <code>{cluster.sbatch_args}</code> : "—"],
    [t("fact.idleReturn"), slurmDurationText(t, cluster.idle_return_seconds)],
    [t("fact.pendingTimeout"), slurmDurationText(t, cluster.pending_timeout_seconds)],
    [t("fact.createdAt"), formatTime(cluster.created_at)],
  );
  // The platform sets the entry up itself (the server says so): the button leads and the commands are the way by hand.
  const byPlatform = cluster.can_apply && !cluster.revoked;
  const why = slurmWaitingText(t, cluster);
  return (
    <div className="flex flex-col gap-3 [container-type:inline-size]">
      <FactList facts={facts} />
      <SlurmMissingNodes cluster={cluster} admin={admin} onChanged={onChanged} />
      {admin && cluster.shared_storage_hint && (
        <div className="flex max-w-[880px] flex-wrap items-center gap-x-3 gap-y-1.5">
          <span>{t("sharedStorage.line", { path: cluster.shared_storage_hint.path })}</span>
          <Button size="sm" variant="secondary" onClick={() => navigate("/storage", { state: storageAddState(cluster.shared_storage_hint!) })}>{t("sharedStorage.register")}</Button>
        </div>
      )}
      {cluster.node_count === 0 && (
        <div className="flex max-w-[880px] flex-col gap-1.5">
          <p className="m-0 text-label text-fg-muted">{why || t(cluster.revoked ? "detail.revoked" : byPlatform ? "detail.waitingApply" : "detail.waiting")}</p>
          {byPlatform && <SlurmApplyLead cluster={cluster} onChanged={onChanged} />}
          {!byPlatform && cluster.setup_command && !cluster.revoked && (cluster.source
            // A cloud cluster's entry is set up from its group; the commands are long, so they fold away.
            ? <SlurmFoldedCommand cluster={cluster} />
            : <SlurmSetupCommand command={cluster.setup_command} checkCommand={cluster.check_command} />)}
        </div>
      )}
    </div>
  );
}

/** The setup commands of a cloud cluster's entry, folded under one line until opened. */
function SlurmFoldedCommand({ cluster }: { cluster: SlurmCluster }) {
  const { t } = useTranslation("slurm");
  const [open, setOpen] = useState(false);
  return (
    <Accordion type="single" collapsible value={open ? "commands" : ""} onValueChange={(next: string) => setOpen(next === "commands")}>
      <AccordionItem value="commands">
        <AccordionTrigger>{t("detail.showCommands")}</AccordionTrigger>
        <AccordionContent>{open && <SlurmSetupCommand command={cluster.setup_command!} checkCommand={cluster.check_command} />}</AccordionContent>
      </AccordionItem>
    </Accordion>
  );
}
