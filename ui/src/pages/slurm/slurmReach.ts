import type { TFunction } from "i18next";
import type { SlurmCluster, SlurmReachHost } from "../../slurmApi";

/**
 * A host that cannot reach the server's address, in words: "Compute node
 * gpu000 cannot reach https://…: timed out". The role, the host and the reason
 * are the server's codes; a reason this console has no text for is shown as
 * the code.
 */
export function slurmReachFailure(t: TFunction<"slurm">, url: string, host: SlurmReachHost): string {
  return t("reach.failed", {
    role: t(`reach.role.${host.role}`, { defaultValue: host.role }), name: host.name, url,
    reason: t(`reach.reason.${host.reason ?? "other"}`, { defaultValue: host.reason ?? "" }),
  });
}

/** A host that could not be asked, and why: "gpu001 (Cloud Assistant is offline)". */
export function slurmReachUnasked(t: TFunction<"slurm">, host: SlurmReachHost): string {
  return `${host.name} (${t(`reach.reason.${host.reason ?? "not_asked"}`, { defaultValue: host.reason ?? "" })})`;
}

/**
 * Why an entry has no node yet, in one sentence, from the server's reason and
 * its hosts; "" when the server gives none (the caller says its generic line).
 */
export function slurmWaitingText(t: TFunction<"slurm">, cluster: SlurmCluster): string {
  const waiting = cluster.waiting;
  if (!waiting) return "";
  const hosts = (waiting.hosts ?? []).map((host) => `${host.name} (${t(`reach.reason.${host.reason ?? "other"}`, { defaultValue: host.reason ?? "" })})`).join(", ");
  return t(`waiting.${waiting.reason}`, { url: cluster.server_url, hosts, defaultValue: "" });
}
