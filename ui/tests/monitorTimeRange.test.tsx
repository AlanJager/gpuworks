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
const { TimeRangeProvider, default: TimeRangeControl, PickedRangeNote, useTimeRange, outsidePickable } = await import("../src/pages/monitor/TimeRangeContext.tsx");
const { rangeParams, rangeBounds, pickedBounds, customRange } = await import("../src/monitorRange.ts");
const dayjs = (await import("dayjs")).default;

afterEach(() => cleanup());

let seenLocation = "";
function LocationProbe() {
  const location = useLocation();
  seenLocation = `${location.pathname}${location.search}`;
  return null;
}

function ValueProbe() {
  const { range, follow, setPicked } = useTimeRange();
  return (
    <>
      <div data-testid="value">{range}:{follow ? "on" : "off"}</div>
      <button type="button" onClick={() => setPicked(1_800_000_000_000, 1_800_003_600_000)}>pick</button>
    </>
  );
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
                <PickedRangeNote />
              </TimeRangeProvider>
            }
          />
        </Routes>
      </MemoryRouter>
    </I18nextProvider>,
  );
}

test("defaults to 1h range with follow off when the URL carries neither", () => {
  renderApp("/monitor");
  assert.equal(screen.getByTestId("value").textContent, "1h:off");
});

test("reads range and follow back out of the URL", () => {
  renderApp("/monitor?range=6h&follow=0");
  assert.equal(screen.getByTestId("value").textContent, "6h:off");
});

test("an unknown range value in the URL falls back to the default", () => {
  renderApp("/monitor?range=3d");
  assert.equal(screen.getByTestId("value").textContent, "1h:off");
});

test("picking a range writes it to the URL search params", async () => {
  renderApp("/monitor?range=1h");
  await userEvent.setup().click(screen.getByText("24h"));
  assert.equal(screen.getByTestId("value").textContent, "24h:off");
  assert.equal(seenLocation, "/monitor?range=24h");
});

test("toggling follow writes follow=0 to the URL and back to follow=1", async () => {
  renderApp("/monitor?range=1h&follow=1");
  const toggle = screen.getByRole("switch", { name: "Follow latest" });
  await userEvent.setup().click(toggle);
  assert.equal(screen.getByTestId("value").textContent, "1h:off");
  assert.equal(seenLocation, "/monitor?range=1h&follow=0");
  await userEvent.setup().click(toggle);
  assert.equal(screen.getByTestId("value").textContent, "1h:on");
  assert.equal(seenLocation, "/monitor?range=1h&follow=1");
});

test("a picked range is read from from/to, never follows, and says what stays current", () => {
  renderApp("/monitor?from=1800000000000&to=1800003600000&follow=1");
  assert.equal(screen.getByTestId("value").textContent, "1800000000000~1800003600000:off");
  const toggle = screen.getByRole("switch", { name: "Follow latest" }) as HTMLButtonElement;
  assert.equal(toggle.disabled, true);
  assert.ok(screen.getByText(/services, replicas and health are current/));
  assert.ok(screen.getByText("Custom"));
});

test("picking writes from/to and drops the preset; a preset drops from/to", async () => {
  renderApp("/monitor?range=6h");
  const user = userEvent.setup();
  await user.click(screen.getByText("pick"));
  assert.equal(seenLocation, "/monitor?from=1800000000000&to=1800003600000");
  await user.click(screen.getByText("24h"));
  assert.equal(seenLocation, "/monitor?range=24h");
  assert.equal(screen.getByTestId("value").textContent, "24h:off");
});

test("a malformed from/to falls back to the preset", () => {
  renderApp("/monitor?range=6h&from=9&to=3");
  assert.equal(screen.getByTestId("value").textContent, "6h:off");
  assert.equal(screen.queryByText(/services, replicas and health are current/), null, "a preset needs no note");
});

test("the picker refuses the future and days before raw samples are kept", () => {
  const now = dayjs("2026-10-22T12:00:00Z");
  assert.equal(outsidePickable(now.add(1, "day"), now), true);
  assert.equal(outsidePickable(now.subtract(31, "day"), now), true);
  assert.equal(outsidePickable(now.subtract(29, "day"), now), false);
  assert.equal(outsidePickable(now, now), false);
});

test("a range becomes the API's parameters and its window", () => {
  assert.deepEqual(rangeParams("6h"), { range: "6h" });
  assert.deepEqual(rangeParams(customRange(1000, 5000)), { from: "1000", to: "5000" });
  assert.deepEqual(rangeBounds("1h", 10_000_000), { from: 10_000_000 - 3_600_000, to: 10_000_000 });
  assert.deepEqual(rangeBounds(customRange(1000, 5000)), { from: 1000, to: 5000 });
  assert.equal(pickedBounds("5000~1000"), null);
  assert.equal(pickedBounds("1h"), null);
});

for (const value of ["1", "true"]) {
  test(`preserves explicit follow=${value} links`, () => {
    renderApp(`/monitor?follow=${value}`);
    assert.equal(screen.getByRole("switch", { name: "Follow latest" }).getAttribute("aria-checked"), "true");
  });
}

test("follow can be enabled and disabled with the keyboard", async () => {
  renderApp("/monitor");
  const toggle = screen.getByRole("switch", { name: "Follow latest" });
  toggle.focus();
  await userEvent.setup().keyboard(" ");
  assert.equal(toggle.getAttribute("aria-checked"), "true");
  assert.equal(seenLocation, "/monitor?follow=1");
  await userEvent.setup().keyboard(" ");
  assert.equal(toggle.getAttribute("aria-checked"), "false");
  assert.equal(seenLocation, "/monitor?follow=0");
});
