import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";


// Automatic placement can end without a node: the server answers ok with no
// selected_node and every candidate carrying the reason it was turned down.
// The scheduler card and the Deploy button must say that, with the reasons,
// instead of "no online node matches the chip" or "wait for re-evaluation".

const { i18n, locale } = await createI18n(["deploy", "nodes"], "en");
const { cleanup, render, renderHook, act, screen, waitFor } = await import("@testing-library/react");
const { Form, message } = await import("antd");
const NodeSection = (await import("../src/pages/deploy/DeployNodeSection.tsx")).default;
const { default: useDeploySubmit } = await import("../src/pages/deploy/useDeploySubmit.ts");
const { api } = await import("../src/api.ts");
const { installFleet } = await import("./helpers/fakeFleet.ts");
type EvaluateResponse = import("../src/api.ts").EvaluateResponse;

const apiOriginal = { ...api };
afterEach(() => {
  cleanup();
  message.destroy();
  Object.assign(api, apiOriginal);
});
after(() => { dom.window.close(); });

const deploy = locale.deploy as Record<string, any>;
const workbench = deploy.workbench.node;
// The form's own wording of a refusal wins over the scheduler's replica wording.
const schedule = { ...deploy.schedule, ...deploy.placementReason };

const a40 = { uuid: "n-a40", name: "a40-150g", state: "online", gpu_count: 2, gpus: [{ index: 0, name: "A40" }, { index: 1, name: "A40" }] };
const h100 = { uuid: "n-h100", name: "h100-1", state: "online", gpu_count: 8, gpus: [{ index: 0, name: "H100" }] };

const diskRefused = { node_uuid: a40.uuid, node_name: a40.name, ok: false, reason: { code: "insufficient_disk" }, plan: { ok: false, message: { code: "insufficient_disk" } } };
const planFailed = { node_uuid: "n-pf", node_name: "plan-broken", ok: false, reason: { code: "plan_failed", message: "engine image is not pullable" } };
const chipRefused = { node_uuid: "n-chip", node_name: "cpu-only", ok: false, reason: { code: "chip_mismatch", params: { predicate: "A100" } } };

const evaluated = (patch: Partial<EvaluateResponse>) => ({ ok: true, effective_params: {}, warnings: [], template_deviated: false, argv: [], image: "img", ...patch }) as EvaluateResponse;

async function renderCard(result: EvaluateResponse | null, known: Array<typeof a40> = []) {
  const fake = installFleet(api as never, [a40, h100] as never);
  const fleet = await fake.nodeFacets({ state: ["online"] });
  const knownNodes = new Map(known.map((node) => [node.uuid, node as never]));
  function Harness() {
    const [form] = Form.useForm();
    return (
      <Form form={form} initialValues={{ node_selection_mode: "auto", target_vendor: "nvidia" }}>
        <NodeSection fleet={fleet} knownNodes={knownNodes} selection={undefined as never} modelReady result={result} loading={false} onNodeMode={() => {}} />
      </Form>
    );
  }
  render(<I18nextProvider i18n={i18n}><Harness /></I18nextProvider>);
}

test("with no candidates at all the card keeps saying no online node matches the chip", async () => {
  await renderCard(evaluated({ candidates: [] }));
  assert.ok(screen.getByText(workbench.noCompatibleOnline));
  assert.ok(screen.queryByText(workbench.noNodeFits) === null);
});

test("with candidates all turned down the card lists each node with its reason", async () => {
  await renderCard(evaluated({ candidates: [chipRefused, diskRefused, planFailed] }));
  assert.ok(screen.getByText(workbench.noNodeFits));
  assert.ok(screen.queryByText(workbench.noCompatibleOnline) === null, "nodes did match; the misleading line is gone");
  const items = [...document.querySelectorAll(".deploy-node-refusals li")].map((li) => li.textContent);
  // Planned nodes list before the ones turned down at the eligibility pass.
  assert.deepEqual(items, [
    `a40-150g: ${schedule.insufficient_disk}`,
    `plan-broken: ${schedule.plan_failed} ${deploy.scheduleNote.plan_failed_detail.replace("{{detail}}", "engine image is not pullable")}`,
    `cpu-only: ${schedule.chip_mismatch.replace("{{predicate}}", "A100")}`,
  ]);
});

// A cordoned node is a reason of the draft, worded without the replica the
// scheduler's wording of the same code waits for.
test("a cordoned node is refused in the form's words, not a replica's", async () => {
  const cordoned = { node_uuid: "n-c", node_name: "cordoned-1", ok: false, reason: { code: "node_cordoned", params: { node: "cordoned-1" } } };
  await renderCard(evaluated({ candidates: [cordoned] }));
  const items = [...document.querySelectorAll(".deploy-node-refusals li")].map((li) => li.textContent);
  assert.deepEqual(items, [`cordoned-1: ${deploy.placementReason.node_cordoned}`]);
  assert.notEqual(deploy.placementReason.node_cordoned, deploy.schedule.node_cordoned);
});

test("a long list of refusals shows the first few and counts the rest", async () => {
  const many = Array.from({ length: 8 }, (_, index) => ({ ...diskRefused, node_uuid: `n${index}`, node_name: `node-${index}` }));
  await renderCard(evaluated({ candidates: many }));
  const items = [...document.querySelectorAll(".deploy-node-refusals li")].map((li) => li.textContent ?? "");
  assert.equal(items.length, 6);
  assert.equal(items[5], workbench.moreRefused.replace("{{count}}", "3"));
});

test("a selected node still renders as before, with no refusal list", async () => {
  await renderCard(
    evaluated({ selected_node: { uuid: a40.uuid, name: a40.name }, candidates: [{ node_uuid: a40.uuid, node_name: a40.name, ok: true }], plan: { ok: true, message: { code: "ok" }, plan: { gpus: [0], tp: 1 } as never } }),
    [a40],
  );
  assert.ok(screen.getByText(workbench.schedulerSelected));
  assert.ok(screen.getByText("a40-150g"));
  assert.ok(screen.queryByText(workbench.noNodeFits) === null);
  assert.ok(screen.queryByText(workbench.noCompatibleOnline) === null);
});

function mountSubmit(evalResult: EvaluateResponse, evaluating = false) {
  const created: unknown[] = [];
  api.createDeployment = (async (body: unknown) => { created.push(body); return { id: 1 }; }) as never;
  const hook = renderHook(() => {
    const [instance] = Form.useForm();
    const submit = useDeploySubmit({
      form: instance, nodes: [], evalErr: null, evaluating, onCreated: () => {}, evalResult,
    });
    return { instance, ...submit };
  }, { wrapper: ({ children }) => <I18nextProvider i18n={i18n}>{children}</I18nextProvider> });
  hook.result.current.instance.setFieldsValue({ model_id: 3, name: "demo", node_selection_mode: "auto", nnodes: 1 });
  const press = () => act(async () => { await hook.result.current.submit(); });
  return { created, press };
}

test("Deploy with every node turned down names the nodes and reasons and creates nothing", async () => {
  const { created, press } = mountSubmit(evaluated({ candidates: [diskRefused, planFailed] }));
  await press();
  assert.equal(created.length, 0);
  const text = (await screen.findByText(/No node can host this deployment now/)).textContent ?? "";
  assert.ok(text.includes(`a40-150g: ${schedule.insufficient_disk}`), text);
  assert.ok(text.includes("plan-broken"), text);
  assert.ok(screen.queryByText(deploy.catalog.evaluationStale) === null, "no re-evaluation is pending");
});

test("Deploy with no candidates says no node matches, not that a re-evaluation is due", async () => {
  const { created, press } = mountSubmit(evaluated({ candidates: [] }));
  await press();
  assert.equal(created.length, 0);
  assert.ok(await screen.findByText(deploy.catalog.noMatchingNode));
  assert.ok(screen.queryByText(deploy.catalog.evaluationStale) === null);
});

// The server did not evaluate the draft (its model was refused): that is a
// failed evaluation, not a fleet with no matching node.
test("Deploy on a draft the server could not evaluate says the evaluation failed", async () => {
  const { created, press } = mountSubmit(evaluated({ ok: false, candidates: [], warnings: [{ code: "model_not_found" }] as never }));
  await press();
  assert.equal(created.length, 0);
  assert.ok(await screen.findByText(deploy.action.evaluationFailed));
});

test("Deploy while an evaluation is still running keeps asking to wait", async () => {
  const { created, press } = mountSubmit(evaluated({ candidates: [diskRefused] }), true);
  await press();
  assert.equal(created.length, 0);
  assert.ok(await screen.findByText(deploy.action.evaluationPending));
});

test("Deploy with a selected node and plan creates on that node", async () => {
  const { created, press } = mountSubmit(evaluated({ selected_node: { uuid: a40.uuid, name: a40.name }, plan: { ok: true, message: { code: "ok" }, plan: { gpus: [0, 1], tp: 2 } as never } }));
  await press();
  await waitFor(() => assert.equal(created.length, 1));
  const body = created[0] as Record<string, any>;
  assert.equal(body.node_uuid, a40.uuid);
  assert.equal(body.placement_mode, "auto");
  assert.deepEqual(body.placement.gpus, [0, 1]);
});

// The manual node list disables a node the evaluation turned down and says why
// in the option's title, in the reader's words rather than as a raw code.
test("the manual node list titles a refused node with its localized reason", async () => {
  const EligibleNodePicker = (await import("../src/pages/deploy/EligibleNodePicker.tsx")).default;
  installFleet(api as never, [a40, h100] as never);
  render(
    <I18nextProvider i18n={i18n}>
      <Form>
        <Form.Item name="node_uuid">
          <EligibleNodePicker aria-label="Node" eligibleNodes={[{ uuid: a40.uuid, eligible: false, reasons: [{ code: "node_cordoned", params: { node: a40.name } }] }, { uuid: h100.uuid, eligible: true }] as never} />
        </Form.Item>
      </Form>
    </I18nextProvider>,
  );
  const { fireEvent } = await import("@testing-library/react");
  fireEvent.mouseDown(document.querySelector(".ant-select-selector") as HTMLElement);
  const option = await waitFor(() => {
    const found = [...document.querySelectorAll(".ant-select-item-option")].find((el) => el.textContent?.includes("a40-150g"));
    assert.ok(found, "the refused node is listed");
    return found as HTMLElement;
  });
  assert.equal(option.getAttribute("title"), deploy.placementReason.node_cordoned);
  assert.ok(option.className.includes("ant-select-item-option-disabled"));
});
