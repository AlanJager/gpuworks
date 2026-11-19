import { useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { api } from "../../api";
import { apiErrorText } from "../../apiError";
import type { SlurmCluster, SlurmRemoveStep } from "../../slurmApi";
import { slurmDurationText, useSlurmDefaults } from "../../slurmClusters";
import { Button } from "../../components/ui/button";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "../../components/ui/dialog";
import { Notice } from "../../ui-kit/Notice";
import SlurmCommandBlock from "./SlurmCommandBlock";

/**
 * Takes an entry's agents and its health check out of the cluster, in the two
 * steps the server defines: the agents first (every node's check removes its
 * own on its next run), then, after the wait the server names, the health
 * check in slurm.conf. For an entry the platform can reach, it runs the steps
 * on the controller itself (the wait is the server's number, counted down
 * here); for any entry the two commands are shown to run by hand. The entry is
 * deleted afterwards. Mount it while it is open.
 */
export default function SlurmUninstallModal({ cluster, onClose, onChanged, onDelete }: {
  cluster: SlurmCluster;
  onClose: () => void;
  onChanged: () => void;
  /** Opens the delete confirmation: offered once the platform has removed the health check. */
  onDelete?: () => void;
}) {
  const { t } = useTranslation("slurm");
  const { t: tCommon } = useTranslation("common");
  const { defaults } = useSlurmDefaults(true);
  const [step, setStep] = useState<SlurmRemoveStep>("uninstall_agents");
  const [running, setRunning] = useState(false);
  const [waitLeft, setWaitLeft] = useState(0);
  const [uninstalled, setUninstalled] = useState(false);
  const [done, setDone] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const alive = useRef(true);
  useEffect(() => { alive.current = true; return () => { alive.current = false; }; }, []);
  // The server's wait, counted down a second at a time.
  useEffect(() => {
    if (waitLeft <= 0) return;
    const timer = setTimeout(() => setWaitLeft((left) => left - 1), 1000);
    return () => clearTimeout(timer);
  }, [waitLeft]);

  const run = async () => {
    setRunning(true);
    setError(null);
    try {
      const outcome = await api.removeSlurmCluster(cluster.uuid, step);
      if (!alive.current) return;
      onChanged();
      if (outcome.next_step) {
        setStep(outcome.next_step);
        setUninstalled(true);
        setWaitLeft(outcome.wait_seconds);
      } else setDone(true);
    } catch (e) {
      if (alive.current) setError(apiErrorText(tCommon, e));
    } finally {
      if (alive.current) setRunning(false);
    }
  };

  const interval = defaults ? slurmDurationText(t, defaults.health_check_interval_seconds) : "";
  const commands = (
    <div className="flex flex-col gap-3">
      {cluster.uninstall_agents_command && (
        <SlurmCommandBlock command={cluster.uninstall_agents_command} title={t("uninstall.step1")}
          hint={interval ? t("uninstall.step1Hint", { interval }) : t("uninstall.step1HintUnknown")} />
      )}
      {cluster.remove_command && <SlurmCommandBlock command={cluster.remove_command} title={t("uninstall.step2")} hint={t("uninstall.step2Hint")} />}
    </div>
  );

  return (
    <Dialog open onOpenChange={(open) => { if (!open && !running) onClose(); }}>
      <DialogContent closeButtonLabel={tCommon("actions.close")} closeButtonDisabled={running} style={{ width: "min(760px, calc(100vw - 32px))", maxWidth: "calc(100vw - 32px)" }} className="flex max-h-[calc(100dvh-32px)] flex-col gap-4">
        <DialogHeader><DialogTitle>{t("uninstall.title", { name: cluster.name })}</DialogTitle></DialogHeader>
        <div className="flex min-h-0 min-w-0 flex-col gap-3 overflow-auto">
          <p>{t("uninstall.intro")}</p>
          {cluster.can_remove && (
            <div className="flex flex-col gap-2">
              <strong className="font-semibold text-fg-default">{t("uninstall.byGpuworks")}</strong>
              <span className="text-fg-muted">{t("uninstall.byGpuworksHelp")}</span>
              {error && <Notice tone="danger" title={error} role="alert" />}
              {uninstalled && !done && <Notice tone="info" title={t("uninstall.agentsSent")} role="status" />}
              {uninstalled && !done && waitLeft > 0 && <p role="status" className="m-0">{t("uninstall.waiting", { seconds: waitLeft })}</p>}
              {done && <Notice tone="info" title={t("uninstall.removed")} role="status">{t("uninstall.removedHelp")}</Notice>}
              <div className="flex flex-wrap gap-2">
                {!done && (
                  <Button variant="primary" loading={running} disabled={waitLeft > 0} onClick={() => void run()}>{t(`uninstall.run.${step}`)}</Button>
                )}
                {done && onDelete && <Button variant="destructive" onClick={onDelete}>{t("uninstall.delete")}</Button>}
              </div>
              <strong className="mt-2 font-semibold text-fg-default">{t("uninstall.byHand")}</strong>
            </div>
          )}
          {commands}
          <p className="text-fg-muted">{t("uninstall.next")}</p>
        </div>
        <DialogFooter>
          <Button variant={cluster.can_remove ? "secondary" : "primary"} disabled={running} onClick={onClose}>{t("uninstall.close")}</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
