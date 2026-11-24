import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";

const { i18n, locale } = await createI18n([]);
const zh = await createI18n([], "zh");
const { cleanup, render, screen, waitFor } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { MemoryRouter, Route, Routes, useLocation } = await import("react-router-dom");
const manifestModule = await import("../src/consoleManifest.ts");
const { default: PlatformGate, PlatformNotice, consolePath, opensPage } = await import("../src/app/PlatformGate.tsx");
const { resetConsoleManifestForTest } = await import("../src/extensions/featureAccess.ts");
type ConsolePlatform = import("../src/consoleManifest.ts").ConsolePlatform;

const c = locale.common as Record<string, any>;
const original = manifestModule.consoleManifestApi.manifest;
afterEach(() => { cleanup(); manifestModule.consoleManifestApi.manifest = original; resetConsoleManifestForTest(); });

// stub answers the manifest request with a server limited as platform says.
function stub(platform?: ConsolePlatform | Error) {
  manifestModule.consoleManifestApi.manifest = async () => {
    if (platform instanceof Error) throw platform;
    return { edition: "test", features: [], nav: [], banners: [], platform };
  };
}

function Where() {
  return <span data-testid="where">{useLocation().pathname}</span>;
}

function renderGate(path: string, withI18n = i18n) {
  const user = userEvent.setup();
  render(
    <I18nextProvider i18n={withI18n}>
      <MemoryRouter initialEntries={[path]}>
        <PlatformNotice />
        <PlatformGate>
          <Routes><Route path="*" element={<><h1>page</h1><Where /></>} /></Routes>
        </PlatformGate>
      </MemoryRouter>
    </I18nextProvider>,
  );
  return user;
}

const locked = (extra: Partial<ConsolePlatform> = {}): ConsolePlatform => ({ access: "locked", message: { code: "PLATFORM_LOCKED", message: "locked" }, ...extra });
const readOnly = (extra: Partial<ConsolePlatform> = {}): ConsolePlatform => ({ access: "read_only", message: { code: "PLATFORM_READ_ONLY", message: "read-only" }, ...extra });

test("a link is a console path only when it starts with one slash", () => {
  assert.equal(consolePath("/x/acme"), "/x/acme");
  assert.equal(consolePath("//example.com/x"), undefined);
  assert.equal(consolePath("https://example.com/x"), undefined);
  assert.equal(consolePath(undefined), undefined);
  assert.equal(opensPage("/x/acme?tab=1#top", "/x/acme/"), true);
  assert.equal(opensPage("/x/acme", "/x/acme/more"), false);
  assert.equal(opensPage("https://example.com/x/acme", "/x/acme"), false);
});

test("an unlimited server, and one whose manifest cannot be read, show the page and no notice", async () => {
  for (const answer of [undefined, new Error("offline")]) {
    stub(answer);
    renderGate("/nodes");
    assert.ok(await screen.findByRole("heading", { name: "page" }));
    assert.equal(screen.queryByRole("alert"), null);
    assert.equal(screen.queryByRole("status"), null);
    cleanup();
    resetConsoleManifestForTest();
  }
});

test("a locked server shows the page under an alert that opens the page lifting the limit", async () => {
  stub(locked({ link: "/x/acme", action: { code: "ACME_RENEW", message: "Renew" } }));
  const user = renderGate("/nodes");
  assert.ok(await screen.findByRole("heading", { name: "page" }), "a locked server serves reads, so the page shows");
  const alert = await screen.findByRole("alert");
  assert.ok(alert.textContent?.includes(c.apiError.PLATFORM_LOCKED));
  await user.click(screen.getByRole("link", { name: "Renew" }));
  await waitFor(() => assert.equal(screen.getByTestId("where").textContent, "/x/acme"));
  assert.equal(screen.queryByRole("alert"), null, "the page that lifts the limit states the matter itself");
});

test("a locked server whose link leaves the console shows the page and opens the link in a new tab", async () => {
  stub(locked({ link: "https://example.com/renew", action: { code: "ACME_RENEW", message: "Renew" } }));
  renderGate("/nodes");
  assert.ok(await screen.findByRole("heading", { name: "page" }));
  const alert = await screen.findByRole("alert");
  assert.ok(alert.textContent?.includes(c.apiError.PLATFORM_LOCKED));
  const link = screen.getByRole("link", { name: "Renew" });
  assert.equal(link.getAttribute("href"), "https://example.com/renew");
  assert.equal(link.getAttribute("target"), "_blank");
});

test("a locked server that names no page states the limit without a control", async () => {
  stub(locked());
  renderGate("/nodes");
  assert.ok(await screen.findByRole("heading", { name: "page" }));
  assert.ok((await screen.findByRole("alert")).textContent?.includes(c.apiError.PLATFORM_LOCKED));
  assert.equal(screen.queryByRole("link"), null);
});

test("a read-only server shows the page under a notice whose control is reached with the keyboard", async () => {
  stub(readOnly({ link: "/x/acme", action: { code: "ACME_RENEW", message: "Renew" } }));
  const user = renderGate("/nodes");
  assert.ok(await screen.findByRole("heading", { name: "page" }));
  const status = await screen.findByRole("status");
  assert.ok(status.textContent?.includes(c.apiError.PLATFORM_READ_ONLY));
  await user.tab();
  assert.equal(document.activeElement, screen.getByRole("link", { name: "Renew" }));
  await user.keyboard("{Enter}");
  await waitFor(() => assert.equal(screen.getByTestId("where").textContent, "/x/acme"));
  assert.equal(screen.queryByRole("status"), null, "the page that lifts the limit states the matter itself");
});

test("a limit without an action uses the console's own wording, in Chinese too", async () => {
  stub(readOnly({ link: "/x/acme" }));
  renderGate("/nodes", zh.i18n);
  const zhCommon = zh.locale.common as Record<string, any>;
  const status = await screen.findByRole("status");
  assert.ok(status.textContent?.includes(zhCommon.apiError.PLATFORM_READ_ONLY));
  assert.ok(screen.getByRole("link", { name: zhCommon.consoleBanners.details }));
});
