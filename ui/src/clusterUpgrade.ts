// A cluster upgrade closes every member's business for a short window; each
// API call then answers 503 CLUSTER_UPGRADE_IN_PROGRESS. The first such
// answer switches the console to its upgrade screen, which follows the
// public progress document and reloads the console when service is back.

/** The product code of a business answer during a cluster upgrade. */
export const CLUSTER_UPGRADE_CODE = "CLUSTER_UPGRADE_IN_PROGRESS";

/** The public progress document; any member answers it, open or closed. */
export const CLUSTER_UPGRADE_PROGRESS_PATH = "/api/v1/cluster/upgrade-progress";

/** The wire form of the progress document. state is a server code. */
export interface ClusterUpgradeProgress {
  state: string;
  business_open: boolean;
  schema: { applied: number; known: number };
  members: { total: number; on_target: number; fresh: number };
}

const listeners = new Set<() => void>();
let active = false;

/** Called for every API answer that carries the upgrade code. */
export function notifyClusterUpgrade() {
  if (active) return;
  active = true;
  for (const listener of listeners) listener();
}

export function isClusterUpgradeActive() { return active; }

export function subscribeClusterUpgrade(listener: () => void): () => void {
  listeners.add(listener);
  return () => { listeners.delete(listener); };
}

/** Tests return the module to its initial state. */
export function resetClusterUpgradeForTest() { active = false; }

/**
 * readClusterUpgradeProgress reads the progress document. "done" means the
 * answering member is serving again with no upgrade under way; "unknown"
 * means the document could not be read now (a member restarting, the entry
 * between members) and the caller asks again.
 */
export async function readClusterUpgradeProgress(signal?: AbortSignal): Promise<{ progress?: ClusterUpgradeProgress; outcome: "running" | "done" | "unknown" }> {
  let response: Response;
  try {
    response = await fetch(CLUSTER_UPGRADE_PROGRESS_PATH, { cache: "no-store", credentials: "omit", signal });
  } catch {
    return { outcome: "unknown" };
  }
  if (response.status === 404) {
    // A server without the document: it is serving again when it is ready.
    try {
      const ready = await fetch("/api/v1/readyz", { cache: "no-store", credentials: "omit", signal });
      return { outcome: ready.ok ? "done" : "unknown" };
    } catch {
      return { outcome: "unknown" };
    }
  }
  if (!response.ok) return { outcome: "unknown" };
  let progress: ClusterUpgradeProgress;
  try {
    progress = await response.json() as ClusterUpgradeProgress;
  } catch {
    return { outcome: "unknown" };
  }
  if (typeof progress?.state !== "string" || !progress.schema || !progress.members) return { outcome: "unknown" };
  return { progress, outcome: progress.state === "idle" && progress.business_open === true ? "done" : "running" };
}
