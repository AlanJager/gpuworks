// Run from ui/: node_modules/.bin/tsx --import ./tests/helpers/rtlTimeout.ts --import ./tests/helpers/reactDomOrder.ts --test tests/deployQuickNodeCount.test.tsx
import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";
import { fixturePdModes } from "./helpers/engineFixture.ts";

const { i18n, locale } = await createI18n(["deploy", "images", "nodes"]);
const { cleanup, render, screen, waitFor } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { Form } = await import("antd");
type FormInstance = import("antd").FormInstance;
const DeployQuickForm = (await import("../src/pages/deploy/DeployQuickForm.tsx")).default;
const { buildPayload } = await import("../src/pages/deploy/deployHelpers.ts");
afterEach(cleanup);
after(() => dom.window.close());

// No dictionary fields are needed at this node-count ownership seam. The
// real QuickForm, NodeSection, Form.Items and InputNumbers create every
// node-count control; the host copies no control markup or topology logic.
// Management APIs and fetch are not mocked.
const profile = {
  name: "vllm", sleep: { supported: false }, quick_form_keys: [], param_specs: [],
  pd_modes: fixturePdModes("vllm"),
} as React.ComponentProps<typeof DeployQuickForm>["profile"];
const model = { id: 1, name: "model", input_modalities: ["text"] } as never;
const w = locale.deploy.workbench;

function renderQuick(mode: "auto" | "multi" = "auto") {
  let current: FormInstance;
  function Host() {
    const [form] = Form.useForm();
    current = form;
    return (
      <Form form={form} initialValues={{
        engine: "vllm", image_mode: "auto", command_mode: "engine",
        target_vendor: "nvidia", node_selection_mode: mode,
        nnodes: mode === "multi" ? 2 : 1, replicas: 1,
      }}>
        <DeployQuickForm templates={[]} knownNodes={new Map()} selectedModel={model}
          profile={profile} engine="vllm" images={[]} result={null} loading={false} />
      </Form>
    );
  }
  const view = render(<I18nextProvider i18n={i18n}><Host /></I18nextProvider>);
  return { ...view, form: current!, payload: () => buildPayload(current!.getFieldsValue(true)).payload };
}

function visibleNodeCounts() {
  return screen.queryAllByRole("spinbutton").filter((control) => control.id === "nnodes") as HTMLInputElement[];
}

function topologyCard(title: string) {
  const card = screen.getByText(title, { exact: true }).closest("button");
  assert.ok(card, `topology caption ${title} belongs to a real button`);
  return card;
}

test("multi-node QuickForm exposes one uniquely labelled, bounded node-count input", () => {
  const { container, payload } = renderQuick("multi");
  const controls = visibleNodeCounts();
  const actual = controls.map((control) => ({
    id: control.id, value: control.value,
    min: control.getAttribute("aria-valuemin"), max: control.getAttribute("aria-valuemax"),
    step: control.getAttribute("step"), label: control.getAttribute("aria-label"),
    inNodeCardHeader: control.closest(".deploy-node-card-head") !== null,
  }));
  assert.equal(controls.length, 1, `expected one visible node-count input; actual=${JSON.stringify(actual)}`);
  const [control] = controls;
  assert.equal(container.querySelectorAll('[id="nnodes"]').length, 1, "node-count DOM id is unique");
  assert.equal(screen.getByLabelText(w.node.nodeCount), control, "visible Nodes caption names the one input");
  assert.equal(control.value, "2");
  assert.equal(control.getAttribute("aria-valuemin"), "2");
  assert.equal(control.getAttribute("aria-valuemax"), "16");
  assert.equal(control.getAttribute("step"), "1");
  assert.equal(payload().nnodes, 2);
});

test("real single-to-multi edits and return preserve numeric nnodes in buildPayload", async () => {
  const { form, payload } = renderQuick();
  const user = userEvent.setup({ document: dom.window.document });
  assert.equal(visibleNodeCounts().length, 0);
  assert.equal(form.getFieldValue("nnodes"), 1);
  assert.equal(payload().nnodes, 1);
  await user.click(topologyCard(w.node.multi));
  const bounded = visibleNodeCounts().filter((control) => control.getAttribute("aria-valuemin") === "2");
  assert.equal(bounded.length, 1, "the real node card owns its bounded input");
  await user.click(bounded[0]);
  await user.keyboard("{ArrowUp}");
  await waitFor(() => assert.equal(payload().nnodes, 3));
  assert.equal(form.getFieldValue("nnodes"), 3);
  assert.equal(typeof payload().nnodes, "number");
  await user.click(topologyCard(w.node.single));
  assert.equal(visibleNodeCounts().length, 0);
  assert.equal(form.getFieldValue("nnodes"), 1);
  assert.equal(payload().nnodes, 1);
  assert.equal(payload().replicas, 1);
});

test("real PD topology keeps ordinary nnodes one and projects its separate role counts", async () => {
  const { form, payload } = renderQuick();
  const user = userEvent.setup({ document: dom.window.document });
  await user.click(topologyCard(w.node.pd));
  await waitFor(() => assert.equal(form.getFieldValue("pd_mode"), "vllm-nixl"));
  assert.equal(visibleNodeCounts().length, 0);
  assert.equal(form.getFieldValue("nnodes"), 1);
  assert.equal(payload().nnodes, 1);
  assert.equal(payload().replicas, 1);
  assert.deepEqual(payload().placement, { pd: {
    mode: "vllm-nixl", fallback: "decode", kv_load_failure: "fail",
    roles: { decode: { tp: 1, units: [{}] }, prefill: { tp: 1, units: [{}] } },
  } });
  for (const role of ["decode", "prefill"]) {
    const name = locale.deploy.pd.tpAria.replace("{{role}}", locale.deploy.pd.role[role]);
    assert.equal((screen.getByRole("spinbutton", { name }) as HTMLInputElement).value, "1");
  }
});
