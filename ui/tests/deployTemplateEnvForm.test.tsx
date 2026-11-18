import { dom, createI18n } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import { register } from "node:module";
import { pathToFileURL } from "node:url";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { fakeFleet } from "./helpers/fakeFleet.ts";
import { fakeModels } from "./helpers/fakeModels.ts";
import { list } from "./helpers/listPage.ts";
register(pathToFileURL(new URL("./deploySleepableCssHook.mjs", import.meta.url).pathname));
const { i18n, locale } = await createI18n(["deploy", "deployments", "models"]);
const { cleanup, render, screen, waitFor } = await import("@testing-library/react");
const { MemoryRouter } = await import("react-router-dom");
const userEvent = (await import("@testing-library/user-event")).default;
const { api } = await import("../src/api.ts");
const DeployPage = (await import("../src/pages/DeployPage.tsx")).default;
const originals = { ...api };
afterEach(() => { cleanup(); Object.assign(api, originals); });
after(() => dom.window.close());

// The actual create page follows a catalog link, confirms its registered
// weights and submits. Template expressions must never become user overrides.
test("a template chosen in the create form leaves expanded device env to the server", async () => {
  const model = { id: 3, name: "org/model", source: "huggingface", source_id: "org/model", input_modalities: ["text"], operations: [{ type: "openai.chat", endpoint: "/v1/chat/completions" }] };
  const node = { uuid: "node-a", hostname: "gpu-a", state: "online", gpu_count: 1, gpu_vendor: "nvidia", gpus: [{ index: 2, name: "NVIDIA H20", vendor: "nvidia", schedulable: true }] };
  const spec = { name: "device-env", source: "user", engine: "vllm", image: "example/vllm:test", scenario: "balanced", gpus: { vendor: "nvidia" }, env: { XPU_VISIBLE_DEVICES: "${LOCAL_VISIBLE_DEVICES}", MODEL_ROOT: "${MODEL_PATH}", CONSTANT: "one" }, weight_sources: [{ provider: "huggingface", repo_id: "org/model" }] };
  const template = { id: 7, model_id: "org/model", specs: [spec] };
  const fleet = fakeFleet([node] as never);
  const models = fakeModels([model] as never);
  for (const name of ["nodes", "nodesPage", "node", "nodeFacets"] as const) api[name] = fleet[name] as never;
  api.models = models.models_all;
  api.modelsPage = models.modelsPage;
  api.model = models.model;
  api.engines = async () => list([{ name: "vllm", sleep: { supported: false }, quick_form_keys: [], param_specs: [] }]) as never;
  api.inferenceTemplateIndex = async () => ({ items: [{ id: 7, model_id: "org/model", source: "user", engines: ["vllm"], scenarios: ["balanced"], profiles: [{ ...spec }] }] }) as never;
  api.inferenceTemplate = async () => template as never;
  api.templateWeightModels = async () => ({ models: [model], sources: [{ ...spec.weight_sources[0], reuse_model_id: 3 }] }) as never;
  api.images = async () => list([]);
  api.imageVersions = async () => list([]);
  api.me = async () => ({ role: "admin" }) as never;
  const evaluated: Record<string, any>[] = [];
  const created: Record<string, any>[] = [];
  api.evaluate = async payload => {
    evaluated.push(structuredClone(payload));
    // As the server's launch materialization does: expand the template first,
    // then apply explicit user env. An accidental copied expression overrides 2.
    const env = { XPU_VISIBLE_DEVICES: "2", MODEL_ROOT: "/model", CONSTANT: "one", ...(payload.container_overrides?.env ?? {}) };
    return { ok: true, warnings: [], defaults: { engine: "vllm", model_mount: "/model" }, form_values: {}, image: spec.image,
      command: ["vllm", "serve", "/model"], argv: [], entrypoint: [], env, injected: [], template_match: true,
      selected_node: { uuid: node.uuid, name: node.hostname, reasons: [] }, eligible_nodes: [{ uuid: node.uuid, name: node.hostname, eligible: true }],
      plan: { ok: true, message: { code: "selected" }, plan: { gpus: [2], tp: 1, per_gpu_bytes: 1, score: 1, reasons: [] }, explain: {} },
      acquisition: { image: { image: spec.image, cached: true } }, yield_policy_eligible: { ok: false, reason: "SLEEP_NOT_ENABLED" },
    } as never;
  };
  api.createDeployment = async body => { created.push(structuredClone(body)); return { id: 99 } as never; };
  const user = userEvent.setup({ document: dom.window.document });
  render(<MemoryRouter initialEntries={["/deploy?template=7&spec=device-env&spec_source=user"]}><I18nextProvider i18n={i18n}><DeployPage /></I18nextProvider></MemoryRouter>);
  await screen.findByRole("radio", { name: /org\/model/ });
  await user.click(screen.getByRole("button", { name: locale.deploy.weightSourceModal.useModel }));
  await user.type(await screen.findByPlaceholderText("my-qwen"), "template-env-form");
  await waitFor(() => assert.ok(evaluated.some(item => item.model_id === 3 && item.template_id === 7)));
  const current = () => evaluated.filter(item => item.model_id === 3 && item.template_id === 7).at(-1)!;
  assert.deepEqual(current().container_overrides?.env ?? {}, {}, "selecting a template must not copy its env into user overrides");
  await waitFor(() => assert.ok(screen.getByText(locale.deploy.workbench.node.schedulerSelected)));
  await user.click(screen.getByRole("button", { name: locale.deploy.page.deploy }));
  await waitFor(() => assert.equal(created.length, 1));
  assert.deepEqual(created[0].container_overrides?.env ?? {}, {});
  assert.equal(created[0].template_id, 7);
});
