import assert from "node:assert/strict";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";

// The dGPU quota switch of the deploy form. An automatic draft has no node of
// its own until a placement fits, and a draft that only fits as a dGPU slice
// never fits before the quota is set: gating the switch on the server's
// selection kept it off exactly when every card was partly taken ("no node can
// host this deployment"). In automatic mode it is gated on what the eligible
// pool offers (pool_partition_capability) instead.

const { i18n, locale } = await createI18n(["deploy", "images"], "en");
const { cleanup, render, screen, fireEvent, waitFor } = await import("@testing-library/react");
const { Form } = await import("antd");
const { MemoryRouter } = await import("react-router-dom");
const DeployQuickForm = (await import("../src/pages/deploy/DeployQuickForm.tsx")).default;
const { draftPartitionCapability } = await import("../src/pages/deploy/deployHelpers.ts");
type EvaluateResponse = import("../src/api.ts").EvaluateResponse;
type Node = import("../src/api.ts").Node;

afterEach(cleanup);
after(() => { dom.window.close(); });

const fixture = (name: string) => JSON.parse(
  fs.readFileSync(new URL(`./fixtures/evaluate/${name}.json`, import.meta.url), "utf8"),
) as EvaluateResponse;
// Real handler answers (internal/server/api): every card held by a whole-GPU
// instance on a dGPU-ready node, and a pool with no dGPU backend at all.
const busyPool = fixture("auto_partition_pool");
const plainPool = fixture("auto_placement");
const resource = (locale.deploy as Record<string, any>).workbench.resource;

const nvidiaNode = {
  uuid: "n1", name: "n1", state: "online", gpu_count: 1, gpus: [],
  partition_capabilities: [{ backend: "nvidia-dgpu", display_name: "NVIDIA dGPU", vendor: "nvidia", maturity: "stable", status: "ready" }],
} as unknown as Node;

test("the fixture is the case the switch must survive: no selected node, a ready pool backend", () => {
  assert.equal(busyPool.selected_node, undefined);
  assert.equal(busyPool.pool_partition_capability?.status, "ready");
  assert.equal(plainPool.pool_partition_capability, undefined);
});

test("an automatic draft takes the pool's backend, never the selection's", () => {
  assert.equal(draftPartitionCapability("auto", undefined, busyPool)?.backend, "nvidia-dgpu");
  assert.equal(draftPartitionCapability("auto", undefined, plainPool), undefined);
  // A stale node in the form does not stand in for the pool.
  assert.equal(draftPartitionCapability("auto", nvidiaNode, plainPool), undefined);
});

test("a draft that names its node takes that node's backend", () => {
  assert.equal(draftPartitionCapability("manual", nvidiaNode, plainPool)?.backend, "nvidia-dgpu");
  assert.equal(draftPartitionCapability("manual", undefined, busyPool), undefined);
});

const profile = { name: "vllm", sleep: { supported: false }, quick_form_keys: [], param_specs: [] } as never;
const model = { id: 1, name: "org/model", source_id: "org/model", input_modalities: ["text"] } as never;

function renderQuick(mode: string, result: EvaluateResponse, node?: Node) {
  let formRef: import("antd").FormInstance | undefined;
  function Host() {
    const [form] = Form.useForm();
    formRef = form;
    return (
      <Form form={form} initialValues={{ engine: "vllm", image_mode: "auto", command_mode: "engine", node_selection_mode: mode, target_vendor: "nvidia" }}>
        <DeployQuickForm
          templates={[]}
          knownNodes={new Map()}
          selectedNode={node}
          partitionCapability={draftPartitionCapability(mode, node, result)}
          selectedModel={model}
          profile={profile}
          engine="vllm"
          images={[]}
          result={result}
          loading={false}
        />
      </Form>
    );
  }
  render(<MemoryRouter><I18nextProvider i18n={i18n}><Host /></I18nextProvider></MemoryRouter>);
  return () => formRef!;
}

function quotaCard(): HTMLButtonElement {
  const card = screen.getByText(resource.partition).closest("button");
  assert.ok(card, "the dGPU memory card is rendered");
  return card as HTMLButtonElement;
}

test("every card taken: the automatic quota switch is on offer and sets a quota", async () => {
  const form = renderQuick("auto", busyPool);
  const card = quotaCard();
  assert.equal(card.disabled, false);
  assert.ok(card.textContent?.includes(resource.fullGpu));
  fireEvent.click(card);
  await waitFor(() => assert.equal(form().getFieldValue("vram_limit_gib"), 16));
  assert.ok(quotaCard().textContent?.includes("16 GiB"));
});

test("a pool with no dGPU backend says so in the pool's words", () => {
  renderQuick("auto", plainPool);
  const card = quotaCard();
  assert.equal(card.disabled, true);
  assert.ok(card.textContent?.includes(resource.dgpuUnavailablePool));
});

test("a named node without a backend keeps the node's words", () => {
  renderQuick("manual", busyPool);
  const card = quotaCard();
  assert.equal(card.disabled, true);
  assert.ok(card.textContent?.includes(resource.dgpuUnavailable));
});
