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

const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: { en: { monitoring: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/monitoring.json", import.meta.url), "utf8")) } },
  defaultNS: "monitoring",
  interpolation: { escapeValue: false },
});

const { MemoryRouter, Routes, Route, useLocation } = await import("react-router-dom");
const { cleanup, render, screen } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { TimeRangeProvider, default: TimeRangeControl, useTimeRange } = await import("../src/pages/monitor/TimeRangeContext.tsx");

afterEach(() => cleanup());

let seenLocation = "";
function LocationProbe() {
  const location = useLocation();
  seenLocation = `${location.pathname}${location.search}`;
  return null;
}

function ValueProbe() {
  const { range, follow } = useTimeRange();
  return <div data-testid="value">{range}:{follow ? "on" : "off"}</div>;
}

function renderApp(initialEntry: string) {
  return render(
    <I18nextProvider i18n={i18n}>
      <MemoryRouter initialEntries={[initialEntry]}>
        <Routes>
          <Route
            path="/monitor"
            element={
              <TimeRangeProvider>
                <LocationProbe />
                <ValueProbe />
                <TimeRangeControl />
              </TimeRangeProvider>
            }
          />
        </Routes>
      </MemoryRouter>
    </I18nextProvider>,
  );
}

test("defaults to 1h range with follow on when the URL carries neither", () => {
  renderApp("/monitor");
  assert.equal(screen.getByTestId("value").textContent, "1h:on");
});

test("reads range and follow back out of the URL", () => {
  renderApp("/monitor?range=6h&follow=0");
  assert.equal(screen.getByTestId("value").textContent, "6h:off");
});

test("an unknown range value in the URL falls back to the default", () => {
  renderApp("/monitor?range=3d");
  assert.equal(screen.getByTestId("value").textContent, "1h:on");
});

test("picking a range writes it to the URL search params", async () => {
  renderApp("/monitor?range=1h");
  await userEvent.setup().click(screen.getByText("24h"));
  assert.equal(screen.getByTestId("value").textContent, "24h:on");
  assert.equal(seenLocation, "/monitor?range=24h");
});

test("toggling follow writes follow=0 to the URL and back to follow=1", async () => {
  renderApp("/monitor?range=1h");
  const toggle = screen.getByRole("switch", { name: "Follow latest" });
  await userEvent.setup().click(toggle);
  assert.equal(screen.getByTestId("value").textContent, "1h:off");
  assert.equal(seenLocation, "/monitor?range=1h&follow=0");
  await userEvent.setup().click(toggle);
  assert.equal(screen.getByTestId("value").textContent, "1h:on");
  assert.equal(seenLocation, "/monitor?range=1h&follow=1");
});
