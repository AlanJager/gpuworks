import assert from "node:assert/strict";
import test from "node:test";

import { navGroups, navItemForPath, sectionLabelKey, selectedNavKey } from "../src/navigation.tsx";

const groupKeys = () => navGroups.map((group) => group.labelKey);
const keysIn = (labelKey: string) => navGroups.find((group) => group.labelKey === labelKey)?.items.map((item) => item.key);

// Monitoring and analysis lead, then daily work, then the platform; new
// features go to the end of a group, so this order is the contract the menu keeps.
test("the overview sits above the groups, which run from monitoring through serving down to administration", () => {
  assert.equal(navGroups[0].labelKey, undefined);
  assert.deepEqual(navGroups[0].items.map((item) => item.key), ["/dashboard"]);
  assert.deepEqual(groupKeys().slice(1), [
    "navigation.groups.observability",
    "navigation.groups.inference",
    "navigation.groups.models",
    "navigation.groups.compute",
    "navigation.groups.admin",
  ]);
  assert.deepEqual(keysIn("navigation.groups.inference"), ["/deployments", "/batches", "/playground/chat", "/evaluations"]);
  assert.deepEqual(keysIn("navigation.groups.models"), ["/models", "/inference-templates", "/images"]);
  assert.deepEqual(keysIn("navigation.groups.compute"), ["/nodes", "/gpus", "/storage", "/networks", "/providers", "/zstack"]);
  assert.deepEqual(keysIn("navigation.groups.observability"), ["/monitor", "/scheduling", "/analytics", "/logs"]);
  assert.deepEqual(keysIn("navigation.groups.admin"), ["/apikeys", "/settings"]);
});

// The group leads the menu, so it is capped: further views become tabs.
test("monitoring and analysis holds at most five entries", () => {
  assert.ok((keysIn("navigation.groups.observability") ?? []).length <= 5);
});

test("every entry is listed once", () => {
  const keys = navGroups.flatMap((group) => group.items.map((item) => item.key));
  assert.equal(new Set(keys).size, keys.length);
});

// Folded, only the icons show, so two entries must never share one.
test("no two entries share an icon", () => {
  const icons = navGroups.flatMap((group) => group.items.map((item) => (item.icon as { type: { displayName?: string; render?: { displayName?: string } } }).type));
  assert.equal(new Set(icons).size, icons.length);
});

// Deploying is the inference services list's create action, not a menu entry.
test("the deploy page has no entry of its own: it highlights inference services under that group", () => {
  assert.equal(navGroups.some((group) => group.items.some((item) => item.key === "/deploy")), false);
  assert.equal(selectedNavKey("/deploy"), "/deployments");
  assert.equal(navItemForPath("/deploy")?.item.labelKey, "navigation.deployments");
  // The deploy page links back to the list (same name as the group) instead.
  assert.equal(sectionLabelKey("/deploy"), undefined);
});

// The list is named after its group: a crumb would repeat the title.
test("the inference services list and the overview show no section crumb; other pages name their group", () => {
  assert.equal(sectionLabelKey("/deployments"), undefined);
  assert.equal(sectionLabelKey("/dashboard"), undefined);
  assert.equal(sectionLabelKey("/batches"), "navigation.groups.inference");
  assert.equal(sectionLabelKey("/scheduling"), "navigation.groups.observability");
  assert.equal(sectionLabelKey("/logs"), "navigation.groups.observability");
  assert.equal(sectionLabelKey("/providers"), "navigation.groups.compute");
});
