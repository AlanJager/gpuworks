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
const { cleanup, render, screen, waitFor } = await import("@testing-library/react");
const apiModule = await import("../src/api.ts");
const settingsPage = await import("../src/pages/SettingsPage.tsx");
const SettingsPage = settingsPage.default;
const { externalHost } = settingsPage;

afterEach(() => cleanup());

const egress = (host: string) => (settingsLocale.ai.egressNotice as string).replace("{{host}}", host);

function stubAI(configs: { id: number; name: string; preset: string; base_url: string; model: string; is_default: boolean }[]) {
  const original = { settings: apiModule.api.settings, aiConfigs: apiModule.api.aiConfigs };
  apiModule.api.settings = async () => ({}) as never;
  apiModule.api.aiConfigs = async () => list(configs);
  return () => Object.assign(apiModule.api, original);
}

function renderPage() {
  return render(
    <I18nextProvider i18n={i18n}>
      <MemoryRouter initialEntries={["/settings#integrations"]}>
        <Routes>
          <Route path="*" element={<SettingsPage />} />
        </Routes>
      </MemoryRouter>
    </I18nextProvider>,
  );
}

const config = { id: 1, name: "primary", preset: "deepseek", model: "deepseek-v4-flash", is_default: true };

test("an external AI service shows where the assistant's reads are sent", async () => {
  const restore = stubAI([{ ...config, base_url: "https://api.deepseek.com" }]);
  try {
    renderPage();
    assert.ok(await screen.findByText(egress("api.deepseek.com")));
  } finally {
    restore();
  }
});

test("a model served by this platform shows no data-egress notice", async () => {
  const restore = stubAI([{ ...config, preset: "custom", base_url: "http://localhost/v1" }]);
  try {
    renderPage();
    await screen.findByText("primary");
    assert.equal(screen.queryByText(/sends what it reads/), null);
  } finally {
    restore();
  }
});

test("the notice follows the default service, and nothing shows without one", async () => {
  const restore = stubAI([
    { ...config, id: 1, name: "internal", preset: "custom", base_url: "http://localhost/v1", is_default: false },
    { ...config, id: 2, name: "external", base_url: "https://openrouter.ai/api/v1", is_default: true },
  ]);
  try {
    renderPage();
    assert.ok(await screen.findByText(egress("openrouter.ai")));
  } finally {
    restore();
  }
  cleanup();
  const restoreEmpty = stubAI([]);
  try {
    renderPage();
    await screen.findByText(settingsLocale.ai.empty as string);
    await waitFor(() => assert.equal(screen.queryByText(/sends what it reads/), null));
  } finally {
    restoreEmpty();
  }
});

test("externalHost tells the platform's own origin from anywhere else", () => {
  assert.equal(externalHost("http://localhost/v1", "http://localhost"), "");
  assert.equal(externalHost("/v1", "https://gpuworks.example:8800"), "");
  assert.equal(externalHost("https://gpuworks.example:8800/v1", "https://gpuworks.example:8800"), "");
  assert.equal(externalHost("https://api.deepseek.com", "https://gpuworks.example:8800"), "api.deepseek.com");
  assert.equal(externalHost("http://gpuworks.example:9000/v1", "http://gpuworks.example:8800"), "gpuworks.example:9000");
});
