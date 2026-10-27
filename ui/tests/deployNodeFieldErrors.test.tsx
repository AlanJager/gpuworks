// API field errors belong beside the mounted node control (docs/reference/api.md).
import { dom, createI18n } from "./helpers/dom.ts";
import { fakeFleet } from "./helpers/fakeFleet.ts";
import { engineProfiles } from "./helpers/engineFixture.ts";
import assert from "node:assert/strict";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
const { render, cleanup, screen, waitFor, act } = await import("@testing-library/react");
const { Form } = await import("antd");
const { api } = await import("../src/api.ts");
const Resource = (await import("../src/pages/deploy/DeployResourceStep.tsx")).default;
const Summary = (await import("../src/pages/deploy/DeployStepSummary.tsx")).default;
const Submit = (await import("../src/pages/deploy/DeploySubmitBar.tsx")).default;
const useEvaluation = (await import("../src/pages/deploy/useDeployEvaluation.ts")).default;
const { deployDraftPayload } = await import("../src/pages/deploy/deployHelpers.ts");
const { i18n, locale } = await createI18n(["common", "deploy", "nodes", "images"]);
const fixture = JSON.parse(fs.readFileSync(new URL("./fixtures/evaluate/distributed_node_count.json", import.meta.url), "utf8"));
const profile = engineProfiles.find(row => row.name === "vllm")!;
const wire = fixture.request;
const { quick_params, container_overrides, placement_mode, dist_role, ...identity } = wire;
// Reconstruct the fixed Form inputs only; the actual serializer builds all
// requests. The response's literal error/code/params is never regenerated.
const initial = { ...identity, ...quick_params, extra_args: "", node_selection_mode: "auto",
  container_env_rows: Object.entries(container_overrides.env).map(([key, value]) => ({ key, value })) };
const originalApi = { evaluate: api.evaluate, nodes: api.nodes, nodesPage: api.nodesPage, node: api.node, nodeFacets: api.nodeFacets };
let form: import("antd").FormInstance;
let evaluation: ReturnType<typeof useEvaluation>;
const requests: unknown[] = [];
const unknownRequests: string[] = [];
const originalFetch = globalThis.fetch;
afterEach(() => { cleanup(); Object.assign(api, originalApi); globalThis.fetch = originalFetch; });
after(() => dom.window.close());
function Host() {
  const [instance] = Form.useForm();
  form = instance;
  const values = Form.useWatch([], { form: instance, preserve: true }) || {};
  evaluation = useEvaluation({ form: instance, signal: values,
    buildPayload: () => deployDraftPayload(instance.getFieldsValue(true), [], profile, ["text"]),
    translate: (key, options) => i18n.t(key, options) });
  // Exact DeployPage.tsx parent seam (not an alternative attribution rule).
  const disabled = evaluation.evaluating || !!evaluation.error || !evaluation.result ||
    !instance.getFieldValue("model_id") || !instance.getFieldValue("target_vendor");
  return <I18nextProvider i18n={i18n}><Form form={instance} initialValues={initial}>
    <Resource profile={profile} eligibleNodes={evaluation.result?.eligible_nodes} result={evaluation.result} />
    <Summary result={evaluation.result} error={evaluation.error} loading={evaluation.evaluating} onReevaluate={evaluation.reevaluate} />
    <Submit submit={() => { throw new Error("characterization must not submit"); }} submitting={false} disabled={disabled} />
  </Form></I18nextProvider>;
}
function snapshot() {
  const control = document.querySelector<HTMLInputElement>("#distributed_nodes")!;
  const field = control.closest(".ant-form-item")!;
  const button = screen.getByRole("button", { name: locale.deploy.page.deploy }) as HTMLButtonElement;
  return { visibleControlCount: document.querySelectorAll("#distributed_nodes").length,
    getFieldInstancePresent: form.getFieldInstance("distributed_nodes") !== undefined,
    fieldErrors: form.getFieldError("distributed_nodes"), globalError: evaluation.error,
    evaluating: evaluation.evaluating, responseSeq: evaluation.responseSeq,
    ariaInvalid: control.getAttribute("aria-invalid"), ariaDescribedby: control.getAttribute("aria-describedby"),
    ariaRequired: control.getAttribute("aria-required"),
    fieldErrorText: field.querySelector(".ant-form-item-explain-error")?.textContent ?? null,
    globalAlertText: document.querySelector(".deploy-step-summary .ant-alert-error .ant-alert-message")?.textContent ?? null,
    submitDisabled: button.disabled, requestCount: requests.length };
}
test("a distributed node error reaches its real field, accessible feedback and focus handle", async () => {
  const uuid = fixture.response.eligible_nodes[0].uuid;
  const fleet = fakeFleet([{ uuid, name: "node-contract-1", hostname: "node-contract-1", state: "online",
    management_address: "127.0.0.1", gpu_count: 1, gpu_vendor: "nvidia", gpus: [] }]);
  Object.assign(api, { nodes: fleet.nodes, nodesPage: fleet.nodesPage, node: fleet.node, nodeFacets: fleet.nodeFacets });
  globalThis.fetch = async (input) => { unknownRequests.push(String(input)); throw new Error("unexpected management API request"); };
  api.evaluate = async (payload) => {
    const actual = JSON.parse(JSON.stringify(payload));
    assert.deepEqual(actual, wire, "strict literal request, produced by real serializer");
    requests.push(actual);
    return structuredClone(fixture.response);
  };
  assert.deepEqual(fixture.response.errors, [{ field: "node_uuids", code: "DISTRIBUTED_NODE_COUNT", params: { actual: 0, required: 2 } }]);
  render(<Host />);
  const samples: ReturnType<typeof snapshot>[] = [];
  for (let index = 0; index < 3; index++) {
    const previous = index ? evaluation.responseSeq : 0;
    if (index) act(() => evaluation.reevaluate());
    await waitFor(() => {
      assert.ok(evaluation.responseSeq > previous);
      assert.equal(evaluation.evaluating, false);
      assert.deepEqual(evaluation.result?.errors, fixture.response.errors);
      assert.equal(document.querySelectorAll("#distributed_nodes").length, 1);
    });
    samples.push(snapshot());
  }
  const observed = samples.at(-1)!;
  const expected = locale.common.apiError.DISTRIBUTED_NODE_COUNT
    .replace("{{required}}", "2").replace("{{actual}}", "0");
  assert.equal(observed.getFieldInstancePresent, true, "the actual Select ref reaches Form");
  assert.deepEqual(observed.fieldErrors, [expected], "the literal API error routes to distributed_nodes");
  assert.equal(observed.globalError, null, "a mounted node field is not a global error");
  assert.equal(observed.globalAlertText, null);
  assert.equal(observed.fieldErrorText, expected);
  assert.equal(observed.submitDisabled, false, "settled field feedback is not a successful-submit claim");
  const control = document.querySelector<HTMLInputElement>("#distributed_nodes")!;
  assert.equal(control.getAttribute("aria-invalid"), "true");
  assert.equal(control.getAttribute("aria-required"), "true");
  const descriptionIds = control.getAttribute("aria-describedby")!.split(/\s+/);
  assert.ok(descriptionIds.includes("distributed_nodes_help"));
  for (const id of descriptionIds) assert.ok(document.getElementById(id), "description ID resolves");
  assert.ok(document.getElementById("distributed_nodes_help")!.textContent!.includes(expected));
  const select = form.getFieldInstance("distributed_nodes");
  act(() => select.focus());
  assert.equal(document.activeElement, control, "Form can focus the real native combobox");
  assert.deepEqual(unknownRequests, []);
});
