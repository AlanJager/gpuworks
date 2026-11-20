import { useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { api } from "../../api";
import { APIError, apiErrorText } from "../../apiError";
import type { SlurmApplyCheck, SlurmApplyOutcome, SlurmCluster } from "../../slurmApi";
import { Button } from "../../components/ui/button";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "../../components/ui/dialog";
import { Notice } from "../../ui-kit/Notice";
import SlurmApplyOutcomeView from "./SlurmApplyOutcomeView";
import SlurmCheckPlanView from "./SlurmCheckPlanView";

type Stage = "checking" | "checked" | "running" | "done";
type Failure = { text: string; output: string };

const failureOf = (tCommon: Parameters<typeof apiErrorText>[0], e: unknown): Failure =>
  ({ text: apiErrorText(tCommon, e), output: e instanceof APIError ? String(e.params?.output ?? "") : "" });

/**
 * Has the platform set a Slurm entry up on its controller, in the two steps
 * Kubernetes entries take: it first has the manager node check what the setup
 * would do, which changes nothing, and shows that plan; only when the operator
 * confirms does it run the setup (POST /slurm/clusters/{uuid}/apply with
 * dry_run true, then false). A refusal or failure of either is a coded error,
 * shown in the operator's language with what the controller printed. Mount it
 * while it is open.
 */
export default function SlurmApplyModal({ cluster, onClose, onDone }: {
  cluster: SlurmCluster;
  onClose: () => void;
  /** The apply finished (fully or in part): the entry's counts changed. */
  onDone?: () => void;
}) {
  const { t } = useTranslation("slurm");
  const { t: tCommon } = useTranslation("common");
  const [stage, setStage] = useState<Stage>("checking");
  const [plan, setPlan] = useState<SlurmApplyCheck | null>(null);
  const [outcome, setOutcome] = useState<SlurmApplyOutcome | null>(null);
  const [failure, setFailure] = useState<Failure | null>(null);
  const [attempt, setAttempt] = useState(0);
  const alive = useRef(true);
  useEffect(() => { alive.current = true; return () => { alive.current = false; }; }, []);

  useEffect(() => {
    let current = true;
    setStage("checking");
    setPlan(null);
    setFailure(null);
    api.checkSlurmCluster(cluster.uuid)
      .then((result) => { if (current) { setPlan(result); setStage("checked"); } })
      .catch((e) => { if (current) { setFailure(failureOf(tCommon, e)); setStage("checked"); } });
    return () => { current = false; };
  }, [cluster.uuid, attempt, tCommon]);

  const run = async () => {
    setStage("running");
    setFailure(null);
    try {
      const result = await api.applySlurmCluster(cluster.uuid);
      if (!alive.current) return;
      setOutcome(result);
      onDone?.();
    } catch (e) {
      if (alive.current) setFailure(failureOf(tCommon, e));
    } finally {
      if (alive.current) setStage("done");
    }
  };

  const running = stage === "running";
  const checkFailed = stage === "checked" && failure !== null;
  const applyFailed = stage === "done" && failure !== null;
  const footer = stage === "done" || checkFailed ? (
    <>
      {checkFailed && <Button variant="secondary" onClick={() => setAttempt((n) => n + 1)}>{t("operation.checkAgain")}</Button>}
      {applyFailed && <Button variant="secondary" onClick={() => void run()}>{t("apply.retry")}</Button>}
      <Button variant="primary" onClick={onClose}>{t("operation.close")}</Button>
    </>
  ) : (
    <>
      <Button variant="ghost" onClick={onClose} disabled={running}>{t("operation.cancel")}</Button>
      <Button variant="primary" disabled={stage !== "checked"} loading={running} onClick={() => void run()}>{t("operation.confirm")}</Button>
    </>
  );

  return (
    <Dialog open onOpenChange={(open) => { if (!open && !running) onClose(); }} disablePointerDismissal>
      <DialogContent closeButtonLabel={tCommon("actions.close")} closeButtonDisabled={running} style={{ width: "min(760px, calc(100vw - 32px))", maxWidth: "calc(100vw - 32px)" }} className="flex max-h-[calc(100dvh-32px)] flex-col gap-4">
        <DialogHeader><DialogTitle>{t("operation.title", { name: cluster.name })}</DialogTitle></DialogHeader>
        <div className="flex min-h-0 min-w-0 flex-col gap-3 overflow-auto">
          <p>{t("operation.intro", { account: cluster.source?.provider_name || cluster.source?.provider_kind || "", cluster: cluster.source?.provider_cluster_id ?? "" })}</p>
          {stage === "checking" && <Notice tone="info" title={t("operation.checking")} role="status" />}
          {stage === "checked" && plan && <Notice tone="info" title={t(plan.status === "ok" ? "operation.checked" : "operation.checkedPartial")} />}
          {running && <Notice tone="info" title={t("apply.running")} role="status" />}
          {failure && (
            <Notice tone="danger" title={failure.text} role="alert">
              {failure.output ? <pre aria-label={t("apply.output")} tabIndex={0} className="code-block m-0 max-h-48 overflow-auto whitespace-pre-wrap break-all p-3">{failure.output}</pre> : undefined}
            </Notice>
          )}
          {stage === "checked" && plan && <SlurmCheckPlanView plan={plan} />}
          {outcome && stage === "done" && <SlurmApplyOutcomeView outcome={outcome} />}
        </div>
        <DialogFooter>{footer}</DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
