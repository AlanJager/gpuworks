import assert from "node:assert/strict";
import test from "node:test";

import { navGroups, navItemForPath, sectionLabelKey } from "../src/navigation.tsx";

// Scheduling analysis is read alongside usage analytics: it sits in the
// overview group right below it, and its breadcrumb names that group.
test("scheduling analysis sits in the overview group right below usage analytics", () => {
  const overview = navGroups.find((group) => group.labelKey === "navigation.groups.overview");
  assert.ok(overview);
  const keys = overview.items.map((item) => item.key);
  assert.equal(keys.indexOf("/scheduling"), keys.indexOf("/analytics") + 1);
  const elsewhere = navGroups.filter((group) => group !== overview).flatMap((group) => group.items.map((item) => item.key));
  assert.equal(elsewhere.includes("/scheduling"), false, "listed once");
  assert.equal(navItemForPath("/scheduling")?.item.labelKey, "navigation.scheduling");
  assert.equal(sectionLabelKey("/scheduling"), "navigation.groups.overview");
});
