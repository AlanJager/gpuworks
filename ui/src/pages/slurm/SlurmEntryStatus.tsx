import { useTranslation } from "react-i18next";
import CopyButton from "../../components/CopyButton";
import { Badge } from "../../components/ui/badge";
import { Button } from "../../components/ui/button";
import { Popover, PopoverContent, PopoverTitle, PopoverTrigger } from "../../components/ui/popover";
import { Tooltip } from "../../components/ui/tooltip";
import type { SlurmCluster } from "../../slurmApi";
import { slurmTroubleshootCommands } from "../../slurmClusters";
import { slurmWaitingText } from "./slurmReach";

/**
 * Why a cluster's nodes have not joined: the server's reason when it has one,
 * else the general waiting line, then the commands that tell which of the
 * usual causes it is. An entry the platform sets up itself leads with Check
 * and apply and has no controller command.
 */
function StalledHelp({ cluster }: { cluster: SlurmCluster }) {
  const { t } = useTranslation("slurm");
  const commands = slurmTroubleshootCommands(cluster);
  // An entry the platform sets up itself (the server says so) is not troubleshot on the controller: its step is Check and apply.
  const byPlatform = cluster.can_apply && !cluster.revoked;
  const blocks = byPlatform ? ([["node", commands.node]] as const) : ([["controller", commands.controller], ["node", commands.node]] as const);
  return (
    <div className="flex max-w-[520px] flex-col gap-2">
      <p>{slurmWaitingText(t, cluster) || t(byPlatform ? "stalled.introApplied" : "stalled.intro")}</p>
      {blocks.map(([where, command]) => (
        <div key={where} className="flex flex-col gap-1">
          <span className="text-fg-muted">{t(`stalled.on.${where}`)}</span>
          <div className="flex items-start gap-1">
            <pre aria-label={t(`stalled.on.${where}`)} className="code-block m-0 min-w-0 flex-1 overflow-x-auto">{command}</pre>
            <CopyButton size="small" label={t("stalled.copy", { where: t(`stalled.on.${where}`) })} text={command} />
          </div>
        </div>
      ))}
      <strong className="font-semibold text-fg-default">{t("stalled.causes")}</strong>
      <ul className="m-0 list-disc ps-5">
        {([byPlatform ? "notRunApplied" : "notRun", "script", "connect", "conf"] as const).map((key) => <li key={key}>{t(`stalled.${key}`)}</li>)}
      </ul>
    </div>
  );
}

/**
 * Where a Slurm entry stands, as the server says: revoked, waiting for its
 * first node (with how to find out why once it is stalled), or joined.
 */
export default function SlurmEntryStatus({ cluster }: { cluster: SlurmCluster }) {
  const { t } = useTranslation("slurm");
  if (cluster.state === "revoked") return <Badge status="danger">{t("status.revoked")}</Badge>;
  if (cluster.state === "stalled") {
    return (
      // Wraps under the badge where the column is narrow.
      <span className="flex flex-wrap items-center gap-x-1 whitespace-normal">
        <Badge status="warning">{t("status.stalled")}</Badge>
        <Popover>
          <PopoverTrigger render={<Button variant="link" size="sm">{t("status.troubleshoot")}</Button>} />
          <PopoverContent align="end" className="w-[560px] max-w-[calc(100vw-32px)]">
            <PopoverTitle className="mb-2">{t("stalled.title")}</PopoverTitle>
            <StalledHelp cluster={cluster} />
          </PopoverContent>
        </Popover>
      </span>
    );
  }
  if (cluster.state === "pending") return <Tooltip content={t("status.pendingHint")}><Badge status="neutral">{t("status.pending")}</Badge></Tooltip>;
  return <Badge status="success">{t("status.active")}</Badge>;
}
