// Console overlays' account menu entries (group "account", rendered by
// ui/src/components/UserMenu.tsx after the built-in entries and before signing
// out) and admin-only entries (admin: true) across the frame: the page in
// ui/src/App.tsx, the side menu, the settings tabs and the top bar, shown by
// the signed-in user's role from the session read (/api/v1/auth/me).
//
// The server is a fetch stub answering the frame's own reads and the manifest;
// every other request gets a 404.
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
const { MemoryRouter, Route, Routes, useLocation } = await import("react-router-dom");
const { ConfigProvider } = await import("antd");
const { default: App } = await import("../src/App.tsx");
const UserMenu = (await import("../src/components/UserMenu.tsx")).default;
const { SessionUserContext } = await import("../src/components/sessionUser.ts");
const registry = await import("../src/extensions/index.ts");
const { resetConsoleManifestForTest } = await import("../src/extensions/featureAccess.ts");
const { resetBrandingForTest } = await import("../src/branding.tsx");
const { resetAnimationFrameScheduler } = await import("@base-ui/utils/useAnimationFrame");
type ConsoleExtension = import("../src/extensions/types.ts").ConsoleExtension;

// A fake overlay: an admin-only page opened from the account menu, an account
// entry for everyone behind a feature key, and admin-only entries of every
// other kind.
function KeysPage() {
  const { t } = useTranslation();
  return <h2>{t("keys:page.title")}</h2>;
}
const KeysSettings = () => <p>keys settings</p>;
const KeysHeader = () => <button type="button" aria-label="Key alerts">!</button>;
const keysExtension: ConsoleExtension = {
  id: "keys",
  routes: [{ path: "/x/keys", component: KeysPage, admin: true }],
  navItems: [
    { group: "account", key: "/x/keys", labelKey: "keys:menu.manage", admin: true },
    { group: "account", key: "/x/profile", labelKey: "keys:menu.profile", feature: "profile.view" },
    { group: "admin", key: "/x/keys-audit", icon: <span />, labelKey: "keys:nav.audit", admin: true },
  ],
  settingsTabs: [{ key: "keys", labelKey: "keys:settings.tab", component: KeysSettings, admin: true }],
  headerItems: [{ key: "keys-alerts", component: KeysHeader, admin: true }],
  locales: {
    en: { keys: { menu: { manage: "Manage keys", profile: "Profile" }, page: { title: "Keys" }, nav: { audit: "Key audit" }, settings: { tab: "Keys" } } },
    zh: { keys: { menu: { manage: "管理密钥", profile: "个人资料" }, page: { title: "密钥" }, nav: { audit: "密钥审计" }, settings: { tab: "密钥" } } },
  },
};
registry.registerExtensionLocales(i18n, [keysExtension]);

const originalFetch = globalThis.fetch;
type Route = () => Response | Promise<Response>;
function serve(me: Route, profileAccess = "full") {
  const routes: Record<string, Route> = {
    "/api/v1/auth/me": me,
    "/api/v1/branding": () => Response.json({ product_name: "", console_title: "", login_description: "", logo: "" }),
    "/api/v1/notices": () => Response.json({ notices: [], unread: 0 }),
    "/api/v1/updates": () => Response.json({ code: "UPDATE_CHECK_FAILED", message: "channel unreachable" }, { status: 502 }),
    "/api/v1/console/manifest": () => new Response(
      `{"edition":"test","features":[{"key":"profile.view","access":"${profileAccess}","presentation":"promote"}],"nav":[],"banners":[]}`,
      { headers: { "Content-Type": "application/json" } },
    ),
  };
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    const path = new URL(String(input instanceof Request ? input.url : input), "http://localhost/").pathname;
    const route = routes[path];
    if (route) return route();
    return Response.json({ code: "NOT_FOUND", message: `no stub for ${path}` }, { status: 404 });
  }) as typeof fetch;
}
const asRole = (role: string) => () => Response.json({ id: 1, username: "ops", role });

beforeEach(() => {
  localStorage.clear();
  resetBrandingForTest();
  resetConsoleManifestForTest();
  registry.setConsoleExtensionsForTest([keysExtension]);
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
const sideMenu = () => screen.getByRole("complementary", { name: common.header.navigation });
const topBar = () => document.querySelector<HTMLElement>("header.app-header")!;
const adminLinks = () => within(within(sideMenu()).getByRole("region", { name: common.navigation.groups.admin })).getAllByRole("link").map((link) => link.textContent);
const builtInTabs = ["general", "pull", "nodes", "access", "integrations", "data", "diagnostics"].map((key) => locale.settings.page.tabs[key]);

async function openAccountMenu(user: ReturnType<typeof userEvent.setup>) {
  await user.click(screen.getByRole("button", { name: common.header.account.replace("{{name}}", "ops") }));
  return screen.findByRole("menu");
}
const entryNames = (menu: HTMLElement) => within(menu).getAllByRole("menuitem").map((item) => item.textContent);

test("an administrator gets the account entries between the built-in ones and signing out, and every admin-only entry", async () => {
  serve(asRole("admin"));
  const user = userEvent.setup();
  await mountApp("/dashboard");
  const menu = await openAccountMenu(user);
  await waitFor(() => assert.deepEqual(entryNames(menu), [common.header.changePassword, "Manage keys", "Profile", common.header.logout]));
  const separator = within(menu).getByRole("separator");
  assert.ok(within(menu).getByRole("menuitem", { name: "Profile" }).compareDocumentPosition(separator) & Node.DOCUMENT_POSITION_FOLLOWING, "the entries come before the separator");
  assert.equal(within(sideMenu()).queryByRole("link", { name: "Manage keys" }), null, "account entries are not in the side menu");
  assert.deepEqual(adminLinks(), [common.navigation.apiKeys, common.navigation.logs, common.navigation.settings, "Key audit"]);
  assert.ok(within(topBar()).getByRole("button", { name: "Key alerts" }));

  await user.click(within(menu).getByRole("menuitem", { name: "Manage keys" }));
  assert.ok(await screen.findByRole("heading", { name: "Keys" }));
  await waitFor(() => assert.equal(screen.queryByRole("menu"), null), "selecting an entry closes the menu");

  await user.click(within(sideMenu()).getByRole("link", { name: common.navigation.settings }));
  await waitFor(() => assert.deepEqual(screen.getAllByRole("tab").map((tab) => tab.textContent), [...builtInTabs, "Keys"]));
});

test("a member sees no admin-only entry and the admin-only page is not found", async () => {
  serve(asRole("member"));
  const user = userEvent.setup();
  await mountApp("/x/keys");
  assert.ok(await screen.findByText(common.notFound.title));
  assert.equal(screen.queryByRole("heading", { name: "Keys" }), null);
  const menu = await openAccountMenu(user);
  await waitFor(() => assert.deepEqual(entryNames(menu), [common.header.changePassword, "Profile", common.header.logout]));
  assert.deepEqual(adminLinks(), [common.navigation.apiKeys, common.navigation.logs, common.navigation.settings]);
  assert.equal(within(topBar()).queryByRole("button", { name: "Key alerts" }), null);
  cleanup();
  await mountApp("/settings#keys");
  await waitFor(() => assert.deepEqual(screen.getAllByRole("tab").map((tab) => tab.textContent), builtInTabs));
  assert.equal(screen.queryByText("keys settings"), null);
});

test("when the session's user cannot be read, admin-only entries stay hidden", async () => {
  serve(() => Response.json({ unexpected: true }));
  const user = userEvent.setup();
  await mountApp("/x/keys");
  assert.ok(await screen.findByText(common.notFound.title));
  await user.click(screen.getByRole("button", { name: common.header.account.replace("{{name}}", common.header.administrator) }));
  const menu = await screen.findByRole("menu");
  await waitFor(() => assert.deepEqual(entryNames(menu), [common.header.changePassword, "Profile", common.header.logout]));
  assert.equal(within(topBar()).queryByRole("button", { name: "Key alerts" }), null);
});

test("an account entry whose feature is hidden or locked is left out", async () => {
  for (const access of ["hidden", "locked"]) {
    resetConsoleManifestForTest();
    serve(asRole("admin"), access);
    const user = userEvent.setup();
    await mountApp("/dashboard");
    const menu = await openAccountMenu(user);
    await waitFor(() => assert.deepEqual(entryNames(menu), [common.header.changePassword, "Manage keys", common.header.logout]), access);
    cleanup();
  }
});

// UserMenu alone, as the shell renders it: under the session's user and a router.
function Where() {
  return <span data-testid="where">{useLocation().pathname}</span>;
}
function renderMenu(role: string) {
  render(
    <I18nextProvider i18n={i18n}>
      <SessionUserContext.Provider value={{ id: 1, username: "ops", role }}>
        <MemoryRouter initialEntries={["/dashboard"]}>
          <UserMenu onLogout={() => {}} />
          <Routes><Route path="*" element={<Where />} /></Routes>
        </MemoryRouter>
      </SessionUserContext.Provider>
    </I18nextProvider>,
  );
}

test("the account entry is reached with the arrow keys and opened with Enter", async () => {
  serve(asRole("admin"));
  renderMenu("admin");
  const user = userEvent.setup();
  screen.getByRole("button", { name: common.header.account.replace("{{name}}", "ops") }).focus();
  await user.keyboard("{Enter}");
  const menu = await screen.findByRole("menu");
  await waitFor(() => assert.deepEqual(entryNames(menu), [common.header.changePassword, "Manage keys", "Profile", common.header.logout]));
  await waitFor(() => assert.equal(document.activeElement?.textContent, common.header.changePassword));
  await user.keyboard("{ArrowDown}");
  await waitFor(() => assert.equal(document.activeElement?.textContent, "Manage keys"));
  await user.keyboard("{Enter}");
  await waitFor(() => assert.equal(screen.getByTestId("where").textContent, "/x/keys"));
});

test("with no extension the account menu is the built-in one", async () => {
  registry.setConsoleExtensionsForTest(undefined);
  serve(asRole("admin"));
  renderMenu("admin");
  const user = userEvent.setup();
  const menu = await openAccountMenu(user);
  assert.deepEqual(entryNames(menu), [common.header.changePassword, common.header.logout]);
});
