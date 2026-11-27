import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";
import { Button } from "../../components/ui/button";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "../../components/ui/dialog";
import { Notice } from "../../ui-kit/Notice";

/** A deployment that keeps a model from being deleted (MODEL_IN_USE params). */
export interface ModelReference { id: number; name: string; state: string; instances: number }

/** The deployments a refused deletion names, and whether they can all go with the model. */
export function modelReferences(params: Record<string, unknown> | undefined): { deployments: ModelReference[]; deletable: boolean } {
  const raw = Array.isArray(params?.deployments) ? params.deployments : [];
  const deployments = raw.filter((d): d is ModelReference => !!d && typeof d === "object" && typeof (d as ModelReference).id === "number");
  return { deployments, deletable: params?.deletable_with_deployments === true };
}

/**
 * Says which deployments keep a model, each linked to its page. When the
 * server says every one is stopped and runs nothing, it offers to delete them
 * together with the model; the server decides that, not this dialog (GWKS-530).
 */
export default function ModelInUseDialog({ name, deployments, deletable, onDeleteWith, onClose }: {
  name: string;
  deployments: ModelReference[];
  deletable: boolean;
  onDeleteWith: () => Promise<void>;
  onClose: () => void;
}) {
  const { t } = useTranslation("models");
  const { t: tCommon } = useTranslation("common");
  const [busy, setBusy] = useState(false);
  const remove = async () => {
    setBusy(true);
    try { await onDeleteWith(); } finally { setBusy(false); }
  };
  return (
    <Dialog open onOpenChange={(open) => { if (!open && !busy) onClose(); }}>
      <DialogContent closeButtonLabel={tCommon("actions.close")} closeButtonDisabled={busy} style={{ width: "min(640px, calc(100vw - 32px))" }} className="flex flex-col gap-4">
        <DialogHeader><DialogTitle>{t("inUse.title", { name })}</DialogTitle></DialogHeader>
        <Notice tone={deletable ? "info" : "warning"} title={t(deletable ? "inUse.idle" : "inUse.running", { count: deployments.length })} />
        <ul className="m-0 flex list-none flex-col gap-1 p-0">
          {deployments.map((d) => (
            <li key={d.id} className="flex flex-wrap items-center gap-2">
              <Link to={`/deployments?deployment=${d.id}`}>{d.name || `#${d.id}`}</Link>
              <span className="text-fg-muted">{t("inUse.row", { state: t(`inUse.state.${d.state}`, { defaultValue: d.state }), count: d.instances })}</span>
            </li>
          ))}
        </ul>
        <DialogFooter>
          <Button variant="secondary" disabled={busy} onClick={onClose}>{tCommon("actions.close")}</Button>
          {deletable && <Button variant="destructive" loading={busy} onClick={() => void remove()}>{t("inUse.deleteWith", { count: deployments.length })}</Button>}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
