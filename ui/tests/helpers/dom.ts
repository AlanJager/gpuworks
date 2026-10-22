// Shared jsdom + i18n setup for component tests. Import it first (statically)
// and import React Testing Library and the components dynamically afterwards,
// so the globals exist before antd reads them.
import fs from "node:fs";
import { JSDOM } from "jsdom";
import { createInstance } from "i18next";
import { initReactI18next } from "react-i18next";

export const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
const names = [
  "window", "document", "navigator", "HTMLElement", "HTMLBodyElement", "HTMLInputElement", "HTMLTextAreaElement",
  "HTMLButtonElement", "HTMLSelectElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot",
] as const;
for (const name of names) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", {
  configurable: true,
  value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }),
});
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });

// React's legacy input-event polyfill reaches for IE's attachEvent when an input gains focus under jsdom.
Object.defineProperties(dom.window.HTMLElement.prototype, {
  attachEvent: { configurable: true, value() {} },
  detachEvent: { configurable: true, value() {} },
});
// rc-overflow creates a MessageChannel per update and never closes the ports, which would pin the node
// event loop; timer-based delivery leaves no live handle behind.
class ShimPort {
  onmessage: ((ev: { data: unknown }) => void) | null = null;
  postMessage(data: unknown) { setTimeout(() => this.onmessage?.({ data }), 0); }
}
class ShimMessageChannel { port1 = new ShimPort(); port2 = new ShimPort(); }
Object.defineProperty(globalThis, "MessageChannel", { value: ShimMessageChannel, configurable: true });

/** Loads the real English locale files of the given namespaces (common is always included). */
export async function createI18n(namespaces: string[], lng: "en" | "zh" = "en") {
  const all = [...new Set(["common", ...namespaces])];
  const resources: Record<string, Record<string, unknown>> = { [lng]: {} };
  for (const ns of all) {
    resources[lng][ns] = JSON.parse(fs.readFileSync(new URL(`../../src/i18n/locales/${lng}/${ns}.json`, import.meta.url), "utf8"));
  }
  const i18n = createInstance();
  await i18n.use(initReactI18next).init({
    lng, fallbackLng: lng, resources, ns: all, defaultNS: namespaces[0] ?? "common", interpolation: { escapeValue: false },
  });
  return { i18n, locale: resources[lng] as Record<string, Record<string, any>> };
}
