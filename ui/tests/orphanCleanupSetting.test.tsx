import assert from "node:assert/strict";
import fs from "node:fs";
import { afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import { list } from "./helpers/listPage.ts";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });

const settingsLocale = JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/settings.json", import.meta.url), "utf8"));
const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: { en: { settings: settingsLocale } },
  defaultNS: "settings",
  interpolation: { escapeValue: false },
});

const { MemoryRouter, Route, Routes } = await import("react-router-dom");
const { cleanup, render, screen, waitFor, within } = await import("@testing-library/react");
const userEventModule = await import("@testing-library/user-event");
const userEvent = userEventModule.default ?? userEventModule;
const apiModule = await import("../src/api.ts");
const SettingsPage = (await import("../src/pages/SettingsPage.tsx")).default;

afterEach(() => cleanup());

const text = (key: string) => settingsLocale.orphanCleanup[key] as string;

// antd only wires a Form.Item label into the accessible name when the field has
// a name, so locate the card first and query the switch within it.
async function findOrphanToggle() {
  const title = await screen.findByText(text("title"));
  const card = title.closest("div.ant-card") as HTMLElement;
  return within(card).getByRole("switch");
}

function stubAPI(settingsValue: Record<string, unknown> | Error) {
  const puts: { key: string; value: unknown }[] = [];
  const original = {
    settings: apiModule.api.settings,
    putSetting: apiModule.api.putSetting,
   
    aiConfigs: apiModule.api.aiConfigs,
  };
  apiModule.api.settings = async () => {
    if (settingsValue instanceof Error) throw settingsValue;
    return settingsValue as never;
  };
  apiModule.api.putSetting = async (key: string, value: unknown) => {
    puts.push({ key, value });
  };
  apiModule.api.aiConfigs = async () => list([]);
  return {
    puts,
    restore: () => {
      apiModule.api.settings = original.settings;
      apiModule.api.putSetting = original.putSetting;
      apiModule.api.aiConfigs = original.aiConfigs;
    },
  };
}

function renderPage() {
  return render(
    <I18nextProvider i18n={i18n}>
      <MemoryRouter initialEntries={["/settings#nodes"]}>
        <Routes>
          <Route path="*" element={<SettingsPage />} />
        </Routes>
      </MemoryRouter>
    </I18nextProvider>,
  );
}

test("shows the orphan cleanup toggle unchecked by default and saves the enabled value", async () => {
  const stub = stubAPI({ orphan_cleanup: false });
  try {
    renderPage();
    const toggle = await findOrphanToggle();
    assert.equal((toggle as HTMLInputElement).getAttribute("aria-checked"), "false");

    await userEvent.click(toggle);
    const card = toggle.closest("div.ant-card") as HTMLElement;
    await userEvent.click(within(card).getByRole("button", { name: settingsLocale.common.save as string }));
    await waitFor(() => assert.equal(stub.puts.length, 1));
    assert.equal(stub.puts[0].key, "orphan_cleanup");
    assert.equal(stub.puts[0].value, true);
  } finally {
    stub.restore();
  }
});

test("reflects an already-enabled setting and keeps it unchecked when loading fails", async () => {
  const stub = stubAPI({ orphan_cleanup: true });
  try {
    renderPage();
    const toggle = await findOrphanToggle();
    // The card renders before the settings promise resolves, so the checked
    // state arrives asynchronously; a slow runner asserting too early would
    // read the pre-load default.
    await waitFor(() => assert.equal((toggle as HTMLInputElement).getAttribute("aria-checked"), "true"));
  } finally {
    stub.restore();
  }

  cleanup();

  const failing = stubAPI(new Error("settings unavailable"));
  try {
    renderPage();
    const toggle = await findOrphanToggle();
    assert.equal((toggle as HTMLInputElement).getAttribute("aria-checked"), "false");
  } finally {
    failing.restore();
  }
});
