import type { ReactNode } from "react";
import { useTranslation } from "react-i18next";
import type { SlurmApplyCheck, SlurmCheckWarning } from "../../slurmApi";
import { Notice } from "../../ui-kit/Notice";
import SlurmReachView from "./SlurmReachView";

/** Node names or lines under a heading, or nothing when there are none. */
function Items({ title, items }: { title: string; items: string[] }) {
  if (items.length === 0) return null;
  return (
    <div className="flex flex-col gap-0.5">
      <span className="text-fg-muted">{title}</span>
      <ul className="m-0 list-none p-0">
        {items.map((item) => <li key={item}><code className="break-all">{item}</code></li>)}
      </ul>
    </div>
  );
}

function Section({ title, children }: { title: string; children: ReactNode }) {
  return (
    <section className="flex flex-col gap-1.5">
      <strong className="font-semibold text-fg-default">{title}</strong>
      {children}
    </section>
  );
}

/**
 * What applying a Slurm entry would do, from the check the manager node ran
 * (dto.SlurmApplyCheck): the gres.conf lines it would add, how the check
 * script reaches the compute nodes, the block it would put in slurm.conf and
 * the cluster's own health check settings it would comment and chain, the
 * nodes it would resume, whether the server's address can be reached from the
 * controller and each GPU node, and the points that need attention, each a
 * code the server gave. Nothing here decides anything.
 */
export default function SlurmCheckPlanView({ plan }: { plan: SlurmApplyCheck }) {
  const { t } = useTranslation("slurm");
  const { gres, script, conf } = plan;
  const warning = (code: SlurmCheckWarning): string => {
    switch (code) {
      case "conf_include": return t("plan.warning.conf_include", { files: conf.includes.join(", ") });
      case "program_not_in_conf": return t("plan.warning.program_not_in_conf", { program: conf.unnamed_program ?? "" });
      case "not_configless": return t("plan.warning.not_configless", { path: conf.path });
      case "no_gpu_gres": return t("plan.warning.no_gpu_gres", { nodes: gres.no_gpu_gres.join(", ") });
      case "server_unreachable": return t("plan.warning.server_unreachable", { url: plan.cluster.server_url });
    }
  };
  return (
    <div className="flex min-w-0 flex-col gap-3">
      {plan.warnings.length > 0 && (
        <Notice tone="warning" title={t("plan.attention")}>
          <ul className="m-0 list-disc ps-5">{plan.warnings.map((code) => <li key={code}>{warning(code)}</li>)}</ul>
        </Notice>
      )}
      <Section title={t("plan.gres.title")}>
        {gres.would_add.length === 0 ? <span className="text-fg-muted">{t("plan.gres.none")}</span> : <Items title={t("plan.gres.add")} items={gres.would_add.map((l) => l.line)} />}
        <Items title={t("plan.gres.present")} items={gres.present} />
        <Items title={t("plan.gres.noGpu")} items={gres.no_gpu_gres} />
      </Section>
      <Section title={t("plan.script.title")}>
        <span>{t("plan.script.write", { path: script.check_script, token: script.token_file })}</span>
        {script.shared
          ? <span className="text-fg-muted">{t("plan.script.shared", { fstype: script.fstype })}</span>
          : script.copy_to.length > 0
            ? <span>{t("plan.script.copy", { count: script.copy_to.length, nodes: script.copy_to.join(", "), files: script.copy_files.join(", ") })}</span>
            : <span className="text-fg-muted">{t("plan.script.copyNone")}</span>}
      </Section>
      <Section title={t("plan.conf.title", { path: conf.path })}>
        {conf.changed ? (
          <>
            <span className="text-fg-muted">{t("plan.conf.add", { interval: conf.interval })}</span>
            <pre aria-label={t("plan.conf.block")} tabIndex={0} className="code-block m-0 whitespace-pre-wrap break-all p-3">{conf.block.join("\n")}</pre>
          </>
        ) : <span className="text-fg-muted">{t("plan.conf.unchanged")}</span>}
        <Items title={t("plan.conf.commented")} items={conf.commented.map((c) => `${t("plan.conf.line", { line: c.line })}: ${c.key}=${c.value}`)} />
        {conf.chained_program && <span>{t("plan.conf.chained", { program: conf.chained_program, state: conf.chained_state ?? "" })}</span>}
        <span className="text-fg-muted">{t(plan.configless ? "plan.conf.configless" : "plan.conf.notConfigless")}</span>
      </Section>
      <SlurmReachView reach={plan.reach} url={plan.cluster.server_url} />
      {plan.rearm && <span>{t("plan.rearm")}</span>}
      <Items title={t("plan.resume")} items={plan.resume_nodes} />
    </div>
  );
}
