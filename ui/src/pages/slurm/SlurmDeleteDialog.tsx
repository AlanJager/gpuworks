import { useId, useState } from "react";
import { useTranslation } from "react-i18next";
import { api } from "../../api";
import { apiErrorText } from "../../apiError";
import type { SlurmCluster } from "../../slurmApi";
import { Button } from "../../components/ui/button";
import { Checkbox } from "../../components/ui/checkbox";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "../../components/ui/dialog";
import { message } from "../../ui-kit/feedback";
import { Notice } from "../../ui-kit/Notice";

/**
 * Deleting an entry. One that has nodes can only go together with them: the
 * option is the server's (`delete_with_nodes_allowed`), offered when it would
 * succeed and otherwise shown disabled with the reason the server gives
 * (`delete_with_nodes_blocker`). The way to bring the agents down first is the
 * uninstall dialog. Mount it while it is open.
 */
export default function SlurmDeleteDialog({ cluster, onClose, onDeleted, onUninstall }: {
  cluster: SlurmCluster;
  onClose: () => void;
  onDeleted: () => void;
  /** Opens the uninstall dialog instead. */
  onUninstall?: () => void;
}) {
  const { t } = useTranslation("slurm");
  const { t: tCommon } = useTranslation("common");
  const optionId = useId();
  const [withNodes, setWithNodes] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const hasNodes = cluster.node_count > 0;

  const remove = async () => {
    setDeleting(true);
    setError(null);
    try {
      await api.deleteSlurmCluster(cluster.uuid, hasNodes && withNodes);
      message.success(t("delete.deleted", { name: cluster.name }));
      onDeleted();
      onClose();
    } catch (e) {
      setError(apiErrorText(tCommon, e));
      setDeleting(false);
    }
  };

  return (
    <Dialog open onOpenChange={(open) => { if (!open && !deleting) onClose(); }}>
      <DialogContent closeButtonLabel={tCommon("actions.close")} closeButtonDisabled={deleting} className="flex flex-col gap-4">
        <DialogHeader><DialogTitle>{t("delete.title", { name: cluster.name })}</DialogTitle></DialogHeader>
        <div className="flex min-w-0 flex-col items-start gap-2">
          <p className="m-0 text-fg-muted">{t("delete.note")}</p>
          {hasNodes && (
            <>
              <p className="m-0 text-fg-danger">{t("delete.hasNodes", { count: cluster.node_count })}</p>
              <div className="flex items-center gap-2">
                <Checkbox id={optionId} checked={withNodes} disabled={!cluster.delete_with_nodes_allowed || deleting} onCheckedChange={(checked) => setWithNodes(checked === true)} />
                <label htmlFor={optionId} className={cluster.delete_with_nodes_allowed ? "cursor-pointer" : "text-fg-muted"}>{t("delete.withNodes", { count: cluster.node_count })}</label>
              </div>
              {!cluster.delete_with_nodes_allowed && (
                <p className="m-0 text-fg-muted">{t(`delete.blocker.${cluster.delete_with_nodes_blocker || "unknown"}`, { defaultValue: t("delete.blocker.unknown") })}</p>
              )}
              {onUninstall && <button type="button" className="text-fg-brand hover:underline" onClick={onUninstall}>{t("delete.uninstall")}</button>}
            </>
          )}
          {error && <Notice tone="danger" title={error} role="alert" />}
        </div>
        <DialogFooter>
          <Button variant="ghost" disabled={deleting} onClick={onClose}>{t("delete.cancel")}</Button>
          <Button variant="destructive" loading={deleting} disabled={hasNodes && !withNodes} onClick={() => void remove()}>{t("delete.confirm")}</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
