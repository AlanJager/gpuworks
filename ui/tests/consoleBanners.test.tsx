import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";

const { i18n, locale } = await createI18n(["common", "consoleBanners"]);
const zh = await createI18n(["common", "consoleBanners"], "zh");
const { cleanup, render, screen, waitFor, act } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { MemoryRouter, Route, Routes, useLocation } = await import("react-router-dom");
const manifestModule = await import("../src/consoleManifest.ts");
const ConsoleBanners = (await import("../src/app/ConsoleBanners.tsx")).default;
const { resetConsoleManifestForTest } = await import("../src/extensions/featureAccess.ts");
const { resetBannerAckForTest, bannerAckStorageKey, useBannerAck } = await import("../src/extensions/bannerAck.ts");
const { SessionUserContext } = await import("../src/components/sessionUser.ts");
type ConsoleManifest = import("../src/consoleManifest.ts").ConsoleManifest;

const c = locale.common as Record<string, any>;
const ack = (locale.consoleBanners as Record<string, string>).acknowledge;
const original = manifestModule.consoleManifestApi.manifest;
afterEach(() => {
  cleanup(); manifestModule.consoleManifestApi.manifest = original; resetConsoleManifestForTest();
  window.localStorage.clear(); resetBannerAckForTest();
});

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

// userID null renders with no known user.
function renderBanners(withI18n = i18n, path = "/dashboard", userID: number | null = 7) {
  const user = userEvent.setup();
  render(
    <I18nextProvider i18n={withI18n}>
      <SessionUserContext.Provider value={userID === null ? undefined : { id: userID, username: "ops", role: "admin" }}>
        <MemoryRouter initialEntries={[path]}>
          <ConsoleBanners />
          <Routes><Route path="*" element={<Where />} /></Routes>
        </MemoryRouter>
      </SessionUserContext.Provider>
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

const reminder = { code: "NO_SUCH_CODE", message: "Renew soon" };
const shownTexts = () => [...(region()?.querySelectorAll("[data-slot=alert]") ?? [])].map((note) => note.textContent ?? "");

test("only a banner with an ack_key offers to be acknowledged", async () => {
  stub(manifest([
    { id: "soon", severity: "info", message: reminder, link: "/x/renew", ack_key: "soon:30" },
    { id: "held", severity: "danger", message: { code: "NO_SUCH_CODE", message: "Nodes held" }, link: "/x/renew" },
  ]));
  renderBanners();
  const box = await screen.findByRole("region", { name: c.consoleBanners.region });
  const notes = box.querySelectorAll("[data-slot=alert]");
  assert.equal(notes.length, 2);
  assert.equal(screen.getAllByRole("button", { name: ack }).length, 1);
  assert.ok(notes[0].querySelector("button"), "the banner with a key has the control");
  assert.equal(notes[1].querySelector("button"), null, "a banner without a key cannot be acknowledged");
  // The details link stays beside the control.
  assert.equal(screen.getAllByRole("link", { name: c.consoleBanners.details }).length, 2);
});

test("acknowledging a banner hides it for this user, keeps the others, and survives a reload", async () => {
  stub(manifest([
    { id: "soon", severity: "info", message: reminder, ack_key: "soon:30" },
    { id: "plain", severity: "warning", message: { code: "NO_SUCH_CODE", message: "Other matter" } },
  ]));
  const user = renderBanners();
  await user.click(await screen.findByRole("button", { name: ack }));
  await waitFor(() => assert.deepEqual(shownTexts().map((t) => t.includes("Other matter")), [true]));
  assert.equal(window.localStorage.getItem(bannerAckStorageKey(7, "soon:30")), "1");
  cleanup();
  renderBanners();
  await waitFor(() => assert.equal(shownTexts().length, 1));
  assert.equal(screen.queryByText("Renew soon"), null);
  cleanup();
  // Another user on the same browser has not acknowledged it.
  renderBanners(i18n, "/dashboard", 8);
  await waitFor(() => assert.equal(shownTexts().length, 2));
});

test("a banner raised again under a new key shows again", async () => {
  window.localStorage.setItem(bannerAckStorageKey(7, "soon:30"), "1");
  stub(manifest([{ id: "soon", severity: "info", message: reminder, ack_key: "soon:7" }]));
  renderBanners();
  assert.ok(await screen.findByText("Renew soon"));
  assert.ok(screen.getByRole("button", { name: ack }));
});

test("an acknowledgement made elsewhere, in this tab or another, hides the banner at once", async () => {
  stub(manifest([{ id: "soon", severity: "info", message: reminder, ack_key: "soon:30" }]));
  function Elsewhere() {
    const ack = useBannerAck();
    return <button type="button" onClick={() => ack.acknowledge("soon:30")}>elsewhere</button>;
  }
  const user = userEvent.setup();
  render(
    <I18nextProvider i18n={i18n}>
      <SessionUserContext.Provider value={{ id: 7, username: "ops", role: "admin" }}>
        <MemoryRouter initialEntries={["/dashboard"]}><ConsoleBanners /><Elsewhere /></MemoryRouter>
      </SessionUserContext.Provider>
    </I18nextProvider>,
  );
  assert.ok(await screen.findByText("Renew soon"));
  await user.click(screen.getByRole("button", { name: "elsewhere" }));
  await waitFor(() => assert.equal(region(), null));
  cleanup();
  window.localStorage.clear();
  renderBanners();
  assert.ok(await screen.findByText("Renew soon"));
  // Another tab stores the acknowledgement; this one hears of it.
  await act(async () => {
    window.localStorage.setItem(bannerAckStorageKey(7, "soon:30"), "1");
    window.dispatchEvent(new window.StorageEvent("storage", { key: bannerAckStorageKey(7, "soon:30"), newValue: "1" }));
  });
  await waitFor(() => assert.equal(region(), null));
});

test("a banner is acknowledged with the keyboard, and in Chinese", async () => {
  stub(manifest([{ id: "soon", severity: "info", message: reminder, ack_key: "soon:30" }]));
  const zack = (zh.locale.consoleBanners as Record<string, string>).acknowledge;
  const user = renderBanners(zh.i18n);
  const button = await screen.findByRole("button", { name: zack });
  assert.equal(zack, "已知晓");
  await user.tab();
  assert.equal(document.activeElement, button);
  await user.keyboard("{Enter}");
  await waitFor(() => assert.equal(region(), null));
});

// The shell after a sign-in: the session's user is unknown until /auth/me
// answers (SessionBoundary), then known.
function SignedIn({ user }: { user?: { id: number; username: string; role: string } }) {
  return (
    <I18nextProvider i18n={i18n}>
      <SessionUserContext.Provider value={user}>
        <MemoryRouter initialEntries={["/dashboard"]}><ConsoleBanners /></MemoryRouter>
      </SessionUserContext.Provider>
    </I18nextProvider>
  );
}

test("until the signed-in user is known a banner with a key waits, so an acknowledged one never flashes", async () => {
  window.localStorage.setItem(bannerAckStorageKey(7, "done:30"), "1");
  stub(manifest([
    { id: "done", severity: "info", message: { code: "NO_SUCH_CODE", message: "Already acknowledged" }, ack_key: "done:30" },
    { id: "soon", severity: "info", message: reminder, ack_key: "soon:30" },
    { id: "held", severity: "danger", message: { code: "NO_SUCH_CODE", message: "Nodes held" } },
  ]));
  const { rerender } = render(<SignedIn />);
  // A banner without a key is shown at once; none offers to be acknowledged.
  assert.ok(await screen.findByText("Nodes held"));
  assert.equal(screen.queryByText("Already acknowledged"), null);
  assert.equal(screen.queryByText("Renew soon"), null);
  assert.equal(screen.queryByRole("button", { name: ack }), null);
  rerender(<SignedIn user={{ id: 7, username: "ops", role: "admin" }} />);
  assert.ok(await screen.findByText("Renew soon"));
  assert.equal(screen.queryByText("Already acknowledged"), null, "the acknowledged banner never showed");
  assert.equal(screen.getAllByRole("button", { name: ack }).length, 1);
});

test("an acknowledgement made before the user is known records nothing", async () => {
  stub(manifest([{ id: "soon", severity: "info", message: reminder, ack_key: "soon:30" }]));
  let acknowledge!: (key: string) => void;
  function Probe() {
    const a = useBannerAck();
    acknowledge = a.acknowledge;
    return <span data-testid="ready">{String(a.ready)}</span>;
  }
  const { rerender } = render(<SessionUserContext.Provider value={undefined}><Probe /></SessionUserContext.Provider>);
  assert.equal(screen.getByTestId("ready").textContent, "false");
  act(() => acknowledge("soon:30"));
  const keys = Array.from({ length: window.localStorage.length }, (_, i) => window.localStorage.key(i) ?? "");
  assert.deepEqual(keys.filter((key) => key.startsWith("gpuworks.banner-ack.")), []);
  rerender(<SessionUserContext.Provider value={{ id: 7, username: "ops", role: "admin" }}><Probe /></SessionUserContext.Provider>);
  assert.equal(screen.getByTestId("ready").textContent, "true");
  cleanup();
  // Once the user is known the banner is there to be acknowledged.
  renderBanners();
  assert.ok(await screen.findByRole("button", { name: ack }));
});
