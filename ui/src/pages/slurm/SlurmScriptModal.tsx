import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { api } from "../../api";
import { apiErrorText } from "../../apiError";
import type { SlurmCluster } from "../../slurmApi";
import CopyButton from "../../components/CopyButton";
import { Button } from "../../components/ui/button";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "../../components/ui/dialog";
import { Notice } from "../../ui-kit/Notice";
import { Spinner } from "../../ui-kit/Spinner";

/**
 * The setup script the entry's controller is served, to read before running
 * it, the way a Kubernetes entry's manifest is shown. The server leaves the
 * enrollment token out of this text, so it is safe to read and copy; the
 * commands that carry the token are the ones to run. Mount it while it is open.
 */
export default function SlurmScriptModal({ cluster, onClose }: { cluster: SlurmCluster; onClose: () => void }) {
  const { t } = useTranslation("slurm");
  const { t: tCommon } = useTranslation("common");
  const [script, setScript] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    let alive = true;
    api.slurmClusterScript(cluster.uuid)
      .then((text) => { if (alive) setScript(text); })
      .catch((e) => { if (alive) setError(apiErrorText(tCommon, e)); });
    return () => { alive = false; };
  }, [cluster.uuid, tCommon]);
  return (
    <Dialog open onOpenChange={(open) => { if (!open) onClose(); }}>
      <DialogContent closeButtonLabel={tCommon("actions.close")} style={{ width: "min(760px, calc(100vw - 32px))", maxWidth: "calc(100vw - 32px)" }} className="flex max-h-[calc(100dvh-32px)] flex-col gap-4">
        <DialogHeader><DialogTitle>{t("script.title", { name: cluster.name })}</DialogTitle></DialogHeader>
        <div className="flex min-h-0 min-w-0 flex-col gap-3 overflow-auto">
          {error !== null && <Notice tone="danger" title={t("script.failed")}>{error}</Notice>}
          {error === null && script === null && <Spinner label={tCommon("dataTable.refresh")} />}
          {script !== null && <Notice tone="info" title={t("script.tokenHidden")} />}
          {script !== null && <pre aria-label={t("script.title", { name: cluster.name })} tabIndex={0} className="code-block max-h-[60vh] overflow-auto p-4">{script}</pre>}
        </div>
        <DialogFooter>
          {script !== null && <CopyButton type="default" label={t("script.copy")} text={script}>{t("script.copy")}</CopyButton>}
          <Button variant="primary" onClick={onClose}>{t("script.close")}</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
