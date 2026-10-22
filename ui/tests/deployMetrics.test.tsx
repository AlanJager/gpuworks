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

const locale = JSON.parse(
  fs.readFileSync(
    new URL("../src/i18n/locales/en/deploy.json", import.meta.url),
    "utf8",
  ),
);
const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: { en: { deploy: locale } },
  defaultNS: "deploy",
  interpolation: { escapeValue: false },
});
const { act, cleanup, fireEvent, render, screen, waitFor } =
  await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { Form } = await import("antd");
type FormInstance = import("antd").FormInstance;
const DeployMetricsField = (
  await import("../src/pages/deploy/DeployMetricsField.tsx")
).default;
const DeployQuickForm = (
  await import("../src/pages/deploy/DeployQuickForm.tsx")
).default;
const DeployPerformanceStep = (
  await import("../src/pages/deploy/DeployPerformanceStep.tsx")
).default;
const DeployServedNameField = (
  await import("../src/pages/deploy/DeployServedNameField.tsx")
).default;
const useDeploymentIdentity = (
  await import("../src/pages/deploy/useDeploymentIdentity.ts")
).default;
const {
  applyEvaluateFormValues,
  buildPayload,
  EngineDecidesContext,
  effectiveEngineDecidesParamKeys,
} = await import("../src/pages/deploy/deployHelpers.ts");
const DeployParamField = (
  await import("../src/pages/deploy/DeployParamField.tsx")
).default;
const metricSpec = {
  key: "enable_metrics",
  flag: "--enable-metrics",
  type: "boolean",
  control: "switch",
  group: "performance",
  default: false,
  label: { en: "Inference Metrics", zh: "推理监控指标" },
  help: { en: "Expose engine inference metrics for GPUWorks monitoring" },
} as never;
const parserSpec = {
  key: "tool_call_parser",
  flag: "--tool-call-parser",
  type: "string",
  control: "combobox",
  label: { en: "Tool Call Parser", zh: "工具调用解析器" },
} as never;
const prefixCachingSpec = {
  key: "enable_prefix_caching",
  flag: "--enable-prefix-caching",
  type: "boolean",
  control: "switch",
  default: true,
  label: { en: "Prefix Caching", zh: "前缀缓存" },
} as never;
const quickProfile = {
  name: "sglang",
  quick_form_keys: [
    "enable_metrics",
    "tool_call_parser",
    "enable_prefix_caching",
  ],
  sleep: { supported: false },
  param_specs: [metricSpec, parserSpec, prefixCachingSpec],
} as never;
const profile = { name: "sglang", param_specs: [metricSpec] } as never;
afterEach(cleanup);
after(() => {
  closeChannels();
  dom.window.close();
});

test("the catalog's metrics flag arrives as a template parameter, not as argument text", () => {
  // ADR 0020: the spec's --enable-metrics is decoded by the server, which
  // reports it as a template parameter and keeps the rest of the spec's tokens
  // in the selection snapshot. The form applies the parameter instead of
  // parsing the catalog arguments itself, and the argument box stays the
  // user's own.
  let form: FormInstance | undefined;
  function Host() {
    const [instance] = Form.useForm();
    form = instance;
    return (
      <Form
        form={instance}
        initialValues={{
          template_id: 1,
          template_spec_source: "upstream",
          template_spec_name: "general",
        }}
      >
        <DeployMetricsField spec={metricSpec} />
      </Form>
    );
  }
  render(
    <I18nextProvider i18n={i18n}>
      <Host />
    </I18nextProvider>,
  );
  act(() => {
    // The template's parameter contribution arrives as the server's display
    // instruction; the residual tokens stay in the selection snapshot.
    applyEvaluateFormValues(
      form!,
      { form_values: { enable_metrics: true } } as never,
      [],
    );
  });
  assert.equal(
    screen
      .getByRole("switch", { name: "Inference Metrics" })
      .getAttribute("aria-checked"),
    "true",
  );
  assert.equal(
    form!.getFieldValue("extra_args"),
    undefined,
    "the template's residual tokens are not user arguments",
  );
  assert.deepEqual(buildPayload(form!.getFieldsValue(true), profile).quick, {
    enable_metrics: true,
  });
  assert.deepEqual(
    buildPayload({ engine: "sglang", enable_metrics: false }, profile).quick,
    { enable_metrics: false },
  );
});

test("metrics starts checked from the catalog and can be turned off then on with the keyboard", async () => {
  let payload: Record<string, unknown> = {};
  render(
    <I18nextProvider i18n={i18n}>
      <Form
        initialValues={{ enable_metrics: true }}
        onValuesChange={(_, values) => {
          payload = buildPayload(values, profile).payload;
        }}
      >
        <DeployMetricsField spec={metricSpec} />
      </Form>
    </I18nextProvider>,
  );
  const toggle = screen.getByRole("switch", { name: "Inference Metrics" });
  assert.equal(toggle.getAttribute("aria-checked"), "true");
  assert.equal(screen.queryByText(locale.metrics.disabledWarning), null);
  const user = userEvent.setup({ document: dom.window.document });
  await user.click(toggle);
  assert.equal(toggle.getAttribute("aria-checked"), "false");
  assert.deepEqual(payload.quick_params, { enable_metrics: false });
  assert.ok(screen.getByText(locale.metrics.disabledWarning));
  toggle.focus();
  await user.keyboard("[Space]");
  await waitFor(() =>
    assert.equal(toggle.getAttribute("aria-checked"), "true"),
  );
  assert.equal(screen.queryByText(locale.metrics.disabledWarning), null);
});

test("a catalog without the metrics flag starts unchecked with an explanation", () => {
  render(
    <I18nextProvider i18n={i18n}>
      <Form>
        <DeployMetricsField spec={metricSpec} />
      </Form>
    </I18nextProvider>,
  );
  assert.equal(
    screen
      .getByRole("switch", { name: "Inference Metrics" })
      .getAttribute("aria-checked"),
    "false",
  );
  assert.ok(screen.getByText(locale.metrics.disabledWarning));
});

// rc-select opens on the selector's mousedown and portals its dropdown, so
// the option is found by its dropdown item container.
async function pickOption(
  user: ReturnType<typeof userEvent.setup>,
  select: HTMLElement,
  label: string,
) {
  fireEvent.mouseDown(
    (select.closest(".ant-select")?.querySelector(".ant-select-selector") ??
      select) as HTMLElement,
  );
  const option = screen
    .getAllByText(label)
    .find((element) => element.closest(".ant-select-item-option"));
  if (!option) throw new Error(`dropdown option not rendered: ${label}`);
  await user.click(option);
}

test("metrics left to the engine start at automatic and can still be turned on or off", async () => {
  const user = userEvent.setup();
  let form: FormInstance | undefined;
  function Capture() {
    form = Form.useFormInstance();
    return null;
  }
  for (const compact of [true, false]) {
    const view = render(
      <I18nextProvider i18n={i18n}>
        <EngineDecidesContext.Provider value={["enable_metrics"]}>
          <Form>
            <Capture />
            <DeployMetricsField spec={metricSpec} compact={compact} />
          </Form>
        </EngineDecidesContext.Provider>
      </I18nextProvider>,
    );
    // Not hidden: the control is there, in its automatic position, and no
    // switch pretends the flag is on.
    assert.equal(
      screen.queryByRole("switch", { name: "Inference Metrics" }),
      null,
    );
    const select = screen.getByRole("combobox", { name: "Inference Metrics" });
    assert.ok(
      view.container.querySelector(".ant-select-selection-item")
        ?.textContent === locale.workbench.triStateAuto,
    );
    assert.equal(form?.getFieldValue("enable_metrics"), undefined);
    await pickOption(user, select, locale.workbench.triStateOn);
    assert.equal(form?.getFieldValue("enable_metrics"), true);
    await pickOption(user, select, locale.workbench.triStateOff);
    assert.equal(form?.getFieldValue("enable_metrics"), false);
    await pickOption(user, select, locale.workbench.triStateAuto);
    assert.equal(form?.getFieldValue("enable_metrics"), undefined);
    view.unmount();
  }
});

test("a memory share left to the engine shows no number until the deployer sets one", async () => {
  const user = userEvent.setup();
  const memorySpec = {
    key: "gpu_memory_utilization",
    flag: "--mem-fraction-static",
    type: "number",
    control: "slider",
    group: "performance",
    default: 0.88,
    min: 0.01,
    max: 0.98,
    step: 0.01,
    label: { en: "GPU memory share", "zh-CN": "显存比例" },
  } as never;
  let form: FormInstance | undefined;
  function Capture() {
    form = Form.useFormInstance();
    return null;
  }
  const view = render(
    <I18nextProvider i18n={i18n}>
      <EngineDecidesContext.Provider value={["gpu_memory_utilization"]}>
        <Form>
          <Capture />
          <DeployParamField spec={memorySpec} />
        </Form>
      </EngineDecidesContext.Provider>
    </I18nextProvider>,
  );
  assert.ok(screen.getByText(locale.workbench.engineDefault));
  assert.equal(screen.queryByRole("slider"), null);
  assert.equal(screen.queryByText("0.88"), null);
  const source = () =>
    screen.getByRole("combobox", { name: locale.workbench.valueSource });
  await pickOption(user, source(), locale.workbench.specify);
  assert.equal(form?.getFieldValue("gpu_memory_utilization"), 0.88);
  assert.ok(screen.getByRole("slider"));
  await pickOption(user, source(), locale.workbench.triStateAuto);
  assert.equal(form?.getFieldValue("gpu_memory_utilization"), undefined);
  assert.equal(screen.queryByRole("slider"), null);
  view.unmount();

  // Outside such a launch the slider shows the dictionary default as before.
  render(
    <I18nextProvider i18n={i18n}>
      <Form>
        <DeployParamField spec={memorySpec} />
      </Form>
    </I18nextProvider>,
  );
  assert.ok(screen.getByRole("slider"));
  assert.ok(screen.getByText("0.88"));
});

test("the payload states exactly what the draft states, whatever the launch leaves to the engine", () => {
  for (const [draft, want] of [
    [{ engine: "sglang", enable_metrics: true }, true],
    [{ engine: "sglang", enable_metrics: false }, false],
    [{ engine: "sglang" }, undefined],
  ] as const) {
    const built = buildPayload(draft, quickProfile, []);
    assert.equal(
      (built.payload.quick_params as Record<string, unknown> | undefined)
        ?.enable_metrics,
      want,
    );
    assert.deepEqual(built.quickParamsUnset, []);
  }
});

test("an evaluation's engine-decides keys replace the template projection", () => {
  const templateProjection = ["enable_metrics"];
  assert.deepEqual(
    effectiveEngineDecidesParamKeys(null, templateProjection),
    templateProjection,
    "before evaluation the selected template answers",
  );
  assert.deepEqual(
    effectiveEngineDecidesParamKeys({ ok: true } as never, templateProjection),
    [],
    "an evaluated LLM override wins even when the empty list is omitted",
  );
  assert.deepEqual(
    effectiveEngineDecidesParamKeys(
      { ok: true, engine_decides_param_keys: ["enable_metrics"] } as never,
      [],
    ),
    ["enable_metrics"],
  );
});

test("an untouched sglang quick form shows dictionary defaults without changing the draft", async () => {
  // The server owns defaults. Merely opening the quick form must not turn
  // metrics on or serialize unrelated dictionary positions into the draft.
  let payload: Record<string, unknown> = {};
  function PayloadProbe() {
    payload = buildPayload(
      (Form.useWatch([], { preserve: true }) || {}) as Record<string, unknown>,
      quickProfile,
    ).payload;
    return null;
  }
  render(
    <I18nextProvider i18n={i18n}>
      <Form>
        <PayloadProbe />
        <DeployQuickForm
          templates={[]}
          nodes={[]}
          profile={quickProfile}
          engine="sglang"
          images={[]}
          result={null}
        />
      </Form>
    </I18nextProvider>,
  );
  const toggle = screen.getByRole("switch", { name: "Inference Metrics" });
  assert.equal(toggle.getAttribute("aria-checked"), "false");
  // An untouched draft sends nothing: the dictionary's defaults are the
  // server's to state, and the captured sglang evaluation answers them.
  assert.deepEqual(payload.quick_params, undefined);
  const user = userEvent.setup({ document: dom.window.document });
  await user.click(toggle);
  await waitFor(() =>
    assert.deepEqual(payload.quick_params, { enable_metrics: true }),
  );
  assert.equal(toggle.getAttribute("aria-checked"), "true");
});

test("an engine dictionary without the metrics param leaves the form untouched", () => {
  const vllmProfile = {
    name: "vllm",
    quick_form_keys: ["enable_prefix_caching"],
    sleep: { supported: false },
    param_specs: [prefixCachingSpec],
  } as never;
  let payload: Record<string, unknown> = {};
  function PayloadProbe() {
    payload = buildPayload(
      (Form.useWatch([], { preserve: true }) || {}) as Record<string, unknown>,
      vllmProfile,
    ).payload;
    return null;
  }
  render(
    <I18nextProvider i18n={i18n}>
      <Form>
        <PayloadProbe />
        <DeployQuickForm
          templates={[]}
          nodes={[]}
          profile={vllmProfile}
          engine="vllm"
          images={[]}
          result={null}
        />
      </Form>
    </I18nextProvider>,
  );
  assert.equal(
    screen.queryByRole("switch", { name: "Inference Metrics" }),
    null,
  );
  // Neither an unavailable parameter nor an untouched dictionary default
  // becomes a user override.
  assert.deepEqual(payload.quick_params, undefined);
});

test("the quick form keeps the metrics switch in a card the size of its neighbours", async () => {
  // The deploy page follows the form through Form.useWatch([]) rather than
  // onValuesChange, so the card has to be observable the same way.
  let payload: Record<string, unknown> = {};
  function PayloadProbe() {
    payload = buildPayload(
      (Form.useWatch([], { preserve: true }) || {}) as Record<string, unknown>,
      quickProfile,
    ).payload;
    return null;
  }
  render(
    <I18nextProvider i18n={i18n}>
      <Form
        initialValues={{
          enable_metrics: true,
          tool_call_parser: "qwen3_coder",
        }}
      >
        <PayloadProbe />
        <DeployQuickForm
          templates={[]}
          nodes={[]}
          profile={quickProfile}
          engine="sglang"
          images={[]}
          result={null}
        />
      </Form>
    </I18nextProvider>,
  );
  const card = screen
    .getByText(locale.workbench.metricsHint)
    .closest(".deploy-feature-card");
  const sibling = screen
    .getByText("Tool Call Parser")
    .closest(".deploy-feature-card");
  assert.ok(card && sibling);
  // The card carries the same markup and state class as the feature cards next
  // to it, instead of the labelled form row with a help line and a warning
  // alert that made it taller and wider than the row it sits in.
  assert.equal(card.className, sibling.className);
  assert.equal(
    card.querySelector(".deploy-feature-heading strong")?.textContent,
    "Inference Metrics",
  );
  assert.equal(card.querySelector(".ant-alert"), null);
  assert.equal(
    screen.queryByText(
      "Expose engine inference metrics for GPUWorks monitoring",
    ),
    null,
  );
  // The feature order is the dictionary's: this profile lists metrics first,
  // and the captured /engines fixture orders it after the engine features.
  const strip = card.parentElement as HTMLElement;
  assert.deepEqual(
    [...strip.querySelectorAll(".deploy-feature-card strong")].map(
      (element) => element.textContent,
    ),
    ["Inference Metrics", "Tool Call Parser", "Prefix Caching"],
  );
  const toggle = screen.getByRole("switch", { name: "Inference Metrics" });
  assert.equal(toggle.getAttribute("aria-checked"), "true");
  const user = userEvent.setup({ document: dom.window.document });
  await user.click(toggle);
  assert.equal(toggle.getAttribute("aria-checked"), "false");
  assert.equal(
    (payload.quick_params as Record<string, unknown>).enable_metrics,
    false,
  );
  assert.ok(screen.getByText(locale.workbench.metricsHint));
});

test("clearing the public model name explains the fallback and remains cleared after model refresh", async () => {
  const modelValues = { id: 1, name: "org/qwen", source_id: "org/qwen" };
  const model = modelValues as never;
  let payload: Record<string, unknown> = {};
  function IdentityFields({ model }: { model: never }) {
    const form = Form.useFormInstance();
    useDeploymentIdentity(form, model);
    return <DeployServedNameField model={model} />;
  }
  const view = (current: never) => (
    <I18nextProvider i18n={i18n}>
      <Form
        onValuesChange={(_, values) => {
          payload = buildPayload(values, profile).payload;
        }}
      >
        <IdentityFields model={current} />
      </Form>
    </I18nextProvider>
  );
  const rendered = render(view(model));
  const input = screen.getByRole("textbox", { name: locale.basic.servedName });
  await waitFor(() =>
    assert.match((input as HTMLInputElement).value, /^org\/qwen-[a-z0-9]{4}$/),
  );
  const user = userEvent.setup({ document: dom.window.document });
  await user.clear(input);
  assert.equal(payload.served_name, "");
  assert.ok(
    screen.getByText(i18n.t("basic.servedNameFallback", { name: "org/qwen" })),
  );
  rendered.rerender(view({ ...modelValues } as never));
  assert.equal((input as HTMLInputElement).value, "");
  await user.type(input, "my-model");
  assert.equal(payload.served_name, "my-model");
  assert.equal(
    screen.queryByText(
      i18n.t("basic.servedNameFallback", { name: "org/qwen" }),
    ),
    null,
  );
});
