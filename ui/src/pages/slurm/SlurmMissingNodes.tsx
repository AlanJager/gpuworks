import { useTranslation } from "react-i18next";
import { api } from "../../api";
import { Button } from "../../components/ui/button";
import type { SlurmCluster } from "../../slurmApi";
import { ConfirmPopover } from "../../ui-kit/ConfirmPopover";

/**
 * The GPU nodes Slurm lists that have no agent online, with why (the server's
 * code, and Slurm's own words for the node) and what usually causes it. The
 * server lists the first of them; the count is of all. A joined node that is
 * offline carries its node's UUID, and an administrator can remove it: for a
 * node that left the cluster while no agent was left to say so.
 */
export default function SlurmMissingNodes({ cluster, admin = false, onChanged }: { cluster: SlurmCluster; admin?: boolean; onChanged?: () => void }) {
  const { t } = useTranslation("slurm");
  if (cluster.missing_count === 0) return null;
  return (
    <div className="flex max-w-[880px] flex-col gap-1.5">
      <strong className="font-semibold text-fg-default">{t("missing.title", { count: cluster.missing_count })}</strong>
      <ul aria-label={t("missing.list")} className="m-0 flex list-none flex-col gap-1 p-0">
        {cluster.missing_nodes.map((node) => (
          <li key={node.name} className="flex flex-wrap items-baseline gap-x-2">
            <code>{node.name}</code>
            <span>{t(`missing.reason.${node.reason}`, { defaultValue: node.reason })}</span>
            {node.slurm_state && <span className="text-fg-muted">{t("missing.slurmSays", { state: node.slurm_state })}{node.slurm_reason ? ` · ${node.slurm_reason}` : ""}</span>}
            {admin && node.node_uuid && (
              <ConfirmPopover
                title={t("missing.removeTitle", { name: node.name })}
                confirmText={t("missing.remove")}
                destructive
                onConfirm={async () => { await api.deleteNode(node.node_uuid!); onChanged?.(); }}
                trigger={<Button variant="link" size="sm">{t("missing.remove")}</Button>}
              >
                <p className="m-0">{t("missing.removeHelp")}</p>
              </ConfirmPopover>
            )}
          </li>
        ))}
      </ul>
      {cluster.missing_count > cluster.missing_nodes.length && <span className="text-fg-muted">{t("missing.truncated", { total: cluster.missing_count, shown: cluster.missing_nodes.length })}</span>}
      <span className="text-fg-muted">{t("missing.causesTitle")}</span>
      <ul className="m-0 list-disc ps-5">
        {(["script", "connect", "drained"] as const).map((key) => <li key={key}>{t(`missing.cause.${key}`)}</li>)}
      </ul>
    </div>
  );
}
