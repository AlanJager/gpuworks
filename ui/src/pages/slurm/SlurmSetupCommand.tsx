import { useTranslation } from "react-i18next";
import { slurmDurationText, useSlurmDefaults } from "../../slurmClusters";
import SlurmCommandBlock from "./SlurmCommandBlock";

const STEPS = ["script", "distribute", "conf", "reconfigure"] as const;

/**
 * The command that connects the cluster, and what it does: it runs once as
 * root on the Slurm controller, and every GPU node then joins by itself within
 * the health-check interval (the server's, GET /slurm/defaults). Under it, when
 * the server gives one, the same command in check-only mode, which says what
 * it would do and changes nothing.
 */
export default function SlurmSetupCommand({ command, checkCommand }: { command: string; checkCommand?: string }) {
  const { t } = useTranslation("slurm");
  const { defaults } = useSlurmDefaults(true);
  return (
    <div className="flex min-w-0 flex-col gap-3">
      {checkCommand && <SlurmCommandBlock command={checkCommand} title={t("setup.checkCommand")} hint={t("setup.checkHint")} />}
      <SlurmCommandBlock command={command} title={t("setup.command")} hint={t("setup.runAsRoot")} />
      <div className="flex flex-col gap-1">
        <span className="text-fg-muted">{t("setup.does")}</span>
        <ol className="m-0 list-decimal ps-5">
          {STEPS.map((step) => <li key={step}>{t(`setup.step.${step}`)}</li>)}
        </ol>
      </div>
      <span className="text-fg-muted">
        {defaults ? t("setup.joins", { interval: slurmDurationText(t, defaults.health_check_interval_seconds) }) : t("setup.joinsUnknown")}
      </span>
      <span className="text-fg-muted">{t("setup.secret")}</span>
    </div>
  );
}
