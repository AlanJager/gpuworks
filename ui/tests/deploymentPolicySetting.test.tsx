import assert from "node:assert/strict";
import fs from "node:fs";
import { afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "HTMLInputElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });

const settingsLocale = JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/settings.json", import.meta.url), "utf8"));
const commonLocale = JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/common.json", import.meta.url), "utf8"));
const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: { en: { settings: settingsLocale, common: commonLocale } },
  defaultNS: "settings",
  interpolation: { escapeValue: false },
});

const { cleanup, render, screen, waitFor } = await import("@testing-library/react");
const userEventModule = await import("@testing-library/user-event");
const userEvent = userEventModule.default ?? userEventModule;
const apiModule = await import("../src/api.ts");
const DeploymentPolicyCard = (await import("../src/pages/settings/DeploymentPolicyCard.tsx")).default;

afterEach(() => cleanup());

const text = (key: string) => settingsLocale.deployment[key] as string;

function stubAPI(settingsValue: Record<string, unknown> | Error, putError?: Error) {
  const puts: { key: string; value: unknown }[] = [];
  const original = { settings: apiModule.api.settings, putSetting: apiModule.api.putSetting };
  apiModule.api.settings = async () => {
    if (settingsValue instanceof Error) throw settingsValue;
    return settingsValue as never;
  };
  apiModule.api.putSetting = async (key: string, value: unknown) => {
    puts.push({ key, value });
    if (putError) throw putError;
  };
  return {
    puts,
    restore: () => {
      apiModule.api.settings = original.settings;
      apiModule.api.putSetting = original.putSetting;
    },
  };
}

function renderCard() {
  render(
    <I18nextProvider i18n={i18n}>
      <DeploymentPolicyCard />
    </I18nextProvider>,
  );
  return {
    input: () => screen.getByRole("spinbutton", { name: text("startTimeout") }) as HTMLInputElement,
    save: () => screen.getByRole("button", { name: settingsLocale.common.save as string }) as HTMLButtonElement,
  };
}

test("shows the stored start timeout and saves only that field", async () => {
  const stub = stubAPI({ deployment: { pending_timeout_sec: 600, start_timeout_sec: 1800, event_retention_days: 180 } });
  try {
    const card = renderCard();
    assert.ok(screen.getByText(text("title")));
    await waitFor(() => assert.equal(card.input().value, "1800"));

    const user = userEvent.setup({ document: dom.window.document });
    await user.clear(card.input());
    await user.type(card.input(), "3600");
    await user.click(card.save());
    await waitFor(() => assert.equal(stub.puts.length, 1));
    // The server overlays the body on the stored policy, so the card names
    // only the field it edits and cannot reset the pending timeout.
    assert.deepEqual(stub.puts[0], { key: "deployment", value: { start_timeout_sec: 3600 } });
  } finally {
    stub.restore();
  }
});

test("an out-of-range value cannot be saved", async () => {
  const stub = stubAPI({ deployment: { start_timeout_sec: 1800 } });
  try {
    const card = renderCard();
    await waitFor(() => assert.equal(card.input().value, "1800"));
    const user = userEvent.setup({ document: dom.window.document });
    await user.clear(card.input());
    assert.equal(card.save().disabled, true, "an empty field is not a value to save");
    await user.type(card.input(), "30");
    await waitFor(() => assert.equal(card.save().disabled, true));
    assert.equal(stub.puts.length, 0);
  } finally {
    stub.restore();
  }
});

test("a failed load says so and leaves nothing to save", async () => {
  const stub = stubAPI(new Error("settings unavailable"));
  try {
    const card = renderCard();
    await waitFor(() => assert.ok(screen.getByText(/settings unavailable/)));
    assert.equal(card.input().value, "");
    assert.equal(card.save().disabled, true);
  } finally {
    stub.restore();
  }
});

test("a rejected save reports the server's reason", async () => {
  const stub = stubAPI({ deployment: { start_timeout_sec: 1800 } }, new Error("deployment.start_timeout_sec must be between 60 and 86400"));
  try {
    const card = renderCard();
    await waitFor(() => assert.equal(card.input().value, "1800"));
    const user = userEvent.setup({ document: dom.window.document });
    await user.click(card.save());
    await waitFor(() => assert.ok(screen.getByText(/must be between 60 and 86400/)));
  } finally {
    stub.restore();
  }
});
