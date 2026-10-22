import assert from "node:assert/strict";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

// ADR 0020: the form is a projection of the parameter dictionary. A control
// that shows a value submits that value, and the set of submittable keys comes
// from the dictionary the server sends, never from a list in this code base.

const closeChannels = trackMessageChannels();
const dom = new JSDOM("<!doctype html><html><body></body></html>", {
  url: "http://localhost/",
});
for (const name of [
  "window",
  "document",
  "navigator",
  "HTMLElement",
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
    dispatchEvent() {
      return false;
    },
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

const locale = JSON.parse(
  fs.readFileSync(
    new URL("../src/i18n/locales/en/deploy.json", import.meta.url),
    "utf8",
  ),
);
const i18n = createInstance();
await i18n
  .use(initReactI18next)
  .init({
    lng: "en",
    fallbackLng: "en",
    resources: { en: { deploy: locale } },
    defaultNS: "deploy",
    interpolation: { escapeValue: false },
  });
const { cleanup, fireEvent, render, screen } =
  await import("@testing-library/react");
const { Form } = await import("antd");
type FormInstance = import("antd").FormInstance;
const DeployParamField = (
  await import("../src/pages/deploy/DeployParamField.tsx")
).default;
const { buildPayload } = await import("../src/pages/deploy/deployHelpers.ts");

const utilizationSpec = {
  key: "gpu_memory_utilization",
  flag: "--gpu-memory-utilization",
  type: "number",
  control: "slider",
  group: "performance",
  default: 0.92,
  min: 0.7,
  max: 0.98,
  step: 0.01,
  label: { en: "GPU memory utilization", zh: "显存利用率" },
} as never;
const profile = {
  name: "vllm",
  sleep: { supported: true },
  param_specs: [utilizationSpec],
} as never;
afterEach(cleanup);
after(() => {
  closeChannels();
  dom.window.close();
});

test("the utilization slider shows the server default without changing the draft, then submits an edit", async () => {
  let form: FormInstance | undefined;
  function Host() {
    const [instance] = Form.useForm();
    form = instance;
    return (
      <Form form={instance}>
        <DeployParamField spec={utilizationSpec} />
      </Form>
    );
  }
  render(
    <I18nextProvider i18n={i18n}>
      <Host />
    </I18nextProvider>,
  );
  assert.ok(
    screen.getByText("0.92"),
    "the slider renders the dictionary default",
  );
  const submitted = buildPayload(form!.getFieldsValue(true), profile)
    .quick as Record<string, unknown>;
  assert.equal(
    form!.getFieldValue("gpu_memory_utilization"),
    undefined,
    "rendering does not change the draft",
  );
  // The default the slider shows is the server's to state: an untouched draft
  // sends nothing and the evaluate response carries 0.92 in form_values.
  assert.equal(submitted.gpu_memory_utilization, undefined);
  const { applyEvaluateFormValues } =
    await import("../src/pages/deploy/deployHelpers.ts");
  applyEvaluateFormValues(
    form!,
    { form_values: { gpu_memory_utilization: 0.92 } } as never,
    [],
  );
  assert.equal(
    form!.getFieldValue("gpu_memory_utilization"),
    0.92,
    "the display instruction lands on the form",
  );
  assert.equal(
    (
      buildPayload(form!.getFieldsValue(true), profile).quick as Record<
        string,
        unknown
      >
    ).gpu_memory_utilization,
    0.92,
  );
  const slider = screen.getByRole("slider");
  fireEvent.keyDown(slider, { key: "ArrowRight", keyCode: 39, which: 39 });
  assert.equal(
    (
      buildPayload(form!.getFieldsValue(true), profile).quick as Record<
        string,
        unknown
      >
    ).gpu_memory_utilization,
    0.93,
  );
});

test("a key the server adds to the dictionary is submittable without a front-end change", () => {
  const newSpec = {
    key: "brand_new_knob",
    flag: "--brand-new-knob",
    type: "integer",
    control: "number",
    label: { en: "Brand new knob", zh: "新旋钮" },
  } as never;
  const extended = {
    name: "vllm",
    sleep: { supported: true },
    param_specs: [utilizationSpec, newSpec],
  } as never;
  const submitted = buildPayload(
    { engine: "vllm", brand_new_knob: 3, gpu_memory_utilization: 0.9 },
    extended,
  ).quick as Record<string, unknown>;
  assert.deepEqual(submitted, {
    brand_new_knob: 3,
    gpu_memory_utilization: 0.9,
  });
});
