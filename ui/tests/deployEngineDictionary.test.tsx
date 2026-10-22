import assert from "node:assert/strict";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

// These cases render the deploy form against the dictionary the server really
// publishes, captured from /api/v1/engines by
// internal/server/api/engines_fixture_test.go. Every other form test writes the
// profile it wants, so a key the server stops publishing — the KV tier keys
// were missing from vLLM's quick form whitelist for months — used to be
// invisible on both sides. Here the front end fails when the server's own
// response stops carrying what the KV Offload controls need.

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

const deployLocale = JSON.parse(
  fs.readFileSync(
    new URL("../src/i18n/locales/en/deploy.json", import.meta.url),
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
  resources: { en: { deploy: deployLocale, common: commonLocale } },
  defaultNS: "common",
  interpolation: { escapeValue: false },
});
const { act, cleanup, fireEvent, render, screen, waitFor } =
  await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { Form } = await import("antd");
type FormInstance = import("antd").FormInstance;
type EngineProfile = import("../src/api.ts").EngineProfile;
const DeployQuickForm = (
  await import("../src/pages/deploy/DeployQuickForm.tsx")
).default;
const DeployPerformanceStep = (
  await import("../src/pages/deploy/DeployPerformanceStep.tsx")
).default;
const { buildPayload } = await import("../src/pages/deploy/deployHelpers.ts");

// The disk tier's companions are the depends_on rows of its key in the
// published dictionary - the same relation the payload projection reads.
const diskTierCompanionSpecs = (
  specs?: { key: string; depends_on?: string }[],
) =>
  (specs ?? []).filter(
    (spec) =>
      spec.depends_on === "kv_disk_offload" && spec.key !== "kv_disk_offload",
  );

const engines = JSON.parse(
  fs.readFileSync(new URL("./fixtures/engines.json", import.meta.url), "utf8"),
) as EngineProfile[];
const sglangFixture = JSON.parse(
  fs.readFileSync(
    new URL("./fixtures/evaluate/template_sglang.json", import.meta.url),
    "utf8",
  ),
) as { form_values?: Record<string, unknown> };
const vllm = engines.find((profile) => profile.name === "vllm");
assert.ok(
  vllm,
  "the captured /api/v1/engines response must carry a vllm profile",
);
const sglang = engines.find((profile) => profile.name === "sglang");
assert.ok(
  sglang,
  "the captured /api/v1/engines response must carry an sglang profile",
);
const model = {
  id: 1,
  name: "org/qwen",
  source_id: "org/qwen",
  input_modalities: ["text"],
} as never;

afterEach(cleanup);

const fieldCount = (label: string) => screen.queryAllByLabelText(label).length;
const cardFor = (title: string) =>
  screen
    .getAllByText(title)
    .map((element) => element.closest("button.deploy-choice-card"))
    .find(Boolean) as HTMLButtonElement;

after(() => {
  closeChannels();
  dom.window.close();
});

test("the published dictionary names every disk tier companion by dependency", () => {
  assert.deepEqual(
    diskTierCompanionSpecs(vllm!.param_specs).map((spec) => spec.key),
    [
      "shared_kv_dir",
      "kv_offload_read_threads",
      "kv_offload_write_threads",
      "kv_disk_limit_gib",
    ],
    "the disk tier keys the form renders come from the server's dictionary, not a list the form keeps",
  );
});

test("the SGLang cache-report ON default reaches evaluation and eager is a direct feature", () => {
  let form: FormInstance | undefined;
  function Host() {
    const [instance] = Form.useForm();
    form = instance;
    return (
      <Form
        form={instance}
        initialValues={{ engine: "sglang", command_mode: "engine" }}
      >
        <DeployQuickForm
          templates={[]}
          nodes={[]}
          selectedModel={model}
          profile={sglang}
          engine="sglang"
          images={[]}
          result={null}
        />
      </Form>
    );
  }
  render(
    <I18nextProvider i18n={i18n}>
      <Host />
    </I18nextProvider>,
  );
  assert.ok(
    screen.getByText("Enforce Eager Mode").closest(".deploy-choice-strip"),
    "eager belongs in the directly visible feature row",
  );
  assert.equal(
    form!.getFieldValue("enable_cache_report"),
    undefined,
    "opening the form does not change the draft",
  );
  // The server states the dictionary's defaults at materialization: an
  // untouched draft sends nothing, and the sglang fixture's form_values answer
  // carries the ON cache-report default the form will display.
  const quick = buildPayload(form!.getFieldsValue(true), sglang, [
    "text",
  ]).quick;
  assert.equal(quick.enable_cache_report, undefined);
  assert.equal(quick.enforce_eager, undefined);
  assert.equal(
    sglangFixture.form_values?.enable_cache_report,
    true,
    "the captured evaluation answers the dictionary's ON default",
  );
});

test("the quick form's KV cards are live against the published quick form whitelist", async () => {
  let form: FormInstance | undefined;
  function Host() {
    const [instance] = Form.useForm();
    form = instance;
    return (
      <Form
        form={instance}
        initialValues={{ engine: "vllm", command_mode: "engine" }}
      >
        <DeployQuickForm
          templates={[]}
          nodes={[]}
          selectedModel={model}
          profile={vllm}
          engine="vllm"
          images={[]}
          result={null}
        />
      </Form>
    );
  }
  render(
    <I18nextProvider i18n={i18n}>
      <Host />
    </I18nextProvider>,
  );
  const user = userEvent.setup({ document: dom.window.document });
  for (const title of [
    deployLocale.workbench.kv.disabled,
    deployLocale.workbench.kv.memory,
    deployLocale.workbench.kv.disk,
  ]) {
    assert.equal(
      cardFor(title).textContent?.includes(
        deployLocale.workbench.engineUnavailable,
      ),
      false,
      `the ${title} card reads as unavailable, so the server dropped its key from quick_form_keys`,
    );
  }
  assert.equal(cardFor(deployLocale.workbench.kv.memory).disabled, false);

  await user.click(cardFor(deployLocale.workbench.kv.memory));
  await waitFor(() => assert.equal(fieldCount("KV Offload to Memory"), 1));
  assert.equal(form!.getFieldValue("kv_offloading_size"), undefined);
  assert.equal(cardFor(deployLocale.workbench.kv.disk).disabled, true);
  await user.type(screen.getByLabelText("KV Offload to Memory"), "32");
  await waitFor(() =>
    assert.equal(cardFor(deployLocale.workbench.kv.disk).disabled, false),
  );

  await user.click(cardFor(deployLocale.workbench.kv.disk));
  await waitFor(() => assert.equal(fieldCount("Shared KV Directory"), 1));
  assert.equal(form!.getFieldValue("kv_disk_offload"), true);
  assert.equal(fieldCount("Disk Read Threads"), 1);
  assert.equal(fieldCount("Disk Write Threads"), 1);
  assert.equal(fieldCount("Disk Tier Capacity (GiB)"), 1);
});

test("the expert performance step's KV switches are live against the published dictionary", async () => {
  let form: FormInstance | undefined;
  function Host() {
    const [instance] = Form.useForm();
    form = instance;
    return (
      <Form form={instance} layout="vertical">
        <DeployPerformanceStep profile={vllm} engine="vllm" model={model} />
      </Form>
    );
  }
  render(
    <I18nextProvider i18n={i18n}>
      <Host />
    </I18nextProvider>,
  );
  const user = userEvent.setup({ document: dom.window.document });
  // The performance group publishes other switches too, so the tier switches
  // are taken from the KV tier blocks rather than from the page's switch order.
  const tierSwitches = Array.from(
    dom.window.document.querySelectorAll(".deploy-kv-tier [role=switch]"),
  ) as HTMLElement[];
  assert.equal(
    tierSwitches.length,
    2,
    "the memory and disk tiers each own one switch",
  );
  const [memorySwitch, diskSwitch] = tierSwitches;
  assert.equal(memorySwitch.getAttribute("aria-checked"), "false");
  assert.equal(
    diskSwitch.hasAttribute("disabled"),
    true,
    "the disk tier waits for the memory tier",
  );

  await user.click(memorySwitch);
  await waitFor(() =>
    assert.equal(memorySwitch.getAttribute("aria-checked"), "true"),
  );
  assert.equal(form!.getFieldValue("kv_offloading_size"), undefined);
  await user.type(screen.getByLabelText("KV Offload to Memory"), "32");
  await waitFor(() => assert.equal(diskSwitch.hasAttribute("disabled"), false));

  await user.click(diskSwitch);
  await waitFor(() =>
    assert.equal(diskSwitch.getAttribute("aria-checked"), "true"),
  );
  assert.equal(form!.getFieldValue("kv_disk_offload"), true);
  await waitFor(() => assert.equal(fieldCount("Shared KV Directory"), 1));
  assert.equal(fieldCount("Disk Read Threads"), 1);
  assert.equal(fieldCount("Disk Write Threads"), 1);
  assert.equal(fieldCount("Disk Tier Capacity (GiB)"), 1);
});

// A platform parameter can depend on an engine tier even though it names no
// engine flag. The explicit dependency keeps the draft grouped with the disk
// controls and lets disabling the tier suppress the agent sweeper ceiling too.
test("the disk tier's capacity ceiling follows the server-owned dependency", async () => {
  const ceiling = vllm!.param_specs.find(
    (spec) => spec.key === "kv_disk_limit_gib",
  );
  assert.ok(ceiling, "the server no longer publishes the platform parameter");
  assert.equal(
    ceiling!.flag,
    "",
    "a platform parameter must advertise no engine flag",
  );
  assert.equal(
    diskTierCompanionSpecs(vllm!.param_specs).some(
      (spec) => spec.key === "kv_disk_limit_gib",
    ),
    true,
    "the platform ceiling dependency disappeared from the published dictionary",
  );

  render(
    <I18nextProvider i18n={i18n}>
      <Form layout="vertical" initialValues={{ kv_offloading_size: 32 }}>
        <DeployPerformanceStep profile={vllm} engine="vllm" model={model} />
      </Form>
    </I18nextProvider>,
  );
  const diskSwitch = (
    Array.from(
      dom.window.document.querySelectorAll(".deploy-kv-tier [role=switch]"),
    ) as HTMLElement[]
  )[1];
  assert.equal(diskSwitch.getAttribute("aria-checked"), "false");
  assert.equal(fieldCount("Disk Tier Capacity (GiB)"), 0);
  assert.equal(fieldCount("Shared KV Directory"), 0);
  await userEvent.click(diskSwitch);
  await waitFor(() => assert.equal(fieldCount("Disk Tier Capacity (GiB)"), 1));
});

test("SGLang logging is visible in Features, defaults from the real dictionary, and retains Unset across modes", async () => {
  const profile = engines.find((item) => item.name === "sglang")!;
  const spec = profile.param_specs!.find((item) => item.key === "log_level")!;
  let form: FormInstance;
  function Host({
    expert = false,
    manual = false,
  }: {
    expert?: boolean;
    manual?: boolean;
  }) {
    const [instance] = Form.useForm();
    form = instance;
    return (
      <I18nextProvider i18n={i18n}>
        <Form
          form={instance}
          initialValues={{ engine: "sglang", command_mode: "engine" }}
        >
          {expert ? (
            <DeployPerformanceStep
              profile={profile}
              engine="sglang"
              model={model}
            />
          ) : (
            <DeployQuickForm
              templates={[]}
              nodes={[]}
              selectedModel={model}
              profile={profile}
              engine="sglang"
              images={[]}
              result={null}
            />
          )}
          <button
            type="button"
            onClick={() =>
              instance.setFieldValue(
                "command_mode",
                manual ? "engine" : "manual",
              )
            }
          >
            Change command mode
          </button>
        </Form>
      </I18nextProvider>
    );
  }
  const view = render(<Host />);
  const label = spec.label.en!;
  // The dictionary default is the server's to state: the untouched draft sends
  // nothing, and the captured evaluation's form_values carries it for display.
  await waitFor(() =>
    assert.equal(
      buildPayload(form!.getFieldsValue(true), profile).quick.log_level,
      undefined,
    ),
  );
  const card = screen
    .getByRole("combobox", { name: label })
    .closest(".deploy-feature-card");
  assert.ok(
    card,
    "logging is a visible feature card without opening advanced parameters",
  );
  const { applyEvaluateFormValues } =
    await import("../src/pages/deploy/deployHelpers.ts");
  act(() =>
    applyEvaluateFormValues(
      form!,
      { form_values: { log_level: "info" } } as never,
      [],
    ),
  );
  await waitFor(() => assert.equal(form!.getFieldValue("log_level"), "info"));
  const user = userEvent.setup({ document: dom.window.document });
  const select = screen.getByRole("combobox", { name: label });
  fireEvent.mouseDown(
    select.closest(".ant-select")!.querySelector(".ant-select-selector")!,
  );
  await user.click(
    screen
      .getAllByText(deployLocale.workbench.unset)
      .find((el) => el.closest(".ant-select-item-option"))!,
  );
  await waitFor(() => {
    const built = buildPayload(form!.getFieldsValue(true), profile);
    assert.equal(built.quick.log_level, undefined);
    assert.deepEqual(built.quickParamsUnset, ["log_level"]);
  });
  assert.ok(screen.getByText(deployLocale.workbench.logLevelUnset));
  view.rerender(<Host expert />);
  await waitFor(() =>
    assert.equal(
      screen.getByRole("combobox", { name: label }).closest(".ant-select")
        ?.textContent,
      deployLocale.workbench.unset,
    ),
  );
  assert.deepEqual(
    buildPayload(form!.getFieldsValue(true), profile).quickParamsUnset,
    ["log_level"],
  );
  view.rerender(<Host />);
  assert.deepEqual(
    buildPayload(form!.getFieldsValue(true), profile).quickParamsUnset,
    ["log_level"],
  );
  const nextSelect = screen.getByRole("combobox", { name: label });
  fireEvent.mouseDown(
    nextSelect.closest(".ant-select")!.querySelector(".ant-select-selector")!,
  );
  fireEvent.keyDown(nextSelect, {
    key: "ArrowDown",
    code: "ArrowDown",
    keyCode: 40,
  });
  fireEvent.keyDown(nextSelect, { key: "Enter", code: "Enter", keyCode: 13 });
  await waitFor(() =>
    assert.equal(
      buildPayload(form!.getFieldsValue(true), profile).quick.log_level,
      spec.options![0].value,
    ),
  );
  // A template level arrives as the server's form_values display instruction.
  act(() =>
    applyEvaluateFormValues(
      form!,
      { form_values: { log_level: "error" } } as never,
      [],
    ),
  );
  await waitFor(() =>
    assert.equal(
      buildPayload(form!.getFieldsValue(true), profile).quick.log_level,
      "error",
    ),
  );
  await user.click(screen.getByRole("button", { name: "Change command mode" }));
  assert.equal(
    screen.getByRole("combobox", { name: label }).hasAttribute("disabled"),
    true,
  );
});
