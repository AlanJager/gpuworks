import { useTranslation } from "react-i18next";
import type { Node } from "../api";
import { Notice } from "../ui-kit/Notice";
import CopyButton from "./CopyButton";

type SlurmNode = Pick<Node, "slurm_node" | "slurm_unavailable" | "slurm_reason" | "slurm_fixes">;

/**
 * Why the platform holds a Slurm node out of scheduling: Slurm will not start
 * the placeholder jobs that borrow the node's cards, so every deployment would
 * wait out the pending timeout. Shows what Slurm says (the code, then its own
 * Reason as written) and the steps that bring the node back, as the server
 * sent them (slurm_fixes): the text for each step's code, each command
 * copyable. Nothing is shown while Slurm runs jobs on the node.
 */
export default function SlurmNodeNotice({ node }: { node: SlurmNode }) {
  const { t } = useTranslation("nodes");
  const code = node.slurm_unavailable ?? "";
  if (!code) return null;
  const name = node.slurm_node ?? "";
  const title = t(`slurm.unavailable.${code}`, { node: name, defaultValue: t("slurm.unavailable.other", { code }) });
  const fixes = node.slurm_fixes ?? [];
  return (
    <Notice tone="warning" role="alert" title={title}>
      <div className="slurm-notice">
        {node.slurm_reason && <p>{t("slurm.says", { reason: node.slurm_reason })}</p>}
        {fixes.length > 0 && <p>{t("slurm.howToFix")}</p>}
        {fixes.length > 0 && (
          <ol>
            {fixes.map(fix => (
              <li key={fix.code}>
                {t(`slurm.step.${fix.code}`, { node: name, defaultValue: "" })}
                {fix.commands.map(command => (
                  <div key={command} style={{ display: "flex", alignItems: "center", gap: 4 }}>
                    <code style={{ whiteSpace: "pre-wrap", wordBreak: "break-all" }}>{command}</code>
                    <CopyButton text={command} label={t("slurm.copy")} success={t("slurm.copied")} manualTitle={t("slurm.manualCopyTitle")} size="small" />
                  </div>
                ))}
              </li>
            ))}
          </ol>
        )}
      </div>
    </Notice>
  );
}
