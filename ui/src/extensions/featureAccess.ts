import { useCallback, useEffect, useSyncExternalStore } from "react";
import { consoleManifestApi, type ConsoleFeature, type FeatureAccess } from "../consoleManifestApi";

/**
 * Availability is how the console shows one feature-keyed entry. "pending"
 * means the manifest has not answered yet: menus leave the entry out and a
 * page waits, so nothing flashes before the server has spoken.
 */
export interface Availability {
  access: FeatureAccess | "pending";
  /** The server's "learn more" address for a locked entry. */
  link?: string;
}

type Status = "idle" | "loading" | "ready" | "failed";
interface Snapshot {
  status: Status;
  features: ReadonlyMap<string, ConsoleFeature>;
}

const knownAccess: ReadonlySet<string> = new Set<FeatureAccess>(["full", "read_only", "locked", "hidden"]);
const FULL: Availability = { access: "full" };
const PENDING: Availability = { access: "pending" };
const HIDDEN: Availability = { access: "hidden" };

let snapshot: Snapshot = { status: "idle", features: new Map() };
let generation = 0;
const listeners = new Set<() => void>();

function publish(next: Snapshot) {
  snapshot = next;
  for (const listener of [...listeners]) listener();
}

function subscribe(listener: () => void) {
  listeners.add(listener);
  return () => { listeners.delete(listener); };
}

/**
 * loadConsoleManifest (re)reads GET /api/v1/console/manifest. The console asks
 * only when a registered extension entry carries a feature key, so a console
 * without extensions never calls it. A failed first read hides every
 * feature-keyed entry (fail closed); a failed refresh keeps the last answer.
 */
export function loadConsoleManifest(): Promise<void> {
  const mine = ++generation;
  if (snapshot.status !== "ready") publish({ status: "loading", features: snapshot.features });
  return consoleManifestApi.get().then(
    (manifest) => {
      if (mine !== generation) return;
      publish({ status: "ready", features: new Map((manifest.features ?? []).map((feature) => [feature.key, feature])) });
    },
    () => {
      if (mine !== generation) return;
      if (snapshot.status !== "ready") publish({ status: "failed", features: new Map() });
    },
  );
}

/**
 * availabilityIn answers one entry from a manifest snapshot. An entry without a
 * feature key is always full; a key the manifest does not list, or lists with an
 * access this console does not know, is hidden.
 */
function availabilityIn(current: Snapshot, feature: string | undefined): Availability {
  if (!feature) return FULL;
  if (current.status === "idle" || current.status === "loading") return PENDING;
  const entry = current.features.get(feature);
  if (!entry || !knownAccess.has(entry.access)) return HIDDEN;
  return entry.upsell_url ? { access: entry.access, link: entry.upsell_url } : { access: entry.access };
}

/**
 * useFeatureResolver returns a function answering each entry's availability
 * from the server's console manifest, and loads the manifest when needed is
 * true and nothing has asked yet.
 */
export function useFeatureResolver(needed: boolean): (feature?: string) => Availability {
  const current = useSyncExternalStore(subscribe, () => snapshot, () => snapshot);
  useEffect(() => {
    if (needed && snapshot.status === "idle") void loadConsoleManifest();
  }, [needed]);
  return useCallback((feature?: string) => availabilityIn(current, feature), [current]);
}

/** resetConsoleManifestForTest forgets the manifest, so the next test reads it again. */
export function resetConsoleManifestForTest() {
  generation++;
  publish({ status: "idle", features: new Map() });
}
