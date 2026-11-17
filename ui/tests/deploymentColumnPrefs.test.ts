import assert from "node:assert/strict";
import { test } from "node:test";
import { migrateDeploymentColumnPrefs } from "../src/pages/deploymentColumnPrefs.ts";
import { storageKey } from "../src/components/resourceTable/columnPrefs.ts";

const key = storageKey("deployments");
const oldOrder = ["served_name", "model", "runtime", "node", "targetNode", "state", "health", "replicas", "created_at"];
function storage(raw: string | null) {
  const values = new Map<string, string>();
  if (raw !== null) values.set(key, raw);
  return { getItem: (key: string) => values.get(key) ?? null, setItem: (key: string, value: string) => { values.set(key, value); } };
}

test("upgrades saved default order without losing hidden columns, widths or page size", () => {
  const prefs = { order: oldOrder, hidden: ["targetNode", "model"], widths: { name: 350, state: 180 }, pageSize: 50 };
  const store = storage(JSON.stringify(prefs));
  migrateDeploymentColumnPrefs(store);
  assert.deepEqual(JSON.parse(store.getItem(key)!), { ...prefs, order: ["state", ...oldOrder.slice(1, 5), "served_name", ...oldOrder.slice(6)] });
  // The user may deliberately return to the old order after the upgrade.
  store.setItem(key, JSON.stringify(prefs));
  migrateDeploymentColumnPrefs(store);
  assert.deepEqual(JSON.parse(store.getItem(key)!), prefs);
});

test("preserves genuinely reordered columns and page-size-only preferences", () => {
  for (const prefs of [{ order: ["model", "served_name", ...oldOrder.slice(2)], pageSize: 100 }, { pageSize: 50 }]) {
    const raw = JSON.stringify(prefs);
    const store = storage(raw);
    migrateDeploymentColumnPrefs(store);
    assert.equal(store.getItem(key), raw);
  }
});

test("recognizes older default column subsets and leaves unrelated storage alone", () => {
  const store = storage(JSON.stringify({ order: ["served_name", "model", "node", "state"], hidden: [] }));
  store.setItem(storageKey("nodes"), "unchanged");
  migrateDeploymentColumnPrefs(store);
  assert.deepEqual(JSON.parse(store.getItem(key)!).order, ["state", "model", "node", "served_name"]);
  assert.equal(store.getItem(storageKey("nodes")), "unchanged");
});

test("unreadable and blocked storage never prevent opening the page", () => {
  for (const raw of [null, "{broken", "null", "[]"]) {
    const store = storage(raw);
    assert.doesNotThrow(() => migrateDeploymentColumnPrefs(store));
    assert.equal(store.getItem(key), raw);
  }
  assert.doesNotThrow(() => migrateDeploymentColumnPrefs({ getItem() { throw new Error("blocked"); }, setItem() { throw new Error("blocked"); } }));
});
