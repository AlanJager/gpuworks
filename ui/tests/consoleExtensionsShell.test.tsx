// Console overlays in the frame: ui/src/App.tsx (extension routes before the
// not-found page, top bar controls), ui/src/app/AppSidebar.tsx (menu entries)
// and ui/src/pages/SettingsPage.tsx (tabs), each shown the way the server's
// GET /api/v1/console/manifest says: full, read-only, locked or hidden.
//
// The server is a fetch stub answering the frame's own reads and the manifest;
// every other request gets a 404, so pages show their own error state.
import assert from "node:assert/strict";
import fs from "node:fs";
import { register } from "node:module";
import { pathToFileURL } from "node:url";
import { after, afterEach, beforeEach, test } from "node:test";
import React from "react";
import { I18nextProvider, useTranslation } from "react-i18next";
import { dom, createI18n } from "./helpers/dom.ts";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

register(pathToFileURL(new URL("./deploySleepableCssHook.mjs", import.meta.url).pathname));
const closeChannels = trackMessageChannels();
for (const name of ["KeyboardEvent", "PointerEvent", "MouseEvent", "FocusEvent", "Document", "HTMLAnchorElement", "HTMLDivElement", "HTMLSpanElement", "HTMLImageElement", "DocumentFragment", "Text", "Event", "CustomEvent", "AbortController", "AbortSignal"] as const) {
  if (dom.window[name]) Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
if (!("getAnimations" in dom.window.Element.prototype)) {
  Object.defineProperty(dom.window.Element.prototype, "getAnimations", { configurable: true, value: () => [] });
}
class SilentEventSource { readyState = 0; addEventListener() {} removeEventListener() {} close() {} }
Object.defineProperty(globalThis, "EventSource", { value: SilentEventSource, configurable: true });
Object.defineProperty(globalThis, "localStorage", { value: dom.window.localStorage, configurable: true });
Object.defineProperty(globalThis, "sessionStorage", { value: dom.window.sessionStorage, configurable: true });

const namespaces = fs.readdirSync(new URL("../src/i18n/locales/en/", import.meta.url)).map((file) => file.replace(/\.json$/, ""));
const { i18n, locale } = await createI18n(["common", ...namespaces.filter((ns) => ns !== "common")]);
const common = locale.common;

const { cleanup, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { MemoryRouter } = await import("react-router-dom");
const { ConfigProvider } = await import("antd");
const { default: App } = await import("../src/App.tsx");
const registry = await import("../src/extensions/index.ts");
const { resetConsoleManifestForTest } = await import("../src/extensions/featureAccess.ts");
const { resetBrandingForTest } = await import("../src/branding.tsx");
const { resetAnimationFrameScheduler } = await import("@base-ui/utils/useAnimationFrame");
type SlotProps = import("../src/extensions/types.ts").SlotProps;
type ConsoleExtension = import("../src/extensions/types.ts").ConsoleExtension;

// A fake overlay: a page, a menu entry, a settings tab and a top bar control,
// all behind one feature key, with its own translations.
function AuditPage({ readOnly }: SlotProps) {
  const { t } = useTranslation();
  return (
    <section>
      <h2>{t("audit:page.title")}</h2>
      <p>{readOnly ? "mode: read-only" : "mode: writable"}</p>
      <button type="button" disabled={readOnly}>{t("audit:page.export")}</button>
    </section>
  );
}
function AuditSettings({ readOnly }: SlotProps) {
  return <p>{readOnly ? "audit settings: read-only" : "audit settings: writable"}</p>;
}
function AuditHeader({ readOnly }: SlotProps) {
  return <button type="button" aria-label={readOnly ? "Audit alerts (read-only)" : "Audit alerts"}>!</button>;
}
const auditExtension: ConsoleExtension = {
  id: "audit",
  routes: [{ path: "/audit", component: AuditPage, feature: "audit.log" }],
  navItems: [{ group: "admin", key: "/audit", icon: <span />, labelKey: "audit:nav.audit", feature: "audit.log" }],
  settingsTabs: [{ key: "audit", labelKey: "audit:settings.tab", component: AuditSettings, feature: "audit.log" }],
  headerItems: [{ key: "audit-alerts", component: AuditHeader, feature: "audit.log" }],
  locales: {
    en: { audit: { nav: { audit: "Audit log" }, page: { title: "Audit events", export: "Export" }, settings: { tab: "Audit" } } },
    zh: { audit: { nav: { audit: "审计日志" }, page: { title: "审计事件", export: "导出" }, settings: { tab: "审计" } } },
  },
};
registry.registerExtensionLocales(i18n, [auditExtension]);

const originalFetch = globalThis.fetch;
type Route = () => Response | Promise<Response>;
let unexpected: string[] = [];
let manifestReads = 0;
const frameRoutes: Record<string, Route> = {
  "/api/v1/auth/me": () => Response.json({ id: 1, username: "admin", role: "admin" }),
  "/api/v1/branding": () => Response.json({ product_name: "", console_title: "", login_description: "", logo: "" }),
  "/api/v1/notices": () => Response.json({ notices: [], unread: 0 }),
  "/api/v1/updates": () => Response.json({ code: "UPDATE_CHECK_FAILED", message: "channel unreachable" }, { status: 502 }),
};
function serve(manifest?: Route) {
  unexpected = [];
  manifestReads = 0;
  const routes: Record<string, Route> = { ...frameRoutes };
  if (manifest) routes["/api/v1/console/manifest"] = () => { manifestReads++; return manifest(); };
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    const path = new URL(String(input instanceof Request ? input.url : input), "http://localhost/").pathname;
    const route = routes[path];
    if (route) return route();
    unexpected.push(path);
    return Response.json({ code: "NOT_FOUND", message: `no stub for ${path}` }, { status: 404 });
  }) as typeof fetch;
}
// The manifest as the server writes it, with audit.log at the given access.
const manifestWith = (access: string) => () => new Response(
  `{"edition":"test","features":[{"key":"audit.log","access":"${access}","presentation":"promote","upsell_url":"https://example.com/audit"}],"nav":[],"banners":[]}`,
  { headers: { "Content-Type": "application/json" } },
);

beforeEach(() => {
  localStorage.clear();
  resetBrandingForTest();
  resetConsoleManifestForTest();
});
afterEach(() => {
  cleanup();
  resetAnimationFrameScheduler();
  globalThis.fetch = originalFetch;
  registry.setConsoleExtensionsForTest(undefined);
});
after(() => closeChannels());

async function mountApp(path: string) {
  render(
    <I18nextProvider i18n={i18n}>
      <ConfigProvider theme={{ token: { motion: false } }}>
        <MemoryRouter initialEntries={[path]}><App /></MemoryRouter>
      </ConfigProvider>
    </I18nextProvider>,
  );
  await screen.findByRole("complementary", { name: common.header.navigation });
}
const menu = () => screen.getByRole("complementary", { name: common.header.navigation });
// The frame's top bar (a page header below it is a banner too).
const topBar = () => document.querySelector<HTMLElement>("header.app-header")!;
const adminLinks = () => within(within(menu()).getByRole("region", { name: common.navigation.groups.admin })).getAllByRole("link");
const builtInTabs = ["general", "pull", "nodes", "access", "integrations", "data", "diagnostics"].map((key) => locale.settings.page.tabs[key]);

test("with no extension the frame is the built-in one and never asks for the manifest", async () => {
  serve();
  await mountApp("/audit");
  // The whole menu, group by group: name, then each entry's label and address.
  const regions = within(menu()).getAllByRole("region");
  const structure = regions.map((region) => [region.getAttribute("aria-label"), within(region).getAllByRole("link").map((link) => `${link.textContent} ${link.getAttribute("href")}`)]);
  assert.deepEqual(structure, [
    ["Overview", ["Overview /dashboard"]],
    ["Monitoring & Analytics", ["Monitor Center /monitor", "Scheduling analysis /scheduling", "Usage analytics /analytics"]],
    ["Compute", ["External Platforms /providers", "Compute Nodes /nodes", "GPU Devices /gpus", "Storage /storage", "Networks /networks"]],
    ["Models", ["Model Repository /models", "Inference Templates /inference-templates", "Images /images"]],
    ["Inference", ["Inference Services /deployments", "Batches /batches", "Playground /playground/chat", "Evaluations /evaluations"]],
    ["Administration", ["API Tokens /apikeys", "Logs /logs", "Settings /settings"]],
  ]);
  assert.ok(await screen.findByText(common.notFound.title), "an unregistered page is not found");
  assert.equal(within(topBar()).queryByRole("button", { name: /Audit alerts/ }), null);
  cleanup();
  await mountApp("/settings");
  await waitFor(() => assert.deepEqual(screen.getAllByRole("tab").map((tab) => tab.textContent), builtInTabs));
  assert.equal(manifestReads, 0);
  assert.equal(unexpected.includes("/api/v1/console/manifest"), false);
});

test("a full feature adds its page, menu entry, settings tab, top bar control and translations", async () => {
  registry.setConsoleExtensionsForTest([auditExtension]);
  serve(manifestWith("full"));
  const user = userEvent.setup();
  await mountApp("/dashboard");
  const entry = await within(menu()).findByRole("link", { name: "Audit log" });
  assert.deepEqual(adminLinks().map((link) => link.textContent), [common.navigation.apiKeys, common.navigation.logs, common.navigation.settings, "Audit log"]);
  assert.ok(within(topBar()).getByRole("button", { name: "Audit alerts" }));
  await user.click(entry);
  assert.ok(await screen.findByRole("heading", { name: "Audit events" }));
  assert.ok(screen.getByText("mode: writable"));
  assert.equal((screen.getByRole("button", { name: "Export" }) as HTMLButtonElement).disabled, false);
  await waitFor(() => assert.equal(within(menu()).getByRole("link", { name: "Audit log" }).getAttribute("aria-current"), "page"));
  assert.equal(manifestReads, 1, "the frame's readers share one manifest read");

  await user.click(within(menu()).getByRole("link", { name: common.navigation.settings }));
  await waitFor(() => assert.deepEqual(screen.getAllByRole("tab").map((tab) => tab.textContent), [...builtInTabs, "Audit"]));
  await user.click(screen.getByRole("tab", { name: "Audit" }));
  assert.ok(await screen.findByText("audit settings: writable"));
});

test("a read-only feature renders the same entries with readOnly set", async () => {
  registry.setConsoleExtensionsForTest([auditExtension]);
  serve(manifestWith("read_only"));
  await mountApp("/audit");
  assert.ok(await screen.findByText("mode: read-only"));
  assert.equal((screen.getByRole("button", { name: "Export" }) as HTMLButtonElement).disabled, true);
  assert.ok(within(topBar()).getByRole("button", { name: "Audit alerts (read-only)" }));
  assert.ok(within(menu()).getByRole("link", { name: "Audit log" }));
  cleanup();
  await mountApp("/settings#audit");
  assert.ok(await screen.findByText("audit settings: read-only"));
});

test("a locked feature keeps its menu entry, marked locked, and its page explains with the server's link instead of the content", async () => {
  registry.setConsoleExtensionsForTest([auditExtension]);
  serve(manifestWith("locked"));
  await mountApp("/audit");
  // The lock icon names itself, so the entry reads as the label followed by "Locked".
  const entry = await within(menu()).findByRole("link", { name: new RegExp(`^Audit log\\s*${common.consoleExtension.locked.badge}$`) });
  assert.equal(entry.getAttribute("href"), "/audit");
  assert.ok(await screen.findByText(common.consoleExtension.locked.title));
  const more = screen.getByRole("link", { name: common.consoleExtension.locked.learnMore });
  assert.equal(more.getAttribute("href"), "https://example.com/audit");
  assert.equal(more.getAttribute("target"), "_blank");
  assert.equal(screen.queryByRole("heading", { name: "Audit events" }), null, "the locked page shows none of the content");
  assert.equal(within(topBar()).queryByRole("button", { name: /Audit alerts/ }), null, "the top bar has no locked controls");
  cleanup();
  await mountApp("/settings#audit");
  assert.ok(await screen.findByText(common.consoleExtension.locked.title));
  assert.equal(screen.queryByText(/audit settings/), null);
});

test("a locked page without a server link offers none, and an extension's own locked page replaces the default", async () => {
  const locked = () => Response.json({ edition: "test", features: [{ key: "audit.log", access: "locked", presentation: "promote" }], nav: [], banners: [] });
  registry.setConsoleExtensionsForTest([auditExtension]);
  serve(locked);
  await mountApp("/audit");
  assert.ok(await screen.findByText(common.consoleExtension.locked.title));
  assert.equal(screen.queryByRole("link", { name: common.consoleExtension.locked.learnMore }), null);
  cleanup();
  resetConsoleManifestForTest();
  const OwnLocked = ({ link }: { link?: string }) => <p>{`audit is locked (${link ?? "no link"})`}</p>;
  registry.setConsoleExtensionsForTest([{ ...auditExtension, routes: [{ path: "/audit", component: AuditPage, feature: "audit.log", locked: OwnLocked }] }]);
  serve(locked);
  await mountApp("/audit");
  assert.ok(await screen.findByText("audit is locked (no link)"));
  assert.equal(screen.queryByText(common.consoleExtension.locked.title), null);
});

test("a hidden feature, or one the manifest does not list, leaves no trace and its page is not found", async () => {
  for (const manifest of [manifestWith("hidden"), () => Response.json({ edition: "test", features: [], nav: [], banners: [] })]) {
    registry.setConsoleExtensionsForTest([auditExtension]);
    resetConsoleManifestForTest();
    serve(manifest);
    await mountApp("/audit");
    assert.ok(await screen.findByText(common.notFound.title));
    await waitFor(() => assert.equal(manifestReads, 1));
    assert.deepEqual(adminLinks().map((link) => link.textContent), [common.navigation.apiKeys, common.navigation.logs, common.navigation.settings]);
    assert.equal(within(topBar()).queryByRole("button", { name: /Audit alerts/ }), null);
    cleanup();
    await mountApp("/settings#audit");
    await waitFor(() => assert.deepEqual(screen.getAllByRole("tab").map((tab) => tab.textContent), builtInTabs));
    cleanup();
  }
});

test("until the manifest answers, a feature page waits and its menu entry stays out; a failed read hides it", async () => {
  registry.setConsoleExtensionsForTest([auditExtension]);
  serve(() => new Promise<Response>(() => {}));
  await mountApp("/audit");
  const waiting = await screen.findByText(common.consoleExtension.loading);
  assert.ok(waiting.closest("[role=status]"), "the wait is announced");
  assert.equal(within(menu()).queryByRole("link", { name: /Audit log/ }), null);
  cleanup();
  resetConsoleManifestForTest();
  serve(() => Response.json({ code: "INTERNAL", message: "boom" }, { status: 500 }));
  await mountApp("/audit");
  assert.ok(await screen.findByText(common.notFound.title), "fail closed: a page the server did not confirm is not found");
});

test("the extension's menu entry is reached with the arrow keys and opened with Enter", async () => {
  registry.setConsoleExtensionsForTest([auditExtension]);
  serve(manifestWith("full"));
  const user = userEvent.setup();
  await mountApp("/apikeys");
  await within(menu()).findByRole("link", { name: "Audit log" });
  within(menu()).getByRole("link", { name: common.navigation.settings }).focus();
  await user.keyboard("{ArrowDown}");
  assert.equal(document.activeElement?.textContent, "Audit log");
  await user.keyboard("{Enter}");
  assert.ok(await screen.findByRole("heading", { name: "Audit events" }));
});
