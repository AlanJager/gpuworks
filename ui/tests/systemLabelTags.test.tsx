import assert from "node:assert/strict";
import fs from "node:fs";
import { afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import type { SystemLabel } from "../src/api";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });

const locale = (name: string) =>
  JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/zh/${name}.json`, import.meta.url), "utf8"));

const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "zh",
  fallbackLng: "zh",
  resources: { zh: { nodes: locale("nodes"), common: locale("common"), networks: locale("networks") } },
  defaultNS: "nodes",
  interpolation: { escapeValue: false },
});

const { cleanup, render, screen } = await import("@testing-library/react");
const { SystemLabelTags } = await import("../src/components/SystemLabelTags.tsx");

afterEach(cleanup);

function show(labels?: SystemLabel[], limit?: number) {
  render(
    React.createElement(
      I18nextProvider,
      { i18n },
      React.createElement(SystemLabelTags, { labels, limit }),
    ),
  );
}

// A generated label reads as a named fact. The vendor carries a translation
// code, so the Chinese name is shown rather than the stored value.
test("a generated label is shown by name, translated where the server says so", () => {
  show([
    { key: "gpuworks.ai/vendor", value: "ascend", display: "Ascend", display_code: "common:vendor.ascend" },
    { key: "gpuworks.ai/gpu-model", value: "910C", display: "910C" },
  ]);
  assert.ok(screen.getByText("厂商: 昇腾"));
  assert.ok(screen.getByText("GPU 型号: 910C"));
});

// A network label matches on the id and is shown by the network's name, so
// renaming a network changes the page and not the selector.
test("a network label shows the network name, not its id", () => {
  show([{ key: "gpuworks.ai/compute-network", value: "12", display: "参数网-1" }]);
  assert.ok(screen.getByText("参数网络: 参数网-1"));
  assert.equal(screen.queryByText("参数网络: 12"), null);
});

// The server sends a code for values the client may not know; an untranslated
// one still shows the text the server sent rather than the raw code.
test("an untranslated code falls back to the text the server sent", () => {
  show([{ key: "gpuworks.ai/vendor", value: "nvidia", display: "NVIDIA", display_code: "common:vendor.nvidia" }]);
  assert.ok(screen.getByText("厂商: NVIDIA"));
});

// A key this build has no name for is shown as the key's own short form, so
// a label added by a newer server is still legible.
test("an unknown key is shown by its short form", () => {
  show([{ key: "gpuworks.ai/rack", value: "7", display: "7" }]);
  assert.ok(screen.getByText("rack: 7"));
});

test("a node with no generated labels renders a placeholder", () => {
  show([]);
  assert.ok(screen.getByText("—"));
  cleanup();
  show(undefined);
  assert.ok(screen.getByText("—"));
});

// The list column shows a few and says how many it left out, so a node with
// many cards does not push the row open.
test("beyond the limit the rest are counted", () => {
  show(
    [
      { key: "gpuworks.ai/gpu-model", value: "a", display: "a" },
      { key: "gpuworks.ai/gpu-model", value: "b", display: "b" },
      { key: "gpuworks.ai/gpu-model", value: "c", display: "c" },
    ],
    2,
  );
  assert.ok(screen.getByText("GPU 型号: a"));
  assert.ok(screen.getByText("GPU 型号: b"));
  assert.equal(screen.queryByText("GPU 型号: c"), null);
  assert.ok(screen.getByText("另有 1 项"));
});
