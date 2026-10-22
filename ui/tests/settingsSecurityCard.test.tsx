import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

const closeChannels = trackMessageChannels();
const { i18n, locale } = await createI18n(["settings"]);
const { cleanup, render, screen, waitFor } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const apiModule = await import("../src/api.ts");
const { APIError } = await import("../src/apiError.ts");
const { default: SecurityCard } = await import("../src/pages/settings/SecurityCard.tsx");
const s = locale.settings.security as Record<string, string>;

type Settings = import("../src/api.ts").SecuritySettings;
const original = { securitySettings: apiModule.api.securitySettings, updateSecuritySettings: apiModule.api.updateSecuritySettings, me: apiModule.api.me };

function stub({ role = "admin", load }: { role?: string; load: () => Promise<Settings> }) {
  const updates: Partial<Settings>[] = [];
  let saveError: Error | null = null;
  let loads = 0;
  apiModule.api.me = (async () => ({ id: 1, username: role, role })) as typeof apiModule.api.me;
  apiModule.api.securitySettings = async () => { loads++; return load(); };
  apiModule.api.updateSecuritySettings = async (body) => {
    updates.push(body);
    if (saveError) throw saveError;
    return { playground_preview_network: body.playground_preview_network ?? false };
  };
  return { updates, loads: () => loads, failSaveWith: (error: Error) => { saveError = error; } };
}

function renderCard() {
  const user = userEvent.setup();
  render(<I18nextProvider i18n={i18n}><SecurityCard /></I18nextProvider>);
  return user;
}
const toggle = () => screen.getByRole("switch", { name: s.previewNetwork });
const saveButton = () => screen.getByRole("button", { name: locale.settings.common.save });

afterEach(() => {
  cleanup();
  Object.assign(apiModule.api, original);
});
after(() => closeChannels());

test("the switch is off and busy until the policy loads, then shows it", async () => {
  let resolve!: (value: Settings) => void;
  stub({ load: () => new Promise<Settings>((r) => { resolve = r; }) });
  renderCard();
  assert.equal(toggle().getAttribute("aria-checked"), "false");
  assert.ok(toggle().classList.contains("ant-switch-loading"), "loading while the request is in flight");
  assert.equal((saveButton() as HTMLButtonElement).disabled, true, "nothing to save before the policy is known");
  resolve({ playground_preview_network: true });
  await waitFor(() => assert.equal(toggle().getAttribute("aria-checked"), "true"));
  assert.ok(!toggle().classList.contains("ant-switch-loading"));
  assert.ok(screen.getByText(s.previewNetworkHelp));
});

test("an admin turns network access on and saves it", async () => {
  const api = stub({ load: async () => ({ playground_preview_network: false }) });
  const user = renderCard();
  await waitFor(() => assert.equal((toggle() as HTMLButtonElement).disabled, false));
  await user.click(toggle());
  assert.equal(toggle().getAttribute("aria-checked"), "true");
  assert.equal(api.updates.length, 0, "toggling alone does not save");
  await user.click(saveButton());
  await waitFor(() => assert.deepEqual(api.updates, [{ playground_preview_network: true }]));
  assert.ok(await screen.findByText(s.saved));
});

test("the switch toggles from the keyboard", async () => {
  stub({ load: async () => ({ playground_preview_network: false }) });
  const user = renderCard();
  await waitFor(() => assert.equal((toggle() as HTMLButtonElement).disabled, false));
  toggle().focus();
  await user.keyboard(" ");
  assert.equal(toggle().getAttribute("aria-checked"), "true");
  await user.keyboard("{Enter}");
  assert.equal(toggle().getAttribute("aria-checked"), "false");
});

test("a refused save reports the server's reason", async () => {
  const api = stub({ load: async () => ({ playground_preview_network: false }) });
  api.failSaveWith(new APIError("ADMINISTRATOR_REQUIRED", undefined, "administrator role required", 403));
  const user = renderCard();
  await waitFor(() => assert.equal((toggle() as HTMLButtonElement).disabled, false));
  await user.click(toggle());
  await user.click(saveButton());
  await waitFor(() => assert.equal(api.updates.length, 1));
  assert.ok(await screen.findByText(/Failed to save/));
});

test("a failed load is reported and can be retried", async () => {
  let fail = true;
  const api = stub({ load: async () => {
    if (fail) throw new Error("settings offline");
    return { playground_preview_network: true };
  } });
  const user = renderCard();
  assert.ok(await screen.findByText(/settings offline/));
  assert.equal((toggle() as HTMLButtonElement).disabled, true);
  fail = false;
  await user.click(screen.getByRole("button", { name: locale.common.actions.retry }));
  await waitFor(() => assert.equal(toggle().getAttribute("aria-checked"), "true"));
  assert.equal(api.loads(), 2);
  assert.ok(screen.queryByText(/settings offline/) === null, "the error clears after a successful retry");
});

test("a non-admin sees the policy but cannot change it", async () => {
  stub({ role: "user", load: async () => ({ playground_preview_network: false }) });
  renderCard();
  assert.ok(await screen.findByText(s.adminOnly));
  await waitFor(() => assert.ok(!toggle().classList.contains("ant-switch-loading")));
  assert.equal((toggle() as HTMLButtonElement).disabled, true);
  assert.equal((saveButton() as HTMLButtonElement).disabled, true);
});

test("a #security deep link keeps the card in view while the page above it grows, until the user scrolls", async () => {
  stub({ load: async () => ({ playground_preview_network: false }) });
  const observers: { callback: () => void; disconnected: boolean }[] = [];
  class TrackingResizeObserver {
    entry: { callback: () => void; disconnected: boolean };
    constructor(callback: () => void) { this.entry = { callback, disconnected: false }; observers.push(this.entry); }
    observe() {}
    unobserve() {}
    disconnect() { this.entry.disconnected = true; }
  }
  const originalObserver = globalThis.ResizeObserver;
  const proto = window.HTMLElement.prototype as unknown as { scrollIntoView?: () => void };
  const originalScroll = proto.scrollIntoView;
  let jumps = 0;
  Object.defineProperty(globalThis, "ResizeObserver", { value: TrackingResizeObserver, configurable: true });
  proto.scrollIntoView = function () { if ((this as HTMLElement).id === "security") jumps++; };
  window.location.hash = "#security";
  try {
    renderCard();
    assert.equal(jumps, 1, "jumps once on mount");
    const observer = observers.at(-1)!;
    observer.callback();
    observer.callback();
    assert.equal(jumps, 3, "follows each layout change");
    window.dispatchEvent(new window.WheelEvent("wheel", { deltaY: -40 }));
    assert.equal(observer.disconnected, true, "the user's own scroll ends it");
  } finally {
    window.location.hash = "";
    Object.defineProperty(globalThis, "ResizeObserver", { value: originalObserver, configurable: true });
    proto.scrollIntoView = originalScroll;
  }
});

test("without the fragment the card does not move the page", () => {
  stub({ load: async () => ({ playground_preview_network: false }) });
  const proto = window.HTMLElement.prototype as unknown as { scrollIntoView?: () => void };
  const originalScroll = proto.scrollIntoView;
  let jumps = 0;
  proto.scrollIntoView = () => { jumps++; };
  try {
    renderCard();
    assert.equal(jumps, 0);
  } finally {
    proto.scrollIntoView = originalScroll;
  }
});
