import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";

const { i18n, locale } = await createI18n([]);
const zh = await createI18n([], "zh");
const { cleanup, render, screen, waitFor, act } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { MemoryRouter, Route, Routes, useLocation } = await import("react-router-dom");
const manifestModule = await import("../src/consoleManifest.ts");
const ConsoleBanners = (await import("../src/app/ConsoleBanners.tsx")).default;
const { resetConsoleManifestForTest } = await import("../src/extensions/featureAccess.ts");
type ConsoleManifest = import("../src/consoleManifest.ts").ConsoleManifest;

const c = locale.common as Record<string, any>;
const original = manifestModule.consoleManifestApi.manifest;
afterEach(() => { cleanup(); manifestModule.consoleManifestApi.manifest = original; resetConsoleManifestForTest(); });

function manifest(banners: ConsoleManifest["banners"]): ConsoleManifest {
  return { edition: "community", features: [], nav: [], banners };
}

// stub answers the manifest request with answer (or rejects with it), and
// counts the requests.
function stub(answer: ConsoleManifest | Error | Promise<ConsoleManifest>) {
  let calls = 0;
  manifestModule.consoleManifestApi.manifest = async () => {
    calls++;
    if (answer instanceof Error) throw answer;
    return answer;
  };
  return () => calls;
}

function Where() {
  return <span data-testid="where">{useLocation().pathname}</span>;
}

function renderBanners(withI18n = i18n, path = "/dashboard") {
  const user = userEvent.setup();
  render(
    <I18nextProvider i18n={withI18n}>
      <MemoryRouter initialEntries={[path]}>
        <ConsoleBanners />
        <Routes><Route path="*" element={<Where />} /></Routes>
      </MemoryRouter>
    </I18nextProvider>,
  );
  return user;
}

const region = () => screen.queryByRole("region", { name: c.consoleBanners.region });

test("nothing shows while the manifest loads", async () => {
  let resolve!: (m: ConsoleManifest) => void;
  const calls = stub(new Promise<ConsoleManifest>((r) => { resolve = r; }));
  renderBanners();
  await waitFor(() => assert.equal(calls(), 1));
  assert.equal(region(), null);
  await act(async () => { resolve(manifest([{ id: "b", severity: "info", message: { code: "FEATURE_UNAVAILABLE", params: { feature: "audit" }, message: "x" } }])); });
  assert.ok(await screen.findByRole("region", { name: c.consoleBanners.region }));
});

test("a manifest without banners shows nothing", async () => {
  const calls = stub(manifest([]));
  renderBanners();
  await waitFor(() => assert.equal(calls(), 1));
  await act(async () => {});
  assert.equal(region(), null);
  assert.equal(screen.queryByRole("status"), null);
});

test("a manifest that cannot be read shows nothing and leaves the page alone", async () => {
  const calls = stub(new Error("HTTP 502"));
  renderBanners();
  await waitFor(() => assert.equal(calls(), 1));
  await act(async () => {});
  assert.equal(region(), null);
  assert.equal(screen.getByTestId("where").textContent, "/dashboard");
});

test("banners render in the server's order with their code's text and severity", async () => {
  stub(manifest([
    { id: "ro", severity: "warning", message: { code: "FEATURE_READ_ONLY", params: { feature: "audit" }, message: "audit is read-only" }, link: "/settings" },
    { id: "gone", severity: "danger", message: { code: "FEATURE_UNAVAILABLE", params: { feature: "sso" }, message: "sso is not available" }, link: "https://example.invalid/sso" },
    { id: "unknown", severity: "info", message: { code: "NO_SUCH_CODE_FROM_AN_EXTENSION", message: "Plain English fallback" } },
  ]));
  renderBanners();
  const box = await screen.findByRole("region", { name: c.consoleBanners.region });
  const notes = box.querySelectorAll("[data-slot=alert]");
  assert.equal(notes.length, 3);
  assert.equal(notes[0].getAttribute("data-status"), "warning");
  assert.equal(notes[1].getAttribute("data-status"), "danger");
  assert.equal(notes[1].getAttribute("role"), "alert");
  assert.equal(notes[2].getAttribute("data-status"), "info");
  assert.ok(notes[0].textContent?.includes(c.apiError.FEATURE_READ_ONLY.replace("{{feature}}", "audit")));
  assert.ok(notes[1].textContent?.includes(c.apiError.FEATURE_UNAVAILABLE.replace("{{feature}}", "sso")));
  // A code the console has no text for falls back to the server's English.
  assert.ok(notes[2].textContent?.includes("Plain English fallback"));
  // An outside link opens in a new tab; a banner without a link has none.
  const outside = screen.getAllByRole("link", { name: c.consoleBanners.details })[1];
  assert.equal(outside.getAttribute("href"), "https://example.invalid/sso");
  assert.equal(outside.getAttribute("target"), "_blank");
  assert.ok((outside.getAttribute("rel") ?? "").includes("noopener"));
  assert.equal(notes[2].querySelector("a"), null);
});

test("a banner's console link is reached and followed with the keyboard", async () => {
  stub(manifest([{ id: "ro", severity: "warning", message: { code: "FEATURE_READ_ONLY", params: { feature: "audit" }, message: "x" }, link: "/settings" }]));
  const user = renderBanners();
  const link = await screen.findByRole("link", { name: c.consoleBanners.details });
  await user.tab();
  assert.equal(document.activeElement, link);
  await user.keyboard("{Enter}");
  await waitFor(() => assert.equal(screen.getByTestId("where").textContent, "/settings"));
  // The page it opened states the matter itself, so the banner steps aside there.
  await waitFor(() => assert.equal(region(), null));
});

test("a banner is left out on the page its link opens and shown everywhere else", async () => {
  const readOnly = { code: "FEATURE_READ_ONLY", params: { feature: "audit" }, message: "x" };
  stub(manifest([
    { id: "here", severity: "warning", message: readOnly, link: "/x?tab=second#top" },
    { id: "elsewhere", severity: "info", message: { code: "NO_SUCH_CODE", message: "Elsewhere" }, link: "/x/other" },
    { id: "outside", severity: "info", message: { code: "NO_SUCH_CODE", message: "Outside" }, link: "https://example.invalid/x" },
    { id: "plain", severity: "info", message: { code: "NO_SUCH_CODE", message: "No link" } },
  ]));
  const shown = () => [...(region()?.querySelectorAll("[data-slot=alert]") ?? [])].map((note) => note.textContent ?? "");
  renderBanners(i18n, "/dashboard");
  await waitFor(() => assert.equal(shown().length, 4));
  cleanup();
  for (const path of ["/x", "/x/"]) {
    renderBanners(i18n, path);
    await waitFor(() => assert.equal(shown().length, 3, path));
    const texts = shown();
    assert.equal(texts.some((text) => text.includes(c.apiError.FEATURE_READ_ONLY.replace("{{feature}}", "audit"))), false, `${path}: the banner linking here is left out`);
    assert.ok(texts.some((text) => text.includes("Elsewhere")), "a link to another page, even one below it, still shows");
    assert.ok(texts.some((text) => text.includes("Outside")) && texts.some((text) => text.includes("No link")));
    cleanup();
  }
});

test("banners render in Chinese", async () => {
  stub(manifest([{ id: "ro", severity: "warning", message: { code: "FEATURE_READ_ONLY", params: { feature: "audit" }, message: "x" }, link: "/settings" }]));
  renderBanners(zh.i18n);
  const zc = zh.locale.common as Record<string, any>;
  const box = await screen.findByRole("region", { name: zc.consoleBanners.region });
  assert.ok(box.textContent?.includes(zc.apiError.FEATURE_READ_ONLY.replace("{{feature}}", "audit")));
  assert.ok(screen.getByRole("link", { name: zc.consoleBanners.details }));
});
