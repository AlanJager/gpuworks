import assert from "node:assert/strict";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

const closeChannels = trackMessageChannels();
const dom = new JSDOM("<!doctype html><html><body></body></html>", {
  url: "http://localhost/",
});
for (const name of [
  "window",
  "document",
  "navigator",
  "HTMLElement",
  "HTMLInputElement",
  "HTMLTextAreaElement",
  "SVGElement",
  "Element",
  "Node",
  "MutationObserver",
  "ShadowRoot",
] as const) {
  Object.defineProperty(globalThis, name, {
    value: dom.window[name],
    configurable: true,
  });
}
Object.defineProperty(globalThis, "getComputedStyle", {
  value: (element: Element) => dom.window.getComputedStyle(element),
  configurable: true,
});
Object.defineProperty(dom.window, "matchMedia", {
  configurable: true,
  value: () => ({
    matches: false,
    addListener() {},
    removeListener() {},
    addEventListener() {},
    removeEventListener() {},
    dispatchEvent: () => false,
  }),
});
class ResizeObserverStub {
  observe() {}
  unobserve() {}
  disconnect() {}
}
Object.defineProperty(globalThis, "ResizeObserver", {
  value: ResizeObserverStub,
  configurable: true,
});

const deployLocale = JSON.parse(
  fs.readFileSync(
    new URL("../src/i18n/locales/en/deploy.json", import.meta.url),
    "utf8",
  ),
);
const imagesLocale = JSON.parse(
  fs.readFileSync(
    new URL("../src/i18n/locales/en/images.json", import.meta.url),
    "utf8",
  ),
);
const commonLocale = JSON.parse(
  fs.readFileSync(
    new URL("../src/i18n/locales/en/common.json", import.meta.url),
    "utf8",
  ),
);
const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: {
    en: { deploy: deployLocale, images: imagesLocale, common: commonLocale },
  },
  defaultNS: "common",
  interpolation: { escapeValue: false },
});

const { cleanup, render, screen, waitFor, within } =
  await import("@testing-library/react");
const { Form } = await import("antd");
type FormInstance = import("antd").FormInstance;
const DeployQuickForm = (
  await import("../src/pages/deploy/DeployQuickForm.tsx")
).default;
// Finish asynchronous imports before registering tests so node:test cannot
// run the suite's DOM teardown while later cases are still being declared.
const { buildPayload } = await import("../src/pages/deploy/deployHelpers.ts");
const { applyEvaluateFormValues } = await import("../src/pages/deploy/deployHelpers.ts");

afterEach(cleanup);
after(() => {
  closeChannels();
  dom.window.close();
});

const model = {
  id: 1,
  name: "org/model",
  source_id: "org/model",
  input_modalities: ["text"],
} as never;
const profile = {
  name: "vllm",
  sleep: { supported: false },
  quick_form_keys: [],
  param_specs: [],
} as never;

// The evaluate response the server returns for a two-node draft
// (internal/server/api evaluate fixtures): TP 2 spanning the nodes.
const multiNode = JSON.parse(
  fs.readFileSync(new URL("./fixtures/evaluate/multi_node.json", import.meta.url), "utf8"),
);
const w = deployLocale.workbench;

function renderQuick(initialValues: Record<string, unknown>, result: unknown) {
  function Host() {
    return (
      <Form initialValues={{ engine: "vllm", image_mode: "auto", command_mode: "engine", ...initialValues }}>
        <DeployQuickForm
          templates={[]}
          nodes={[]}
          selectedModel={model}
          profile={profile}
          engine="vllm"
          images={[]}
          result={result as never}
          preflight={null}
          preflighting={false}
          runPreflight={() => {}}
        />
      </Form>
    );
  }
  return render(
    <I18nextProvider i18n={i18n}>
      <Host />
    </I18nextProvider>,
  );
}

test("a multi-node draft sets its parallelism with the nodes, above image and weights", () => {
  const { container } = renderQuick({ node_selection_mode: "multi", nnodes: 2 }, multiNode);
  const labels = [...container.querySelectorAll(".deploy-workbench-label")].map((el) => el.textContent);
  const parallel = labels.indexOf(w.parallel.degrees);
  const image = labels.indexOf(w.acquisition.imageTitle);
  assert.ok(parallel >= 0 && image > parallel, `row order = ${labels.join(" | ")}`);
  assert.ok(screen.getByLabelText(w.resource.tp));
  assert.ok(screen.getByLabelText(w.resource.pp));
  // The split is the server's summary, quoted as is.
  assert.ok(screen.getByText("TP 2 × PP 1, 1 GPUs per node"));
  assert.ok(screen.getByText("TP 2 · PP 1"), "the parallel strategy card names both degrees");
});

test("a multi-node draft without an answer yet says where the default comes from", () => {
  renderQuick({ node_selection_mode: "multi", nnodes: 2, units_placement: "manual" }, null);
  assert.ok(screen.getByText(w.parallel.fromNodes));
});

test("a multi-node draft the platform places says it needs a TP until one is stated", () => {
  renderQuick({ node_selection_mode: "multi", nnodes: 2 }, null);
  assert.ok(screen.getByText(w.parallel.tpRequiredAuto));
  assert.equal(screen.queryByText(w.parallel.fromNodes), null);
  cleanup();
  renderQuick({ node_selection_mode: "multi", nnodes: 2, tp: 8 }, null);
  assert.equal(screen.queryByText(w.parallel.tpRequiredAuto), null);
});

test("a single-node draft keeps TP automatic in the resource row", () => {
  const { container } = renderQuick({ node_selection_mode: "manual", nnodes: 1 }, null);
  const labels = [...container.querySelectorAll(".deploy-workbench-label")].map((el) => el.textContent);
  assert.equal(labels.indexOf(w.parallel.degrees), -1, "no separate parallelism row on one node");
  assert.ok(screen.getByText(w.parallel.tpAutomatic));
  assert.ok(screen.getByLabelText(w.resource.pp));
});

test("the single-node summary reports the scheduled GPUs", () => {
  renderQuick({ node_selection_mode: "manual", nnodes: 1 }, { ...multiNode, parallelism: { tp: 4, pp: 1, nodes: 1, gpus_per_node: 4 } });
  assert.ok(screen.getByText("4 GPUs in all"));
});

test("TP and PP travel in the placement, and the server's defaults land on the form", () => {
  const { payload } = buildPayload({ model_id: 1, engine: "vllm", tp: 2, pp: 2, nnodes: 2 }, profile);
  assert.deepEqual((payload as { placement?: unknown }).placement, { tp: 2, pp: 2 });
  const { payload: bare } = buildPayload({ model_id: 1, engine: "vllm", nnodes: 2 }, profile);
  assert.equal((bare as { placement?: unknown }).placement, undefined, "an untouched draft leaves the parallelism to the server");

  const values: Record<string, unknown> = { nnodes: 2 };
  applyEvaluateFormValues(
    { setFieldsValue: (next) => Object.assign(values, next), getFieldsValue: () => values },
    { form_values: { tp: 4, pp: 2 } } as never,
    [],
  );
  assert.equal(values.tp, 4);
  assert.equal(values.pp, 2);
});

test("the multi-node form shows the server's forecast for each named node", () => {
  renderQuick({ node_selection_mode: "multi", nnodes: 2 }, multiNode);
  const table = screen.getByRole("table", { name: w.nodeAcquisition.forecastNote });
  const row = (name: string) => within(table).getByText(name).closest("tr")!;
  // The captured fixture: neither node holds anything yet, so both pull the
  // image (their probe orders the copies) and download the weights.
  for (const name of ["fixture-node", "fixture-peer"]) {
    const text = row(name).textContent ?? "";
    assert.ok(/Pull from quay\.io \/ \S+/.test(text), `${name}: every registry the probe orders is named: ${text}`);
    assert.ok(text.includes("the node's probe picks the source"), `${name}: ${text}`);
    assert.ok(text.includes("Download from modelscope"), `${name}: ${text}`);
  }
});
