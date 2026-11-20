import { useEffect, useState } from "react";
import type { TFunction } from "i18next";
import { api } from "./api";
import type { SlurmCluster, SlurmDefaults } from "./slurmApi";

/**
 * The Slurm cluster form's defaults and bounds, which the server owns
 * (GET /api/v1/slurm/defaults). null until they arrive; error says why they
 * did not, and the form then sends only what was typed.
 */
export function useSlurmDefaults(enabled: boolean): { defaults: SlurmDefaults | null; error: Error | null } {
  const [state, setState] = useState<{ defaults: SlurmDefaults | null; error: Error | null }>({ defaults: null, error: null });
  useEffect(() => {
    if (!enabled) return;
    let alive = true;
    api.slurmDefaults()
      .then((defaults) => { if (alive) setState({ defaults, error: null }); })
      .catch((error: Error) => { if (alive) setState((previous) => ({ defaults: previous.defaults, error })); });
    return () => { alive = false; };
  }, [enabled]);
  return state;
}

/** A length of time as the list shows it: whole hours, whole minutes, else seconds. */
export function slurmDurationText(t: TFunction<"slurm">, seconds: number): string {
  if (seconds > 0 && seconds % 3600 === 0) return t("duration.hours", { count: seconds / 3600 });
  if (seconds > 0 && seconds % 60 === 0) return t("duration.minutes", { count: seconds / 60 });
  return t("duration.seconds", { count: seconds });
}

/** Single-quoted for a shell, so a directory with a space or a quote reads as one word. */
const quoted = (path: string) => /^[A-Za-z0-9_./-]+$/.test(path) ? path : `'${path.replace(/'/g, "'\\''")}'`;

/** The commands that show why an entry's nodes did not join: on the controller what slurm.conf holds, on a compute node what the check left. */
export function slurmTroubleshootCommands(cluster: Pick<SlurmCluster, "install_dir" | "script_dir">): { controller: string; node: string } {
  return {
    controller: "scontrol show config | grep -i HealthCheck",
    node: [`ls -l ${quoted(`${cluster.script_dir}/check.sh`)}`, `tail -n 50 ${quoted(`${cluster.install_dir}/slurm-check.log`)}`].join("\n"),
  };
}

/** The entries made from one cloud account's cluster, by the cluster's id: at most one each. */
export function slurmEntryOf(entries: readonly SlurmCluster[], providerId: number, clusterId: string): SlurmCluster | undefined {
  return entries.find((entry) => entry.source?.provider_id === providerId && entry.source.provider_cluster_id === clusterId);
}
