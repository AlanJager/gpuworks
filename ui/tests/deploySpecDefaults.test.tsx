import assert from "node:assert/strict";
import { fakeFleet } from "./helpers/fakeFleet.ts";
import { fakeModels } from "./helpers/fakeModels.ts";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";
import { fixturePdModes } from "./helpers/engineFixture.ts";
import { register } from "node:module";
import { pathToFileURL } from "node:url";
import { list } from "./helpers/listPage.ts";
import { pickNodes } from "./helpers/nodePicker.ts";
register(
  pathToFileURL(
    new URL("./deploySleepableCssHook.mjs", import.meta.url).pathname,
  ),
);

// The PD deploy form: choosing "PD disaggregation" as the topology, adding
// and removing role instance rows (including by keyboard), the placement.pd
// payload it builds for evaluate, a field error the server attributes to
// "placement.pd", and evaluate's per-unit preview (a fitting and a
// non-fitting instance). Driven end to end through the real page, the way
// deployEvaluateFieldVisibility.test.tsx drives field-error attribution.

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
  "NodeFilter",
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
  fs.readFileSync(new URL("../src/i18n/locales/en/deploy.json", import.meta.url), "utf8"),
);
const commonLocale = JSON.parse(
  fs.readFileSync(new URL("../src/i18n/locales/en/common.json", import.meta.url), "utf8"),
);
const imagesLocale = JSON.parse(
  fs.readFileSync(new URL("../src/i18n/locales/en/images.json", import.meta.url), "utf8"),
);
const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: { en: { deploy: deployLocale, common: commonLocale, images: imagesLocale } },
  defaultNS: "common",
  interpolation: { escapeValue: false },
});

const { MemoryRouter } = await import("react-router-dom");
const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { apiErrorText, APIError } = await import("../src/apiError.ts");
const { api } = await import("../src/api.ts");

function errorText(code: string, params?: Record<string, unknown>) {
  return apiErrorText(
    (key: string, options?: Record<string, unknown>) => i18n.t(key, options),
    new APIError(code, params ?? {}, code, 200),
  );
}

const gpu = (index: number) => ({
  index,
  name: "H100",
  vendor: "nvidia",
  schedulable: true,
  arch_id: "Hopper",
  compute_capability: "9.0",
});
const nodeOne = {
  uuid: "node-1",
  name: "GPU Node 1",
  hostname: "gpu-node-1",
  state: "online",
  gpu_count: 2,
  gpu_vendor: "nvidia",
  gpus: [gpu(0), gpu(1)],
} as never;
const nodeTwo = {
  uuid: "node-2",
  name: "GPU Node 2",
  hostname: "gpu-node-2",
  state: "online",
  gpu_count: 2,
  gpu_vendor: "nvidia",
  gpus: [gpu(0), gpu(1)],
} as never;
const model = {
  id: 3,
  name: "Qwen/Qwen3-0.6B",
  source: "modelscope",
  source_id: "Qwen/Qwen3-0.6B",
  input_modalities: ["text"],
  weight_bytes: 1_500_000_000,
  nodes: [{ node_uuid: "node-1", state: "present" }],
} as never;
const engineProfile = {
  name: "vllm",
  sleep: { supported: false },
  quick_form_keys: [],
  param_specs: [],
  // Two modes, ascend listed first - exercises defaultPdMode's vendor match
  // (the nvidia vendor picked in openDeployPage) rather than a bare modes[0].
  pd_modes: fixturePdModes("vllm"),
} as never;

const apiAny = api as unknown as Record<string, unknown>;
const originals: Record<string, unknown> = {};
function stub(name: string, value: unknown) {
  originals[name] = apiAny[name];
  apiAny[name] = value;
}

// stubFleet answers the node endpoints as the server does (one searched page,
// single nodes, facets) and refuses listing every node.
function stubFleet(list: unknown[]) {
  const fake = fakeFleet(list as never);
  for (const name of ["nodes", "nodesPage", "node", "nodeFacets"] as const) stub(name, fake[name]);
  return fake;
}
// stubModels answers the model endpoints as the server does (one searched
// page, single models) and refuses listing every model.
function stubModels(list: unknown[]) {
  const fake = fakeModels(list as never);
  stub("models", fake.models_all);
  for (const name of ["modelsPage", "model"] as const) stub(name, fake[name]);
  return fake;
}
function resetStubs() {
  stubModels([model]);
  stubFleet([nodeOne, nodeTwo]);
  stub("engines", async () => list([engineProfile]));
  stub("inferenceTemplateIndex", async () => ({ items: [] }));
  stub("imageVersions", async () => list([]));
  stub("images", async () => list([]));
  stub("routingPolicies", async () => list([]));
}
resetStubs();
afterEach(() => {
  cleanup();
  resetStubs();
});
after(() => {
  closeChannels();
  dom.window.close();
});

const baseAnswer = {
  ok: true,
  warnings: [] as Array<{ code: string; params?: Record<string, unknown> }>,
  defaults: { model_mount: "/model", master_port: 29500, engine: "vllm" },
  form_values: { engine: "vllm" },
  image: "runner:vllm",
  command: ["vllm", "serve", "/model"],
  argv: [],
  template_deviated: false,
  template_match: true,
  eligible_nodes: [
    { uuid: "node-1", name: "GPU Node 1", eligible: true },
    { uuid: "node-2", name: "GPU Node 2", eligible: true },
  ],
  acquisition: { image: { image: "runner:vllm", cached: true } },
};

// The node picker lists a server search's nodes once it opens.
async function selectOption(combobox: HTMLElement, label: string) {
  await pickNodes(combobox, label);
}

const { buildPayload } = await import("../src/pages/deploy/deployHelpers.ts");
const { Form } = await import("antd");
const useDeploySpecDefaults = (await import("../src/pages/deploy/useDeploySpecDefaults.ts")).default;

// Template specs that declare their own PD layout and launch script
// (InferenceSpecDTO pd_roles / manual_command): selecting one starts the
// create form from them. Driven through the real page by the deploy link's
// template/spec query, and through the hook for the "already typed" cases.

const script = '#!/bin/sh\nset -eu\nexec python3 -m sglang.launch_server --model-path "$MODEL"\n';
const pdShape = {
  decode: { tp: 4, pp: 1, nodes: 2, gpus_per_node: 2, units: 1 },
  prefill: { tp: 2, pp: 2, nodes: 1, gpus_per_node: 2, units: 2 },
};
const specBase = { id: 1, source: "upstream", engine: "vllm", image: "runner:vllm", weight_sources: [], gpus: { vendor: "nvidia" } };
const specs = [
  { ...specBase, id: 1, name: "pd-spec", pd_mode: "vllm-nixl", pd_roles: pdShape, manual_command: script },
  { ...specBase, id: 2, name: "pd-no-shape", pd_mode: "vllm-nixl" },
  { ...specBase, id: 3, name: "script-only", manual_command: script },
  { ...specBase, id: 4, name: "plain" },
];
const profiles = specs.map((spec) => ({ name: spec.name, source: "upstream", engine: "vllm", image: "runner:vllm", gpus: { vendor: "nvidia" } }));

async function openSpec(name: string) {
  stub("inferenceTemplateIndex", async () => ({
    items: [{ id: 7, model_id: "org/pd-model", source: "upstream", engines: ["vllm"], scenarios: [], profiles }],
  }));
  stub("inferenceTemplate", async () => ({ id: 7, model_id: "org/pd-model", source: "upstream", specs }));
  const payloads: Array<Record<string, unknown>> = [];
  stub("evaluate", async (payload: Record<string, unknown>) => {
    payloads.push(payload);
    return { ...baseAnswer };
  });
  const DeployPage = (await import("../src/pages/DeployPage.tsx")).default;
  render(
    <MemoryRouter initialEntries={[`/deploy?model=3&template=7&spec=${name}&spec_source=upstream`]}>
      <I18nextProvider i18n={i18n}>
        <DeployPage />
      </I18nextProvider>
    </MemoryRouter>,
  );
  return payloads;
}
const label = (key: "tpAria" | "ppAria", role: "decode" | "prefill") =>
  deployLocale.pd[key].replace("{{role}}", deployLocale.pd.role[role]);
const nodeBox = (role: "decode" | "prefill", index: number, follower?: number) =>
  new RegExp(`^${deployLocale.pd.role[role]} instance ${index} node${follower ? ` ${follower}` : ""}$`);

test("selecting a PD spec with a layout opens the PD form with its roles, TP/PP, instances and follower slots", async () => {
  const payloads = await openSpec("pd-spec");
  await screen.findByText(deployLocale.pd.mode);
  const value = (name: string) => (screen.getByRole("spinbutton", { name }) as HTMLInputElement).value;
  await waitFor(() => assert.equal(value(label("tpAria", "decode")), "4"));
  assert.equal(value(label("ppAria", "decode")), "1");
  assert.equal(value(label("tpAria", "prefill")), "2");
  assert.equal(value(label("ppAria", "prefill")), "2");
  // Decode: one instance with one follower slot; prefill: two single-node instances.
  assert.ok(screen.getByRole("combobox", { name: nodeBox("decode", 1) }));
  assert.ok(screen.getByRole("combobox", { name: nodeBox("decode", 1, 1) }));
  assert.equal(screen.queryByRole("combobox", { name: nodeBox("decode", 2) }), null);
  assert.ok(screen.getByRole("combobox", { name: nodeBox("prefill", 1) }));
  assert.ok(screen.getByRole("combobox", { name: nodeBox("prefill", 2) }));
  assert.equal(screen.queryByRole("combobox", { name: nodeBox("prefill", 1, 1) }), null);

  // Pick the nodes the form left empty: the request carries tp/pp, node_uuid and followers.
  await selectOption(screen.getByRole("combobox", { name: nodeBox("decode", 1) }), "GPU Node 1 · 2 GPU");
  await selectOption(screen.getByRole("combobox", { name: nodeBox("decode", 1, 1) }), "GPU Node 2 · 2 GPU");
  await selectOption(screen.getByRole("combobox", { name: nodeBox("prefill", 1) }), "GPU Node 1 · 2 GPU");
  await selectOption(screen.getByRole("combobox", { name: nodeBox("prefill", 2) }), "GPU Node 2 · 2 GPU");
  await waitFor(() => {
    const last = payloads.at(-1) as { placement_mode?: string; placement?: { pd?: Record<string, unknown> } };
    // Manual placement is the absence of placement_mode on the wire.
    assert.equal(last.placement_mode, undefined);
    assert.deepEqual(last.placement?.pd, {
      mode: "vllm-nixl",
      fallback: "decode",
      kv_load_failure: "fail",
      roles: {
        decode: { tp: 4, pp: 1, units: [{ node_uuid: "node-1", followers: [{ node_uuid: "node-2" }] }] },
        prefill: { tp: 2, pp: 2, units: [{ node_uuid: "node-1" }, { node_uuid: "node-2" }] },
      },
    });
  });
});

test("selecting a spec with a launch script starts in manual command mode with the script", async () => {
  const payloads = await openSpec("script-only");
  await waitFor(() => {
    const editor = document.querySelector("textarea.deploy-command-editor") as HTMLTextAreaElement | null;
    assert.ok(editor, "the manual command editor is shown");
    assert.equal(editor!.value, script);
  });
  await waitFor(() => {
    const last = payloads.at(-1) as { container_overrides?: Record<string, unknown>; placement?: { pd?: unknown } };
    assert.equal(last.container_overrides?.command_mode, "manual");
    assert.equal(last.container_overrides?.cmd_display, script.trim());
    assert.equal(last.placement?.pd, undefined, "a spec without pd_mode keeps the ordinary topology");
  });
});

test("a PD spec without a layout switches to PD with its mode and keeps the empty role defaults", async () => {
  const payloads = await openSpec("pd-no-shape");
  await screen.findByText(deployLocale.pd.mode);
  await waitFor(() => assert.equal((screen.getByRole("spinbutton", { name: label("tpAria", "decode") }) as HTMLInputElement).value, "1"));
  // Still placed by the platform: instance rows without node pickers.
  assert.equal(screen.queryByRole("combobox", { name: nodeBox("decode", 1) }), null);
  await waitFor(() => {
    const last = payloads.at(-1) as { placement_mode?: string; placement?: { pd?: { mode: string; roles: Record<string, { units: unknown[] }> } } };
    assert.equal(last.placement_mode, "auto");
    assert.equal(last.placement?.pd?.mode, "vllm-nixl");
    assert.equal(last.placement?.pd?.roles.decode.units.length, 1);
  });
  assert.equal(document.querySelector("textarea.deploy-command-editor"), null);
});

test("a spec with neither field leaves the form as it was", async () => {
  const payloads = await openSpec("plain");
  await waitFor(() => assert.ok(payloads.length > 0));
  assert.equal(screen.queryByText(deployLocale.pd.mode), null);
  assert.equal(document.querySelector("textarea.deploy-command-editor"), null);
  const last = payloads.at(-1) as { container_overrides?: unknown; placement?: { pd?: unknown } };
  assert.equal(last.container_overrides, undefined);
  assert.equal(last.placement?.pd, undefined);
});

// The hook alone, on a bare form, for the cases the page cannot set up.
type Spec = Parameters<typeof useDeploySpecDefaults>[0]["spec"];
function mountHook(initial: Record<string, unknown>, first: Spec, enabled = true) {
  let form: import("antd").FormInstance | undefined;
  let rerender: (spec: Spec) => void = () => {};
  function Host({ spec }: { spec: Spec }) {
    const [instance] = Form.useForm();
    form = instance;
    useDeploySpecDefaults({ form: instance, templateID: 7, spec, enabled });
    return <Form form={instance} initialValues={initial} />;
  }
  const view = render(<Host spec={first} />);
  rerender = (spec) => view.rerender(<Host spec={spec} />);
  return { form: () => form!, rerender };
}
const scriptSpec = { id: 3, source: "upstream", name: "script-only", manual_command: script };
const otherScriptSpec = { id: 5, source: "upstream", name: "other-script", manual_command: "echo other" };
const plainSpec = { id: 4, source: "upstream", name: "plain" };

test("a command the deployer already typed is not replaced by the spec's script", async () => {
  const { form } = mountHook({ command_mode: "engine", container_cmd: "my own command" }, scriptSpec);
  await waitFor(() => assert.equal(form().getFieldValue("container_cmd"), "my own command"));
  assert.equal(form().getFieldValue("command_mode"), "engine");
  assert.equal(buildPayload(form().getFieldsValue(true)).payload.container_overrides?.cmd_display, "my own command");
});

test("a form opened from a deployment is left alone", async () => {
  const { form } = mountHook({ command_mode: "engine" }, scriptSpec, false);
  await new Promise((resolve) => setTimeout(resolve, 20));
  assert.equal(form().getFieldValue("command_mode"), "engine");
  assert.equal(form().getFieldValue("container_cmd"), undefined);
});

test("changing the spec swaps an untouched script and drops it for a spec without one, but keeps an edited one", async () => {
  const { form, rerender } = mountHook({ command_mode: "engine" }, scriptSpec);
  await waitFor(() => assert.equal(form().getFieldValue("container_cmd"), script));
  rerender(otherScriptSpec);
  await waitFor(() => assert.equal(form().getFieldValue("container_cmd"), "echo other"));
  rerender(plainSpec);
  await waitFor(() => assert.equal(form().getFieldValue("command_mode"), "engine"));
  assert.equal(form().getFieldValue("container_cmd"), undefined);

  rerender(scriptSpec);
  await waitFor(() => assert.equal(form().getFieldValue("container_cmd"), script));
  form().setFieldValue("container_cmd", `${script}# edited`);
  rerender(plainSpec);
  await new Promise((resolve) => setTimeout(resolve, 20));
  assert.equal(form().getFieldValue("command_mode"), "manual");
  assert.equal(form().getFieldValue("container_cmd"), `${script}# edited`);
});
