import assert from "node:assert/strict";
import fs from "node:fs";
import { afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });

const nodesLocale = JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/nodes.json", import.meta.url), "utf8"));
const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: { en: { nodes: nodesLocale } },
  defaultNS: "nodes",
  interpolation: { escapeValue: false },
});

const { cleanup, render, screen } = await import("@testing-library/react");
const { kvDiskKernelShortfall, parseKernelRelease } = await import("../src/kernelSupport.ts");
const KVDiskKernelWarning = (await import("../src/components/KVDiskKernelWarning.tsx")).default;

afterEach(() => cleanup());

// The floors come from the same bytes /api/v1/engines serves, so the test moves
// with the server fact instead of restating "5.14".
const engines = JSON.parse(fs.readFileSync(new URL("./fixtures/engines.json", import.meta.url), "utf8")) as { name: string; kv_disk_min_kernel?: string }[];
const publishedFloor = engines.find((engine) => engine.kv_disk_min_kernel)?.kv_disk_min_kernel;

const warningText = (required: string, current: string) =>
  (nodesLocale.detail.kvDiskKernelTooOld as string).replace("{{required}}", required).replace("{{current}}", current);

test("the engines fixture states exactly one KV disk kernel floor", () => {
  assert.equal(engines.filter((engine) => engine.kv_disk_min_kernel).length, 1);
  assert.equal(publishedFloor, "5.14");
  assert.equal(engines.find((engine) => engine.name === "vllm")?.kv_disk_min_kernel, "5.14");
});

test("kernel strings are read as major.minor across the distributions nodes report", () => {
  assert.deepEqual(parseKernelRelease("4.18.0-553.69.1.13.gc220c6303994.el8.x86_64"), { major: 4, minor: 18 });
  assert.deepEqual(parseKernelRelease("5.15.0-113-generic"), { major: 5, minor: 15 });
  assert.deepEqual(parseKernelRelease("5.10.0-307.0.0.210.ky10.aarch64"), { major: 5, minor: 10 });
  assert.deepEqual(parseKernelRelease("6.8"), { major: 6, minor: 8 });
  assert.deepEqual(parseKernelRelease("  5.14.0-284.el9  "), { major: 5, minor: 14 });
});

test("anything that is not a major.minor release parses to nothing", () => {
  for (const garbage of ["", "   ", "unknown", "linux", "5", "5.", ".14", "v5.15", "Linux 5.15.0", "x5.15", "-", null, undefined]) {
    assert.equal(parseKernelRelease(garbage), null, `parsed ${JSON.stringify(garbage)}`);
  }
});

test("a shortfall is reported only for a kernel that really misses a published floor", () => {
  assert.deepEqual(kvDiskKernelShortfall("4.18.0-553.el8.x86_64", engines), { required: "5.14", current: "4.18.0-553.el8.x86_64" });
  assert.deepEqual(kvDiskKernelShortfall("5.10.0-307.0.0.210.ky10.aarch64", engines), { required: "5.14", current: "5.10.0-307.0.0.210.ky10.aarch64" });
  assert.equal(kvDiskKernelShortfall("5.14.0-284.el9.x86_64", engines), null, "the floor itself is supported");
  assert.equal(kvDiskKernelShortfall("5.15.0-113-generic", engines), null);
  assert.equal(kvDiskKernelShortfall("6.8.0-90-generic", engines), null);
  // No fact to compare against, or nothing to compare: stay silent rather than
  // accuse a node.
  assert.equal(kvDiskKernelShortfall("4.18.0-553.el8.x86_64", []), null);
  assert.equal(kvDiskKernelShortfall("4.18.0-553.el8.x86_64", [{ kv_disk_min_kernel: undefined }]), null);
  assert.equal(kvDiskKernelShortfall("4.18.0-553.el8.x86_64", null), null);
  assert.equal(kvDiskKernelShortfall("not-a-kernel", engines), null);
  assert.equal(kvDiskKernelShortfall("", engines), null);
});

test("the lowest unmet floor is the one reported when engines disagree", () => {
  const shortfall = kvDiskKernelShortfall("4.18.0-553.el8.x86_64", [
    { kv_disk_min_kernel: "6.1" },
    { kv_disk_min_kernel: "5.14" },
  ]);
  assert.deepEqual(shortfall, { required: "5.14", current: "4.18.0-553.el8.x86_64" });
});

function renderWarning(kernel: string | undefined) {
  return render(
    <I18nextProvider i18n={i18n}>
      <KVDiskKernelWarning kernel={kernel} engines={engines} />
    </I18nextProvider>,
  );
}

test("a 4.18 kernel is marked with a warning that names the required and current release", () => {
  const { container } = renderWarning("4.18.0-553.69.1.13.gc220c6303994.el8.x86_64");
  const expected = warningText("5.14", "4.18.0-553.69.1.13.gc220c6303994.el8.x86_64");
  assert.match(expected, /5\.14/);
  assert.match(expected, /4\.18\.0-553/);
  const icon = screen.getByLabelText(expected);
  assert.equal(icon.getAttribute("role"), "img");
  assert.ok(container.querySelector("svg"), "the warning renders an icon, not bare text");
});

test("a 5.15 kernel carries no warning", () => {
  const { container } = renderWarning("5.15.0-113-generic");
  assert.equal(container.textContent, "");
  assert.equal(container.querySelector("svg"), null);
  assert.equal(screen.queryByRole("img"), null);
});

test("a node that reported no kernel carries no warning", () => {
  const { container } = renderWarning("");
  assert.equal(container.querySelector("svg"), null);
  assert.equal(screen.queryByRole("img"), null);
  cleanup();
  const missing = renderWarning(undefined);
  assert.equal(missing.container.querySelector("svg"), null);
});
