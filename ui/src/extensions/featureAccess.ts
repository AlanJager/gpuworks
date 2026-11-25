import { useCallback, useEffect, useSyncExternalStore } from "react";
import { consoleManifestApi, type ConsoleBanner, type ConsoleFeature, type ConsolePlatform, type FeatureAccess } from "../consoleManifest";

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
  banners: readonly ConsoleBanner[];
  /** The limit on the whole server; absent while nothing is limited. */
  platform?: ConsolePlatform;
}

const knownAccess: ReadonlySet<string> = new Set<FeatureAccess>(["full", "read_only", "locked", "hidden"]);
const FULL: Availability = { access: "full" };
const PENDING: Availability = { access: "pending" };
const HIDDEN: Availability = { access: "hidden" };

const NO_BANNERS: readonly ConsoleBanner[] = [];
let snapshot: Snapshot = { status: "idle", features: new Map(), banners: NO_BANNERS };
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
 * loadConsoleManifest (re)reads GET /api/v1/console/manifest. Every reader in
 * the console (the banners and each feature-keyed extension entry) shares this
 * one read. A failed first read hides every feature-keyed entry (fail closed)
 * and shows no banners; a failed refresh keeps the last answer.
 */
export function loadConsoleManifest(): Promise<void> {
  const mine = ++generation;
  if (snapshot.status !== "ready") publish({ ...snapshot, status: "loading" });
  return consoleManifestApi.manifest().then(
    (manifest) => {
      if (mine !== generation) return;
      publish({
        status: "ready",
        features: new Map((manifest.features ?? []).map((feature) => [feature.key, feature])),
        banners: manifest.banners ?? NO_BANNERS,
        platform: knownPlatform(manifest.platform),
      });
    },
    () => {
      if (mine !== generation) return;
      if (snapshot.status !== "ready") publish({ status: "failed", features: new Map(), banners: NO_BANNERS });
    },
  );
}

/**
 * knownPlatform is the server's limit as this console understands it. A limit
 * with an access this console does not know is shown as locked, the stricter
 * of the two notices.
 */
function knownPlatform(platform: ConsolePlatform | undefined): ConsolePlatform | undefined {
  if (!platform) return undefined;
  return platform.access === "read_only" ? platform : { ...platform, access: "locked" };
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
  return entry.link ? { access: entry.access, link: entry.link } : { access: entry.access };
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

/**
 * useConsoleBanners returns the server's console banners, reading the manifest
 * if nothing has asked yet. It is empty while the manifest loads and when it
 * cannot be read: banners are never what a page needs to work.
 */
export function useConsoleBanners(): readonly ConsoleBanner[] {
  const current = useSyncExternalStore(subscribe, () => snapshot, () => snapshot);
  useEffect(() => {
    if (snapshot.status === "idle") void loadConsoleManifest();
  }, []);
  return current.banners;
}

/**
 * PlatformState is the limit on the whole server as the shell needs it.
 * settled is false until the manifest has answered or failed; a console that
 * has extensions waits for it before showing a page, so a limited server's
 * page never shows without its notice.
 */
export interface PlatformState {
  platform?: ConsolePlatform;
  settled: boolean;
}

/**
 * useConsolePlatform returns the limit on the whole server, reading the
 * manifest if nothing has asked yet. A manifest that cannot be read limits
 * nothing here: the server refuses what it does not serve either way.
 */
export function useConsolePlatform(): PlatformState {
  const current = useSyncExternalStore(subscribe, () => snapshot, () => snapshot);
  useEffect(() => {
    if (snapshot.status === "idle") void loadConsoleManifest();
  }, []);
  return { platform: current.platform, settled: current.status === "ready" || current.status === "failed" };
}

/** resetConsoleManifestForTest forgets the manifest, so the next test reads it again. */
export function resetConsoleManifestForTest() {
  generation++;
  publish({ status: "idle", features: new Map(), banners: NO_BANNERS });
}
