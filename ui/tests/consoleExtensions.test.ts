// The console overlay registry (ui/src/extensions/index.ts) and the manifest
// access it renders by (ui/src/extensions/featureAccess.ts): how extension
// entries merge into the built-in menu and settings tabs, which entries the
// server's manifest keeps, hides or locks, and what a clash refuses.
import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import { createElement } from "react";
import { createInstance } from "i18next";
import "./helpers/dom.ts";

const { renderHook, waitFor, cleanup } = await import("@testing-library/react");
const registry = await import("../src/extensions/index.ts");
const featureAccess = await import("../src/extensions/featureAccess.ts");
const { navGroups, sectionLabelKey, selectedNavKey } = await import("../src/navigation.tsx");
type ConsoleExtension = import("../src/extensions/types.ts").ConsoleExtension;
type Availability = import("../src/extensions/featureAccess.ts").Availability;

const Page = () => null;
const icon = createElement("span");
const originalFetch = globalThis.fetch;

afterEach(() => {
  cleanup();
  globalThis.fetch = originalFetch;
  registry.setConsoleExtensionsForTest(undefined);
  featureAccess.resetConsoleManifestForTest();
});

// What the server would answer, by key; a key not listed is absent from the manifest.
function accessFrom(table: Record<string, Availability>) {
  return (feature?: string): Availability => (feature ? table[feature] ?? { access: "hidden" } : { access: "full" });
}

const audit: ConsoleExtension = {
  id: "audit",
  navGroups: [{ id: "governance", labelKey: "audit:nav.group" }],
  navItems: [
    { group: "governance", key: "/audit", icon, labelKey: "audit:nav.audit", feature: "audit.log" },
    { group: "admin", key: "/quotas", icon, labelKey: "audit:nav.quotas", feature: "quota.write", also: ["quota"] },
    { group: "compute", key: "/pools", icon, labelKey: "audit:nav.pools" },
  ],
  settingsTabs: [{ key: "audit", labelKey: "audit:settings.tab", component: Page, feature: "audit.log", anchors: ["audit-retention"] }],
};

test("the community build registers no extension: every merge returns the built-in list itself", () => {
  assert.deepEqual(registry.consoleExtensions(), []);
  assert.deepEqual(registry.extensionRoutes(), []);
  assert.deepEqual(registry.extensionHeaderItems(), []);
  assert.equal(registry.needsManifest(), false, "no feature key, so the console never asks for the manifest");
  const all = accessFrom({});
  assert.equal(registry.mergeNavGroups(navGroups, [], all), navGroups);
  const tabs = [{ key: "general", anchors: [] }];
  assert.equal(registry.mergeSettingsTabs(tabs, [], all), tabs);
});

test("built-in menu groups carry stable ids extensions file entries under", () => {
  assert.deepEqual(navGroups.map((group) => group.id), ["overview", "observability", "compute", "models", "inference", "admin"]);
});

test("extension entries go to the end of their group; a new group sits before administration", () => {
  const merged = registry.mergeNavGroups(navGroups, [audit], accessFrom({ "audit.log": { access: "full" }, "quota.write": { access: "read_only" } }));
  assert.deepEqual(merged.map((group) => group.id), ["overview", "observability", "compute", "models", "inference", "governance", "admin"]);
  const keys = (id: string) => merged.find((group) => group.id === id)?.items.map((item) => item.key);
  assert.deepEqual(keys("governance"), ["/audit"]);
  assert.deepEqual(keys("admin"), ["/apikeys", "/logs", "/settings", "/quotas"]);
  assert.deepEqual(keys("compute"), ["/providers", "/nodes", "/gpus", "/storage", "/networks", "/pools"]);
  assert.equal(merged.find((group) => group.id === "governance")?.labelKey, "audit:nav.group");
  // The built-in list is untouched.
  assert.deepEqual(navGroups.find((group) => group.id === "admin")?.items.map((item) => item.key), ["/apikeys", "/logs", "/settings"]);
  // An extension entry is found by path like a built-in one, through its also list too.
  assert.equal(selectedNavKey("/audit/42", merged), "/audit");
  assert.equal(selectedNavKey("/quota", merged), "/quotas");
  assert.equal(sectionLabelKey("/audit", merged), "audit:nav.group");
  assert.equal(selectedNavKey("/audit"), "/audit", "the built-in lookup alone falls back to the path's section");
  assert.equal(sectionLabelKey("/audit"), undefined);
});

test("the manifest decides each entry: hidden, unknown and not-yet-answered entries are left out, locked ones stay marked", () => {
  const merged = registry.mergeNavGroups(navGroups, [audit], accessFrom({
    "audit.log": { access: "hidden" },
    "quota.write": { access: "locked", link: "https://example.com/more" },
  }));
  assert.equal(merged.some((group) => group.id === "governance"), false, "a group whose entries are all hidden is dropped");
  const admin = merged.find((group) => group.id === "admin")!;
  assert.deepEqual(admin.items.map((item) => [item.key, item.locked ?? false]), [["/apikeys", false], ["/logs", false], ["/settings", false], ["/quotas", true]]);
  assert.deepEqual(admin.items.at(-1)?.also, ["quota"]);
  // Before the manifest answers, every key is pending: feature-keyed entries wait,
  // and an entry without a feature is shown without asking (the resolver is not consulted).
  const pending = registry.mergeNavGroups(navGroups, [audit], () => ({ access: "pending" }));
  assert.equal(pending.some((group) => group.id === "governance"), false);
  assert.deepEqual(pending.find((group) => group.id === "admin")?.items.map((item) => item.key), ["/apikeys", "/logs", "/settings"]);
  assert.equal(pending.find((group) => group.id === "compute")?.items.at(-1)?.key, "/pools", "an entry without a feature is always shown");
});

test("a clash with the built-in menu or between extensions fails loudly", () => {
  const all = accessFrom({});
  assert.throws(() => registry.mergeNavGroups(navGroups, [{ id: "x1", navItems: [{ group: "admin", key: "/settings", icon, labelKey: "x" }] }], all), /menu entry \/settings is built in/);
  assert.throws(() => registry.mergeNavGroups(navGroups, [{ id: "x1", navItems: [{ group: "nowhere", key: "/x", icon, labelKey: "x" }] }], all), /unknown group nowhere/);
  assert.throws(() => registry.mergeNavGroups(navGroups, [{ id: "x1", navGroups: [{ id: "admin", labelKey: "x" }] }], all), /menu group admin is built in/);
  assert.throws(() => registry.setConsoleExtensionsForTest([{ id: "a1", routes: [{ path: "/x", component: Page }] }, { id: "b1", routes: [{ path: "/x", component: Page }] }]), /route \/x is already contributed by a1/);
  assert.throws(() => registry.setConsoleExtensionsForTest([{ id: "a1" }, { id: "a1" }]), /extension a1 is already contributed/);
  assert.throws(() => registry.setConsoleExtensionsForTest([{ id: "Bad-Id" }]), /must match/);
  assert.throws(() => registry.setConsoleExtensionsForTest([{ id: "a1", headerItems: [{ key: "h", component: Page }] }, { id: "b1", headerItems: [{ key: "h", component: Page }] }]), /top bar control h/);
  assert.throws(() => registry.mergeSettingsTabs([{ key: "general", anchors: ["updates"] }], [{ id: "x1", settingsTabs: [{ key: "t", labelKey: "x", anchors: ["updates"], component: Page }] }], all), /settings fragment updates is built in/);
});

test("extensions are registered in id order, and their entries follow it", () => {
  registry.setConsoleExtensionsForTest([
    { id: "zeta", routes: [{ path: "/z", component: Page }], headerItems: [{ key: "z", component: Page }] },
    { id: "alpha", routes: [{ path: "/a", component: Page, feature: "a.read" }], headerItems: [{ key: "a", component: Page }] },
  ]);
  assert.deepEqual(registry.consoleExtensions().map((extension) => extension.id), ["alpha", "zeta"]);
  assert.deepEqual(registry.extensionRoutes().map((route) => route.path), ["/a", "/z"]);
  assert.deepEqual(registry.extensionHeaderItems().map((item) => item.key), ["a", "z"]);
  assert.equal(registry.needsManifest(), true);
  registry.setConsoleExtensionsForTest(undefined);
  assert.deepEqual(registry.consoleExtensions(), [], "undefined restores what the build found");
});

test("settings tabs follow the built-in ones with their availability; hidden ones are left out", () => {
  const base = [{ key: "general", anchors: ["branding"] }];
  const merged = registry.mergeSettingsTabs(base, [audit], accessFrom({ "audit.log": { access: "locked", link: "https://example.com/more" } }));
  assert.equal(merged.length, 2);
  const tab = merged[1];
  assert.ok(registry.isExtensionTab(tab));
  assert.equal(registry.isExtensionTab(merged[0]), false);
  assert.deepEqual({ key: tab.key, anchors: tab.anchors, labelKey: tab.labelKey, availability: tab.availability },
    { key: "audit", anchors: ["audit-retention"], labelKey: "audit:settings.tab", availability: { access: "locked", link: "https://example.com/more" } });
  assert.deepEqual(registry.mergeSettingsTabs(base, [audit], accessFrom({})), base);
});

test("extension translations merge into i18next without replacing built-in text", async () => {
  const i18n = createInstance();
  await i18n.init({ lng: "en", resources: { en: { common: { apiError: { NOT_FOUND: "Not found" } } } }, interpolation: { escapeValue: false } });
  registry.registerExtensionLocales(i18n, [{
    id: "audit",
    locales: {
      en: { audit: { nav: { audit: "Audit log" } }, common: { apiError: { NOT_FOUND: "replaced", AUDIT_EXPORT_FAILED: "Export failed" } } },
      zh: { audit: { nav: { audit: "审计日志" } } },
    },
  }]);
  assert.equal(i18n.t("audit:nav.audit"), "Audit log");
  assert.equal(i18n.t("common:apiError.AUDIT_EXPORT_FAILED"), "Export failed", "an extension adds its codes to a built-in namespace");
  assert.equal(i18n.t("common:apiError.NOT_FOUND"), "Not found", "but never replaces built-in text");
  assert.equal(i18n.getFixedT("zh")("audit:nav.audit"), "审计日志");
});

// GET /api/v1/console/manifest as the server writes it.
const manifestWire = `{"edition":"community-plus","features":[
  {"key":"audit.log","access":"full","presentation":"promote"},
  {"key":"quota.write","access":"read_only","presentation":"promote"},
  {"key":"sso.login","access":"locked","presentation":"promote","link":"https://example.com/sso"},
  {"key":"pools.exclusive","access":"hidden","presentation":"hidden"},
  {"key":"future.key","access":"suspended"}
],"nav":[],"banners":[]}`;

function serveManifest(answer: () => Promise<Response>) {
  const calls: string[] = [];
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    calls.push(String(input));
    return answer();
  }) as typeof fetch;
  return calls;
}

test("the resolver reads the manifest once and answers each key from it, failing closed on what it does not know", async () => {
  const calls = serveManifest(async () => new Response(manifestWire, { status: 200, headers: { "Content-Type": "application/json" } }));
  const { result } = renderHook(() => featureAccess.useFeatureResolver(true));
  assert.deepEqual(result.current("audit.log"), { access: "pending" }, "nothing is decided before the server answers");
  assert.deepEqual(result.current(undefined), { access: "full" }, "an entry without a feature never waits");
  await waitFor(() => assert.deepEqual(result.current("audit.log"), { access: "full" }));
  assert.deepEqual(result.current("quota.write"), { access: "read_only" });
  assert.deepEqual(result.current("sso.login"), { access: "locked", link: "https://example.com/sso" });
  assert.deepEqual(result.current("pools.exclusive"), { access: "hidden" });
  assert.deepEqual(result.current("future.key"), { access: "hidden" }, "an access this console does not know is hidden");
  assert.deepEqual(result.current("not.listed"), { access: "hidden" }, "a key the manifest does not list is hidden");
  assert.deepEqual(calls, ["/api/v1/console/manifest"]);
  renderHook(() => featureAccess.useFeatureResolver(true));
  assert.equal(calls.length, 1, "a second reader shares the answer");
});

test("a console that needs no manifest never asks for it", async () => {
  const calls = serveManifest(async () => new Response(manifestWire));
  const { result } = renderHook(() => featureAccess.useFeatureResolver(false));
  await new Promise((resolve) => setTimeout(resolve, 10));
  assert.deepEqual(calls, []);
  assert.deepEqual(result.current(undefined), { access: "full" });
});

test("a failed first read hides every feature-keyed entry; a failed refresh keeps the last answer", async () => {
  serveManifest(async () => Response.json({ code: "INTERNAL", message: "boom" }, { status: 500 }));
  const { result } = renderHook(() => featureAccess.useFeatureResolver(true));
  await waitFor(() => assert.deepEqual(result.current("audit.log"), { access: "hidden" }));

  featureAccess.resetConsoleManifestForTest();
  serveManifest(async () => new Response(manifestWire));
  await featureAccess.loadConsoleManifest();
  await waitFor(() => assert.deepEqual(result.current("audit.log"), { access: "full" }));
  serveManifest(async () => { throw new TypeError("network down"); });
  await featureAccess.loadConsoleManifest();
  assert.deepEqual(result.current("audit.log"), { access: "full" }, "a refresh that fails changes nothing");
});

test("a refresh replaces the answer, and a slower earlier read cannot overwrite a newer one", async () => {
  let release: (response: Response) => void = () => {};
  serveManifest(() => new Promise<Response>((resolve) => { release = resolve; }));
  const { result } = renderHook(() => featureAccess.useFeatureResolver(true));
  await waitFor(() => assert.deepEqual(result.current("audit.log"), { access: "pending" }));
  const slow = release;
  serveManifest(async () => new Response(`{"edition":"x","features":[{"key":"audit.log","access":"locked"}],"nav":[],"banners":[]}`));
  await featureAccess.loadConsoleManifest();
  await waitFor(() => assert.deepEqual(result.current("audit.log"), { access: "locked" }));
  slow(new Response(manifestWire));
  await new Promise((resolve) => setTimeout(resolve, 10));
  assert.deepEqual(result.current("audit.log"), { access: "locked" }, "the stale read is dropped");
});
