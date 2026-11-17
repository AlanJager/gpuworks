import { storageKey } from "../components/resourceTable/columnPrefs";

const LEGACY_ORDER = ["served_name", "model", "runtime", "node", "targetNode", "state", "health", "replicas", "created_at"];
const TABLE_KEY = storageKey("deployments");
const MIGRATION_KEY = `${TABLE_KEY}.state-first`;

/** Upgrade only an unchanged default order; visibility, widths and paging are independent. */
export function migrateDeploymentColumnPrefs(storage?: Pick<Storage, "getItem" | "setItem">): void {
  try {
    storage ??= window.localStorage;
    if (storage.getItem(MIGRATION_KEY)) return;
    const raw = storage.getItem(TABLE_KEY);
    if (raw) {
      const prefs: unknown = JSON.parse(raw);
      if (prefs && typeof prefs === "object" && "order" in prefs && Array.isArray(prefs.order)) {
        const order = prefs.order;
        const known = order.filter((key): key is string => typeof key === "string" && LEGACY_ORDER.includes(key));
        const expected = LEGACY_ORDER.filter((key) => known.includes(key));
        if (known.includes("state") && known.includes("served_name") && known.length === expected.length && known.every((key, i) => key === expected[i])) {
          const next = order.map((key) => key === "state" ? "served_name" : key === "served_name" ? "state" : key);
          storage.setItem(TABLE_KEY, JSON.stringify({ ...prefs, order: next }));
        }
      }
    }
    // A later manual choice of the old order must remain the viewer's choice.
    storage.setItem(MIGRATION_KEY, "1");
  } catch {
    // Invalid or unavailable browser storage already falls back in ResourceTable.
  }
}
