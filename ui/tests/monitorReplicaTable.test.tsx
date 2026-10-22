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
  resources: {
    en: {
      monitoring: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/monitoring.json", import.meta.url), "utf8")),
      common: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/common.json", import.meta.url), "utf8")),
    },
  },
  defaultNS: "monitoring",
  interpolation: { escapeValue: false },
});

const { MemoryRouter, Route, Routes, useLocation } = await import("react-router-dom");
const { cleanup, render, screen, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { default: ReplicaTable } = await import("../src/pages/monitor/ReplicaTable.tsx");
import type { MonitorReplica } from "../src/api";

afterEach(() => cleanup());

let seenPath = "";
function LocationProbe() {
  seenPath = useLocation().pathname;
  return null;
}

function renderTable(replicas: MonitorReplica[], deploymentId = 102) {
  seenPath = "";
  return render(
    <I18nextProvider i18n={i18n}>
      <MemoryRouter initialEntries={["/monitor/d/102"]}>
        <Routes>
          <Route path="/monitor/d/:id" element={<ReplicaTable replicas={replicas} deploymentId={deploymentId} />} />
          <Route path="/deployments" element={<LocationProbe />} />
        </Routes>
      </MemoryRouter>
    </I18nextProvider>,
  );
}

const fullyCollected: MonitorReplica = {
  instance_uuid: "inst-full-000-0000-0000-000000000001",
  state: "running",
  node_name: "gpu1",
  gpu_indexes: [0, 1],
  running: 38,
  waiting: 9,
  kv_usage_pct: 96,
  kv_hit_rate_pct: 40,
  prompt_tok_s: 1500,
  generation_tok_s: 900,
  gpus: [{ index: 0, util_pct: 91, mem_bw_pct: 70, temperature_c: 78, power_w: 310 }],
};

test("every collected field renders its value", () => {
  renderTable([fullyCollected]);
  const row = screen.getByText("inst-full-00").closest("tr")!;
  assert.ok(row.textContent?.includes("38"));
  assert.ok(row.textContent?.includes("9"));
  assert.ok(row.textContent?.includes("96%"));
  assert.ok(row.textContent?.includes("40%"));
  assert.ok(row.textContent?.includes("1500"));
  assert.ok(row.textContent?.includes("900"));
  assert.ok(row.textContent?.includes("GPU 0 · 91%"));
  assert.ok(row.textContent?.includes("GPU 0 · 78°C"));
  assert.ok(row.textContent?.includes("GPU 0 · 310W"));
});

test("a multi-GPU replica stacks one 'GPU n · value' line per GPU, with a per-GPU dash for an uncollected value", () => {
  const replica: MonitorReplica = { ...fullyCollected, gpu_indexes: [0, 1], gpus: [
    { index: 0, util_pct: 91, mem_bw_pct: 70, temperature_c: 78, power_w: 310 },
    { index: 1, util_pct: null, mem_bw_pct: 65, temperature_c: 75, power_w: 290 },
  ] };
  renderTable([replica]);
  const row = screen.getByText("inst-full-00").closest("tr")!;
  assert.ok(screen.getByText("GPU 0 · 91%"));
  assert.ok(screen.getByText("GPU 1 · 65%"));
  assert.ok(screen.getByText("GPU 1 · 290W"));
  // GPU 1's util is not collected: its own line keeps a dash while GPU 0's
  // value still renders on its line.
  const utilLines = Array.from(row.querySelectorAll("td")).flatMap((td) => Array.from(td.querySelectorAll("span"))).map((el) => el.textContent);
  assert.ok(utilLines.includes("GPU 1 · —"), utilLines.join(" | "));
  assert.equal(row.textContent?.includes("0:91%"), false);
});

test("running null, waiting collected: only running shows a dash", () => {
  const replica: MonitorReplica = { ...fullyCollected, running: null, waiting: 9 };
  renderTable([replica]);
  const row = screen.getByText("inst-full-00").closest("tr")!;
  assert.ok(row.textContent?.includes("— / 9"), row.textContent ?? "");
});

test("waiting null, running collected: only waiting shows a dash, not a fabricated 0", () => {
  const replica: MonitorReplica = { ...fullyCollected, running: 38, waiting: null };
  renderTable([replica]);
  const row = screen.getByText("inst-full-00").closest("tr")!;
  assert.ok(row.textContent?.includes("38 / —"), row.textContent ?? "");
  assert.equal(row.textContent?.includes("38 / 0"), false);
});

test("both running and waiting null render two independent dashes", () => {
  const replica: MonitorReplica = { ...fullyCollected, running: null, waiting: null };
  renderTable([replica]);
  const row = screen.getByText("inst-full-00").closest("tr")!;
  assert.ok(row.textContent?.includes("— / —"), row.textContent ?? "");
});

test("kv_usage_pct null renders a dash", () => {
  const replica: MonitorReplica = { ...fullyCollected, kv_usage_pct: null };
  renderTable([replica]);
  const row = screen.getByText("inst-full-00").closest("tr")!;
  assert.ok(within(row).getAllByText("—").length > 0);
});

test("kv_hit_rate_pct null renders a dash", () => {
  const replica: MonitorReplica = { ...fullyCollected, kv_hit_rate_pct: null };
  renderTable([replica]);
  const row = screen.getByText("inst-full-00").closest("tr")!;
  assert.ok(within(row).getAllByText("—").length > 0);
});

test("prompt_tok_s null renders a dash", () => {
  const replica: MonitorReplica = { ...fullyCollected, prompt_tok_s: null };
  renderTable([replica]);
  const row = screen.getByText("inst-full-00").closest("tr")!;
  assert.ok(within(row).getAllByText("—").length > 0);
});

test("generation_tok_s null renders a dash", () => {
  const replica: MonitorReplica = { ...fullyCollected, generation_tok_s: null };
  renderTable([replica]);
  const row = screen.getByText("inst-full-00").closest("tr")!;
  assert.ok(within(row).getAllByText("—").length > 0);
});

test("per-GPU util/memBw/temperature/power each independently null render a dash for that field only", () => {
  const replica: MonitorReplica = {
    ...fullyCollected,
    gpus: [{ index: 0, util_pct: null, mem_bw_pct: null, temperature_c: null, power_w: null }],
  };
  renderTable([replica]);
  const row = screen.getByText("inst-full-00").closest("tr")!;
  // All four GPU columns render "GPU 0 · —" (the GPU index is still known,
  // only its value is not collected).
  const gpuLines = Array.from(row.querySelectorAll("td span")).map((el) => el.textContent).filter((text) => text?.startsWith("GPU 0"));
  assert.equal(gpuLines.length, 4, gpuLines.join(" | "));
  assert.ok(gpuLines.every((text) => text === "GPU 0 · —"), gpuLines.join(" | "));
});

test("an empty gpus array renders a dash, not an error", () => {
  const replica: MonitorReplica = { ...fullyCollected, gpus: [] };
  renderTable([replica]);
  const row = screen.getByText("inst-full-00").closest("tr")!;
  assert.ok(within(row).getAllByText("—").length >= 4);
});

test("gpu_indexes null is guarded to an empty list instead of throwing", () => {
  const replica = { ...fullyCollected, gpu_indexes: null } as unknown as MonitorReplica;
  renderTable([replica]);
  const row = screen.getByText("inst-full-00").closest("tr")!;
  assert.ok(row.textContent?.includes("gpu1"));
  assert.ok(row.textContent?.includes("GPU —"));
});

test("clicking a row navigates to the deployments drawer with the instance uuid (mouse-only row affordance)", async () => {
  renderTable([fullyCollected], 102);
  const row = screen.getByText("inst-full-00").closest("tr") as HTMLElement;
  assert.ok(row.className.includes("gpu-clickable-row"));
  // No role/tabIndex override: the row stays a plain, non-interactive <tr> -
  // see the instance Link test below for the keyboard-reachable path.
  assert.equal(row.getAttribute("role"), null);
  await userEvent.setup().click(row);
  assert.equal(seenPath, "/deployments");
});

test("the instance cell is a real link that keyboard/AT users can tab to and activate", async () => {
  renderTable([fullyCollected], 102);
  const link = screen.getByRole("link", { name: "inst-full-00" });
  assert.ok(link.getAttribute("href")?.startsWith("/deployments?deployment=102&instance="));
  link.focus();
  await userEvent.setup().keyboard("{Enter}");
  assert.equal(seenPath, "/deployments");
});
