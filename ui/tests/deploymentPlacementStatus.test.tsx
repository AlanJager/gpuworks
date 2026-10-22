import assert from "node:assert/strict";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";

// A deployment whose replicas wait for a node says so in its drawer: how
// many, the controller's reason with the refusal it names rendered, the
// closest nodes; and the live candidate table, one server page at a time,
// filtered by status, with its loading, empty and error states.

const closeChannels = trackMessageChannels();
const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot", "File", "Blob", "HTMLBodyElement"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });

const read = (ns: string) => JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/en/${ns}.json`, import.meta.url), "utf8"));
const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: { en: { deployments: read("deployments"), deploy: read("deploy"), common: read("common") } },
  defaultNS: "deployments",
  interpolation: { escapeValue: false },
});
const locale = i18n.getResourceBundle("en", "deployments").detail.placement;

const { cleanup, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { api } = await import("../src/api.ts");
const { APIError } = await import("../src/apiError.ts");
const { PlacementStatusAlert } = await import("../src/pages/DeploymentPlacementStatus.tsx");

type Page = { status?: string; offset?: number; limit?: number };
const apiAny = api as unknown as Record<string, unknown>;
const original = apiAny.deploymentCandidates;
afterEach(() => { cleanup(); apiAny.deploymentCandidates = original; });
after(() => { closeChannels(); dom.window.close(); });

function deployment(overrides: Record<string, unknown> = {}) {
  return {
    id: 7, name: "waiting", model_name: "m", model_id: 1, served_name: "m", engine: "vllm", node_uuid: "n-1",
    desired_state: "active", runtime_state: "running", sleepable: false, replicas: 3, template_deviated: false,
    created_at: "2026-10-03T00:00:00Z", node_requirements: { "gpuworks.ai/vendor": ["nvidia"], pool: ["a", "b"] },
    placement_strategy: "spread",
    placement_status: {
      unscheduled: 2, at: "2026-10-03T01:02:03Z",
      reason: { code: "pool_no_fit", params: {
        candidates: 40, tried: 8, node: "gpu-7", first: "no_free_gpu", first_reason: { code: "no_free_gpu" },
        closest: [{ node: "u-7", name: "gpu-7", reason: { code: "no_free_gpu" } }, { node: "u-9", name: "gpu-9", reason: { code: "no_free_gpu" } }],
      } },
    },
    ...overrides,
  } as never;
}

function candidates(items: Array<Record<string, unknown>>, total = items.length) {
  return {
    nodes: 1000, counts: { room: 3, busy: 895, unmet: 90, cordoned: 5, offline: 7 }, unmet_by_key: { pool: 90 },
    placement_strategy: "spread", total, offset: 0, limit: 50, items,
  };
}

function node(index: number, status = "busy") {
  return { uuid: `u-${index}`, name: `gpu-${index}`, status, rank: index, replicas: 0, free_gpus: 0, gpus: 8, model: "absent" };
}

function show(dep = deployment()) {
  return render(<I18nextProvider i18n={i18n}><PlacementStatusAlert dep={dep} /></I18nextProvider>);
}

test("nothing is shown while every replica has a node", () => {
  show(deployment({ placement_status: undefined }));
  assert.equal(screen.queryByText(/waiting for a node/), null);
});

test("the waiting count, the controller's reason with its refusal rendered, and the closest nodes", () => {
  show();
  assert.ok(screen.getByText(i18n.t("detail.placement.waiting", { count: 2 })));
  const noFreeGPU = i18n.t("deploy:schedule.no_free_gpu");
  // {{first}} is the rendered refusal, never the raw code.
  const reason = screen.getByText(new RegExp("None of the 8 best of 40 candidate nodes"));
  assert.ok(reason.textContent!.includes(noFreeGPU), reason.textContent!);
  assert.ok(!reason.textContent!.includes("no_free_gpu"), reason.textContent!);
  assert.ok(screen.getByText(locale.closest));
  assert.ok(screen.getByText("gpu-7") && screen.getByText("gpu-9"));
});

test("the candidate table pages and filters on the server, from the keyboard", async () => {
  const asked: Page[] = [];
  apiAny.deploymentCandidates = async (id: number, page: Page) => {
    assert.equal(id, 7);
    asked.push(page);
    if (page.status === "unmet") return candidates([{ ...node(1, "unmet"), rank: undefined, detail: "node gpu-1 does not satisfy pool ∈ {a, b}" }], 90);
    return candidates(Array.from({ length: 50 }, (_, i) => node((page.offset ?? 0) + i + 1)), 1000);
  };
  const user = userEvent.setup({ document: dom.window.document });
  show();
  await user.click(screen.getByRole("button", { name: locale.view }));
  const modal = (await screen.findByText(locale.candidates)).closest(".ant-modal") as HTMLElement;
  await within(modal).findByText("gpu-1");
  assert.deepEqual(asked[0], { status: undefined, offset: 0, limit: 50 });
  // The counts cover the whole cluster, not the page.
  assert.ok(within(modal).getByText(i18n.t("detail.placement.summary", { nodes: 1000, room: 3, busy: 895, unmet: 90, cordoned: 5, offline: 7 })));
  assert.ok(within(modal).getByText("gpuworks.ai/vendor=nvidia") && within(modal).getByText("pool=a | b"));

  await user.click(within(modal).getByTitle("2"));
  await within(modal).findByText("gpu-51");
  assert.deepEqual(asked.at(-1), { status: undefined, offset: 50, limit: 50 });

  // The status filter is a radio group: reach it with the keyboard.
  const unmet = within(modal).getByRole("radio", { name: new RegExp(locale.status.unmet) });
  unmet.focus();
  await user.keyboard(" ");
  await within(modal).findByText("node gpu-1 does not satisfy pool ∈ {a, b}");
  assert.deepEqual(asked.at(-1), { status: "unmet", offset: 0, limit: 50 });
});

test("an empty page and a failed load say so", async () => {
  apiAny.deploymentCandidates = async () => candidates([]);
  const user = userEvent.setup({ document: dom.window.document });
  show();
  await user.click(screen.getByRole("button", { name: locale.view }));
  const modal = (await screen.findByText(locale.candidates)).closest(".ant-modal") as HTMLElement;
  await waitFor(() => assert.ok(modal.querySelector(".ant-empty")));
  cleanup();

  apiAny.deploymentCandidates = async () => { throw new APIError("FIELD_INVALID", { field: "limit", detail: "1..200" }, "limit must be between 1 and 200", 400); };
  show();
  await user.click(screen.getByRole("button", { name: locale.view }));
  await screen.findByText(new RegExp(i18n.t("detail.placement.loadFailed", { detail: "" }).trim()));
});
