import { useTranslation } from "react-i18next";
import type { Node } from "../api";
import { Notice } from "../ui-kit/Notice";
import CopyButton from "./CopyButton";

type SlurmNode = Pick<Node, "slurm_node" | "slurm_unavailable" | "slurm_reason" | "slurm_gres_conf">;

/** One step of the fix: its text and the commands to copy (the gres.conf line is a command-shaped line too). */
interface Step {
  key: string;
  commands: string[];
}

/**
 * The fix for each slurm_unavailable code (what the server computed; the
 * gres.conf line itself comes from the server as slurm_gres_conf). The codes
 * are the server's; this only says what to do for each in the operator's
 * language, with the commands to run.
 */
function steps(node: SlurmNode): Step[] {
  const name = node.slurm_node ?? "";
  const resume = { key: "resume", commands: [`scontrol update nodename=${name} state=resume`] };
  switch (node.slurm_unavailable) {
    case "gres_missing":
      return [
        ...(node.slurm_gres_conf ? [{ key: "gresConf", commands: [node.slurm_gres_conf] }] : []),
        { key: "reconfigure", commands: ["scontrol reconfigure"] },
        resume,
        { key: "restartSlurmd", commands: ["sudo systemctl restart slurmd"] },
      ];
    case "drained":
      return [{ ...resume, key: "resumeDrained" }];
    case "down":
    case "invalid_registration":
      return [{ key: "checkSlurmd", commands: ["systemctl status slurmd", "sudo journalctl -u slurmd -n 100 --no-pager"] }, resume];
    default:
      return [];
  }
}

const knownCodes = ["gres_missing", "invalid_registration", "down", "drained"];

/**
 * Why the platform holds a Slurm node out of scheduling: Slurm will not start
 * the placeholder jobs that borrow the node's cards, so every deployment would
 * wait out the pending timeout. Shows what Slurm says (the code, then its own
 * Reason as written) and the steps that bring the node back, each command
 * copyable. Nothing is shown while Slurm runs jobs on the node.
 */
export default function SlurmNodeNotice({ node }: { node: SlurmNode }) {
  const { t } = useTranslation("nodes");
  const code = node.slurm_unavailable ?? "";
  if (!code) return null;
  const title = knownCodes.includes(code) ? t(`slurm.unavailable.${code}`, { node: node.slurm_node ?? "" }) : t("slurm.unavailable.other", { code });
  return (
    <Notice tone="warning" role="alert" title={title}>
      <div className="slurm-notice">
        {node.slurm_reason && <p>{t("slurm.says", { reason: node.slurm_reason })}</p>}
        <p>{t("slurm.howToFix")}</p>
        <ol>
          {steps(node).map(step => (
            <li key={step.key}>
              {t(`slurm.step.${step.key}`, { node: node.slurm_node ?? "" })}
              {step.commands.map(command => (
                <div key={command} style={{ display: "flex", alignItems: "center", gap: 4 }}>
                  <code style={{ whiteSpace: "pre-wrap", wordBreak: "break-all" }}>{command}</code>
                  <CopyButton text={command} label={t("slurm.copy")} success={t("slurm.copied")} manualTitle={t("slurm.manualCopyTitle")} size="small" />
                </div>
              ))}
            </li>
          ))}
        </ol>
      </div>
    </Notice>
  );
}
