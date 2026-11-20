import { useTranslation } from "react-i18next";
import type { SlurmCluster } from "../../slurmApi";
import { SideDrawer } from "../../ui-kit/SideDrawer";
import { useSlurmClusterForm, type SlurmClusterPrefill } from "./useSlurmClusterForm";

export type { SlurmClusterPrefill };

/**
 * The Slurm cluster form in a drawer: connecting an E-HPC cluster of a cloud
 * account (prefilled from it), or editing an entry. A cluster of its own is
 * connected through the Add External Platform dialog.
 */
export default function SlurmClusterDrawer({ open, onClose, onCreated, onEdited, prefill, edit }: {
  open: boolean;
  onClose: () => void;
  onCreated: () => void;
  /** Called when an edit was saved. */
  onEdited?: () => void;
  prefill?: SlurmClusterPrefill;
  /** The entry to edit; absent connects a new one. */
  edit?: SlurmCluster;
}) {
  const { t } = useTranslation("slurm");
  const form = useSlurmClusterForm({ open, onCreated, onEdited, onClose, prefill, edit });
  return (
    <SideDrawer open={open} onClose={onClose} title={edit ? t("form.editTitle") : t("form.title")} width={560} busy={form.saving} footer={form.footer(onClose)}>
      {form.body}
    </SideDrawer>
  );
}
