import assert from "node:assert/strict";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

const closeChannels = trackMessageChannels();

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "HTMLBodyElement", "HTMLInputElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });

const imagesLocale = JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/images.json", import.meta.url), "utf8"));
const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: { en: { images: imagesLocale } },
  defaultNS: "images",
  interpolation: { escapeValue: false },
});

const { cleanup, render, screen } = await import("@testing-library/react");
const ImageBuiltInConfig = (await import("../src/pages/images/ImageBuiltInConfig.tsx")).default;

afterEach(() => cleanup());
after(() => {
  closeChannels();
  dom.window.close();
});

const version = {
  id: 9, series_id: 1, name: "runner", image_ref: "example/runner:v1", source: "community",
  enabled: true, reference_count: 0, framework: "cuda", nodes: [{ node_uuid: "n1", state: "present", updated_at: "2026-10-08T00:00:00Z" }],
} as never;

function renderDrawer(inspection: unknown) {
  render(React.createElement(I18nextProvider, { i18n }, React.createElement(ImageBuiltInConfig, {
    version,
    inspection,
    nodeName: (uuid: string) => (uuid === "n1" ? "gpu-node-1" : uuid),
  } as never)));
}

// The catalog carries no launch configuration (ADR 0031): before a node is
// inspected the card only says how to read the image's own configuration, and
// nothing about it is editable.
test("an uninspected version explains how to read the image's own configuration", () => {
  renderDrawer(null);
  assert.ok(screen.getByText(imagesLocale.detail.builtInTitle));
  assert.ok(screen.getByText(imagesLocale.detail.builtInHint));
  assert.ok(screen.queryByText(/^ENTRYPOINT:/) === null, "nothing is shown before an inspection");
  assert.equal(screen.queryAllByRole("textbox").length, 0, "no launch default can be edited");
});

test("an inspection is shown as read, naming the node it came from", () => {
  renderDrawer({
    nodeUUID: "n1",
    result: { image_ref: "example/runner:v1", entrypoint: ["/opt/entry.sh"], cmd: [], env: { PATH: "/usr/bin", LANG: "C" }, volumes: ["/data"] },
  });
  assert.ok(screen.getByText(imagesLocale.detail.builtInFrom.replace("{{node}}", "gpu-node-1")));
  assert.ok(screen.getByText('["/opt/entry.sh"]'));
  assert.ok(screen.getByText(imagesLocale.detail.builtInNone), "an empty CMD reads as none rather than disappearing");
  assert.ok(screen.getByText("PATH=/usr/bin"));
  assert.ok(screen.getByText("LANG=C"));
  assert.ok(screen.getByText("/data"));
  assert.ok(screen.queryByText(imagesLocale.detail.builtInHint) === null, "the hint gives way to the inspection");
});
