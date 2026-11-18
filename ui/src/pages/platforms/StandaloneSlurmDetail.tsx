import { useTranslation } from "react-i18next";
import type { SlurmCluster } from "../../slurmApi";
import SlurmEntryDetail from "../slurm/SlurmEntryDetail";

/**
 * A Slurm cluster connected on its own (not from a cloud account), under its
 * row: how it was connected, what it is and, until a node has joined, the
 * command that connects it; `admin` and `onChanged` as SlurmEntryDetail takes
 * them.
 */
export default function StandaloneSlurmDetail({ cluster, admin = false, onChanged }: { cluster: SlurmCluster; admin?: boolean; onChanged?: () => void }) {
  const { t } = useTranslation("slurm");
  return (
    // The detail steps in under its row, behind a rule, so it reads as the row's (ui-guidelines 4.1).
    <div className="ms-6 border-s-2 border-border py-1 ps-3">
      <SlurmEntryDetail cluster={cluster} connectedBy={t("fact.byCommand")} admin={admin} onChanged={onChanged} />
    </div>
  );
}
