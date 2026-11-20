import type { ReactNode } from "react";
import { useTranslation } from "react-i18next";
import type { SlurmApplyOutcome, SlurmApplyStepStatus } from "../../slurmApi";
import { Badge, type BadgeStatus } from "../../components/ui/badge";
import { Notice } from "../../ui-kit/Notice";
import SlurmReachView from "./SlurmReachView";

const STATUS: Record<SlurmApplyStepStatus, BadgeStatus> = { pass: "success", warn: "warning", skip: "neutral", fail: "danger" };

/** Node names under a heading, or nothing when there are none. */
function Names({ title, names }: { title: string; names: string[] }) {
  if (names.length === 0) return null;
  return (
    <div className="flex flex-col gap-0.5">
      <span className="text-fg-muted">{title}</span>
      <span className="break-words"><code>{names.join(", ")}</code></span>
    </div>
  );
}

/**
 * What an apply did on the cluster's controller, as the server reports it:
 * each step with how it ended, which nodes got a gres.conf line, how the check
 * script reached the nodes, which nodes were resumed, whether the server's
 * address can be reached from each host, and, when it finished only in part,
 * what is left to do by hand. Nothing here decides anything.
 */
export default function SlurmApplyOutcomeView({ outcome }: { outcome: SlurmApplyOutcome }) {
  const { t } = useTranslation("slurm");
  const { gres, distribute } = outcome;
  const todo: ReactNode[] = [];
  if (distribute.failed.length > 0) todo.push(<li key="distribute">{t("apply.todo.distribute", { nodes: distribute.failed.join(", ") })}</li>);
  if (gres.no_gpu_gres.length > 0) todo.push(<li key="nogres">{t("apply.todo.noGpuGres", { nodes: gres.no_gpu_gres.join(", ") })}</li>);
  if (outcome.resume_failed_nodes.length > 0) todo.push(<li key="resume">{t("apply.todo.resume", { nodes: outcome.resume_failed_nodes.join(", ") })}</li>);
  const unreachable = outcome.reach.hosts.filter((host) => host.status === "failed");
  if (unreachable.length > 0) todo.push(<li key="reach">{t("apply.todo.reach", { url: outcome.cluster.server_url, nodes: unreachable.map((host) => host.name).join(", ") })}</li>);
  for (const step of outcome.steps) {
    if ((step.status === "warn" || step.status === "fail") && !["distribute", "gres", "resume", "reach"].includes(step.id)) todo.push(<li key={step.id}>{t(`apply.todo.${step.id}`)}</li>);
  }
  return (
    <div className="flex min-w-0 flex-col gap-3">
      <Notice tone={outcome.status === "ok" ? "info" : "warning"} title={t(outcome.status === "ok" ? "apply.done" : "apply.partial")}>
        {outcome.status === "ok" ? t("apply.doneHelp") : undefined}
      </Notice>
      {outcome.status === "partial" && todo.length > 0 && (
        <div className="flex flex-col gap-1">
          <strong className="font-semibold text-fg-default">{t("apply.todoTitle")}</strong>
          <ul className="m-0 list-disc ps-5">{todo}</ul>
        </div>
      )}
      <ul aria-label={t("apply.steps")} className="m-0 flex list-none flex-col gap-1 p-0">
        {outcome.steps.map((step) => (
          <li key={step.id} className="flex items-center gap-2">
            <Badge status={STATUS[step.status] ?? "neutral"}>{t(`apply.status.${step.status}`, { defaultValue: step.status })}</Badge>
            <span>{t(`apply.step.${step.id}`, { defaultValue: step.id })}</span>
          </li>
        ))}
      </ul>
      <Names title={t("apply.gresAdded")} names={gres.added} />
      <Names title={t("apply.gresPresent")} names={gres.present} />
      <Names title={t("apply.gresNoGpu")} names={gres.no_gpu_gres} />
      <span className="text-fg-muted">
        {distribute.shared ? t("apply.distributeShared") : t("apply.distributeCopied", { count: distribute.ok })}
      </span>
      <Names title={t("apply.distributeFailed")} names={distribute.failed} />
      <Names title={t("apply.resumed")} names={outcome.resumed_nodes} />
      <Names title={t("apply.resumeFailed")} names={outcome.resume_failed_nodes} />
      <SlurmReachView reach={outcome.reach} url={outcome.cluster.server_url} />
    </div>
  );
}
