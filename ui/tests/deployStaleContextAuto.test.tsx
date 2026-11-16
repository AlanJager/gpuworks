import { createI18n, dom } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import fs from "node:fs";
import { register } from "node:module";
import { pathToFileURL } from "node:url";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import type { EngineProfile, EvaluateResponse } from "../src/api.ts";
import { fakeFleet } from "./helpers/fakeFleet.ts";
import { fakeModels } from "./helpers/fakeModels.ts";
import { list } from "./helpers/listPage.ts";
register(pathToFileURL(new URL("./deploySleepableCssHook.mjs", import.meta.url).pathname));

// The dictionary gives vLLM's maximum context the native automatic sentinel
// (-1) as its default, and gives SGLang's none. A form first evaluated for
// vLLM displays -1 as "Automatic"; choosing an SGLang template afterwards must
// not carry that displayed default into the SGLang draft, where -1 is no
// value at all (the server refuses it as negative).

const { act, cleanup, fireEvent, render, screen, waitFor } = await import("@testing-library/react");
const { MemoryRouter } = await import("react-router-dom");
const { Form } = await import("antd");
const { api } = await import("../src/api.ts");
const useEvaluation = (await import("../src/pages/deploy/useDeployEvaluation.ts")).default;
const DeployParamField = (await import("../src/pages/deploy/DeployParamField.tsx")).default;
const { buildPayload } = await import("../src/pages/deploy/deployHelpers.ts");
const { i18n, locale } = await createI18n(["deploy", "images"]);
const deployLocale = locale.deploy;
const profiles = JSON.parse(fs.readFileSync(new URL("./fixtures/engines.json", import.meta.url), "utf8")) as EngineProfile[];
const automatic = i18n.t("shared.automatic", { ns: "deploy" });

const node = {
  uuid: "node-uuid-1", hostname: "gpu3", state: "online", gpu_count: 1, gpu_vendor: "nvidia",
  gpus: [{ index: 0, name: "NVIDIA GeForce RTX 2080 Ti", vendor: "nvidia", schedulable: true, arch_id: "Turing", compute_capability: "7.5" }],
} as never;
const model = {
  id: 3, name: "Qwen/Qwen3-0.6B", source: "modelscope", source_id: "Qwen/Qwen3-0.6B", input_modalities: ["text"],
  weight_bytes: 1_500_000_000, nodes: [{ node_uuid: "node-uuid-1", state: "present" }],
} as never;
const sglangTemplate = {
  id: 41, model_id: "Qwen/Qwen3-0.6B", display_title: "Qwen3 0.6B", source: "upstream", scenarios: ["chat"],
  profiles: [{ name: "sglang-nvidia", engine: "sglang", image: "lmsysorg/sglang:v0.5.21", detected_compatible: true, gpus: { vendor: "nvidia" } }],
};

const apiAny = api as unknown as Record<string, unknown>;
const originals = new Map<string, unknown>();
function stub(name: string, value: unknown) {
  if (!originals.has(name)) originals.set(name, apiAny[name]);
  apiAny[name] = value;
}
const requests: Record<string, any>[] = [];
function answer(request: Record<string, any>): EvaluateResponse {
  const engine = String(request.engine);
  const base = {
    warnings: [], defaults: { model_mount: "/model", master_port: 29500, engine: "vllm" }, image: request.image || `runner:${engine}`,
    command: [engine], argv: [], entrypoint: [], env: {}, injected: [],
    eligible_nodes: [{ uuid: "node-uuid-1", name: "gpu3", eligible: true }],
    selected_node: { uuid: "node-uuid-1", name: "gpu3", reasons: [{ code: "auto_selected" }] },
  };
  const stated = request.quick_params?.max_model_len;
  // The server's own rule (deploy.validateQuickParams): only vLLM's
  // --max-model-len takes the -1 sentinel; an early field error carries no
  // display instruction.
  if (engine !== "vllm" && typeof stated === "number" && stated < 0) {
    return { ...base, ok: false, errors: [{ field: "max_model_len", code: "FIELD_INVALID", params: { field: "max_model_len" } }] } as never;
  }
  // form_values are the effective values: the dictionary default for vLLM,
  // nothing for SGLang unless the draft states a context.
  const context = stated ?? (engine === "vllm" ? -1 : undefined);
  return {
    ...base, ok: true, extra_args: "",
    form_values: { extra_args: "", ...(context === undefined ? {} : { max_model_len: context }) },
  } as never;
}

function resetStubs() {
  requests.length = 0;
  const models = fakeModels([model]);
  stub("models", models.models_all);
  for (const name of ["modelsPage", "model"] as const) stub(name, models[name]);
  const fleet = fakeFleet([node]);
  for (const name of ["nodes", "nodesPage", "node", "nodeFacets"] as const) stub(name, fleet[name]);
  stub("engines", async () => list(profiles));
  stub("inferenceTemplateIndex", async () => ({ items: [sglangTemplate], hardware: [] }));
  stub("inferenceTemplate", async () => ({ ...sglangTemplate, specs: [] }));
  stub("imageVersions", async () => list([]));
  stub("images", async () => list([]));
  stub("evaluate", async (payload: Record<string, unknown>) => {
    const request = structuredClone(payload) as Record<string, any>;
    requests.push(request);
    return answer(request);
  });
}
resetStubs();
afterEach(() => { cleanup(); resetStubs(); });
after(() => {
  for (const [name, value] of originals) apiAny[name] = value;
  dom.window.close();
});

const contextInput = () => screen.getByRole("spinbutton", { name: /Maximum Context Length/ }) as HTMLInputElement;

async function openOnVLLM() {
  const DeployPage = (await import("../src/pages/DeployPage.tsx")).default;
  render(
    <MemoryRouter initialEntries={["/deploy?model=3"]}>
      <I18nextProvider i18n={i18n}><DeployPage /></I18nextProvider>
    </MemoryRouter>,
  );
  const vendor = await screen.findByRole("combobox", { name: new RegExp(deployLocale.basic.targetVendor) });
  fireEvent.mouseDown(vendor);
  fireEvent.click(await screen.findByTitle(deployLocale.basic.vendor.nvidia));
  // The vLLM answer lands its dictionary default: the context shows automatic.
  await waitFor(() => assert.equal(contextInput().value, automatic), { timeout: 5000 });
}

test("an SGLang template does not inherit vLLM's automatic context default", async () => {
  await openOnVLLM();
  fireEvent.click(await screen.findByRole("button", { name: /Qwen3 0\.6B/ }));
  await waitFor(() => assert.ok(requests.some((request) => request.engine === "sglang")), { timeout: 5000 });
  await waitFor(() => {
    const last = requests.at(-1)!;
    assert.equal(last.engine, "sglang");
    assert.equal(last.quick_params?.max_model_len, undefined);
    assert.equal(contextInput().value, "");
  }, { timeout: 5000 });
  for (const request of requests.filter((item) => item.engine === "sglang")) {
    assert.notEqual(request.quick_params?.max_model_len, -1, "no SGLang evaluation states the vLLM sentinel");
  }
  assert.equal(screen.queryByText(/FIELD_INVALID|Invalid value/i), null);
});

// The same feedback loop without the page around it, where the second engine
// may arrive with a manual command (as a PD template's launch script does): a
// manual draft serializes the whole form, so a displayed default still on it
// would be sent as if the deployer had stated it.
let form: import("antd").FormInstance;
const profileOf = (engine: string) => profiles.find((profile) => profile.name === engine)!;
function Host() {
  [form] = Form.useForm();
  const values = Form.useWatch([], { form, preserve: true }) || {};
  const engine = String(values.engine || "vllm");
  const spec = profileOf(engine).param_specs!.find((item) => item.key === "max_model_len")!;
  const evaluation = useEvaluation({
    form, signal: values,
    buildPayload: () => buildPayload(form.getFieldsValue(true), profileOf(String(form.getFieldValue("engine")))).payload,
    translate: (key, options) => i18n.t(key, options),
  });
  return (
    <Form form={form} initialValues={{ engine: "vllm" }} onValuesChange={(changed) => evaluation.noteDraftChanged(changed)}>
      <DeployParamField key={engine} spec={spec} />
      <output data-testid="pending">{String(evaluation.evaluating)}</output>
    </Form>
  );
}
const settled = () => waitFor(() => assert.equal(screen.getByTestId("pending").textContent, "false"), { timeout: 5000 });
async function openHost() {
  render(<I18nextProvider i18n={i18n}><Host /></I18nextProvider>);
  await waitFor(() => assert.equal(contextInput().value, automatic), { timeout: 5000 });
  await settled();
}
const switchTo = (engine: string, manual: boolean) => act(() => {
  form.setFieldsValue({ engine, ...(manual ? { command_mode: "manual" } : {}) });
});

for (const manual of [true, false]) {
  test(`switching to SGLang${manual ? " with a manual command" : ""} drops the displayed automatic context`, async () => {
    await openHost();
    const start = requests.length;
    switchTo("sglang", manual);
    await waitFor(() => assert.equal(requests.at(-1)?.engine, "sglang"), { timeout: 5000 });
    await settled();
    for (const request of requests.slice(start)) assert.equal(request.quick_params?.max_model_len, undefined);
    assert.equal(form.getFieldValue("max_model_len"), undefined);
    assert.equal(contextInput().value, "");
    // The full body a create would send carries no sentinel either.
    const body = buildPayload(form.getFieldsValue(true), profileOf("sglang")).payload;
    assert.equal((body.quick_params as Record<string, unknown> | undefined)?.max_model_len, undefined);
  });
}

test("a context the deployer typed survives the switch to SGLang", async () => {
  await openHost();
  fireEvent.change(contextInput(), { target: { value: "16384" } });
  fireEvent.blur(contextInput());
  await waitFor(() => assert.equal(requests.at(-1)?.quick_params?.max_model_len, 16384), { timeout: 5000 });
  await settled();
  switchTo("sglang", true);
  await waitFor(() => assert.equal(requests.at(-1)?.engine, "sglang"), { timeout: 5000 });
  await settled();
  assert.equal(requests.at(-1)?.quick_params?.max_model_len, 16384);
  assert.equal(form.getFieldValue("max_model_len"), 16384);
});

test("a context the deployer set back to automatic is kept across the switch", async () => {
  // An explicit -1 is the deployer's choice; the server judges it for SGLang.
  await openHost();
  fireEvent.change(contextInput(), { target: { value: "8192" } });
  fireEvent.blur(contextInput());
  await waitFor(() => assert.equal(requests.at(-1)?.quick_params?.max_model_len, 8192), { timeout: 5000 });
  fireEvent.click(screen.getByRole("button", { name: automatic }));
  await waitFor(() => assert.equal(requests.at(-1)?.quick_params?.max_model_len, -1), { timeout: 5000 });
  await settled();
  switchTo("sglang", false);
  await waitFor(() => assert.equal(requests.at(-1)?.engine, "sglang"), { timeout: 5000 });
  assert.equal(requests.at(-1)?.quick_params?.max_model_len, -1);
  assert.equal(form.getFieldValue("max_model_len"), -1);
});
