import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";

// The server refuses a served name another deployment holds, so the form
// never proposes a bare model name: it proposes the model name with a random
// "-abcd" suffix, and shows a refusal at the field it belongs to.

const { i18n, locale } = await createI18n(["deploy"], "zh");
const { cleanup, render, screen, waitFor, act } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { Form, Input, message } = await import("antd");
const DeployServedNameField = (await import("../src/pages/deploy/DeployServedNameField.tsx")).default;
const { default: useDeploymentIdentity, servedNameSuffix } = await import("../src/pages/deploy/useDeploymentIdentity.ts");
const { default: useDeploySubmit } = await import("../src/pages/deploy/useDeploySubmit.ts");
const { api } = await import("../src/api.ts");
const { APIError } = await import("../src/apiError.ts");
type FormInstance = import("antd").FormInstance;
type EvaluateResponse = import("../src/api.ts").EvaluateResponse;

const apiOriginal = { ...api };
afterEach(() => {
  cleanup();
  message.destroy();
  Object.assign(api, apiOriginal);
});
after(() => { dom.window.close(); });

const servedNameLabel = locale.deploy.basic.servedName as string;

test("the suffix is a dash and four lowercase letters or digits", () => {
  assert.equal(servedNameSuffix(() => 0), "-aaaa");
  assert.equal(servedNameSuffix(() => 0.999999), "-9999");
  for (let i = 0; i < 50; i++) assert.match(servedNameSuffix(), /^-[a-z0-9]{4}$/);
});

function Identity({ model, onForm }: { model: never; onForm?: (form: FormInstance) => void }) {
  const form = Form.useFormInstance();
  onForm?.(form);
  useDeploymentIdentity(form, model);
  return <DeployServedNameField model={model} />;
}

const view = (model: unknown, onForm?: (form: FormInstance) => void) => (
  <I18nextProvider i18n={i18n}>
    <Form>
      <Identity model={model as never} onForm={onForm} />
    </Form>
  </I18nextProvider>
);

test("the proposed served name is the model name plus one suffix, kept across model refreshes and model changes", async () => {
  const qwen = { id: 1, name: "Qwen/Qwen3-0.6B", source_id: "Qwen/Qwen3-0.6B" };
  const rendered = render(view(qwen));
  const input = screen.getByRole("textbox", { name: servedNameLabel }) as HTMLInputElement;
  await waitFor(() => assert.match(input.value, /^Qwen\/Qwen3-0\.6B-[a-z0-9]{4}$/));
  const proposed = input.value;
  const suffix = proposed.slice(-5);

  // The model list refetches: a new object for the same model keeps the name.
  rendered.rerender(view({ ...qwen }));
  assert.equal(input.value, proposed);

  // Another model: its name, the same suffix.
  rendered.rerender(view({ id: 2, name: "google/gemma-3-27b", source_id: "google/gemma-3-27b" }));
  await waitFor(() => assert.equal(input.value, `google/gemma-3-27b${suffix}`));
});

test("a served name the deployer typed is never replaced by a proposal", async () => {
  const rendered = render(view({ id: 1, name: "org/qwen", source_id: "org/qwen" }));
  const input = screen.getByRole("textbox", { name: servedNameLabel }) as HTMLInputElement;
  await waitFor(() => assert.match(input.value, /^org\/qwen-[a-z0-9]{4}$/));
  const user = userEvent.setup({ document: dom.window.document });
  await user.clear(input);
  await user.type(input, "my-model");
  rendered.rerender(view({ id: 2, name: "org/other", source_id: "org/other" }));
  assert.equal(input.value, "my-model");
});

const evaluated = {
  ok: true, effective_params: {}, warnings: [], template_deviated: false, argv: [], image: "img",
  selected_node: { uuid: "n-1", name: "gpu-1" },
  plan: { ok: true, message: { code: "ok" }, plan: { gpus: [0], tp: 1 } },
} as unknown as EvaluateResponse;

function SubmitHarness() {
  const [form] = Form.useForm();
  const { submit } = useDeploySubmit({ form, nodes: [], evalErr: null, evaluating: false, onCreated: () => {}, evalResult: evaluated });
  return (
    <Form form={form} initialValues={{ model_id: 3, name: "demo", served_name: "qwen3-0.6b", node_selection_mode: "auto", nnodes: 1 }}>
      <DeployServedNameField />
      <button type="button" onClick={() => void submit()}>deploy</button>
    </Form>
  );
}

test("a served name another deployment holds is refused at the field, in the reader's language", async () => {
  const sent: Array<Record<string, unknown>> = [];
  api.createDeployment = (async (body: Record<string, unknown>) => {
    sent.push(body);
    throw new APIError("SERVED_NAME_IN_USE", { served_name: "qwen3-0.6b", deployment: "pd-qwen", deployment_id: 7 }, 'served name "qwen3-0.6b" is already used by deployment pd-qwen (id 7)', 409);
  }) as never;
  render(<I18nextProvider i18n={i18n}><SubmitHarness /></I18nextProvider>);
  await act(async () => { screen.getByRole("button", { name: "deploy" }).click(); });
  await waitFor(() => assert.equal(sent.length, 1));
  assert.equal(sent[0].served_name, "qwen3-0.6b");
  const expected = "对外模型名 qwen3-0.6b 已被部署 pd-qwen 使用，请换一个名字。";
  const fieldError = await waitFor(() => {
    const node = document.querySelector(".ant-form-item-explain-error");
    assert.ok(node, "the field shows the refusal");
    return node as HTMLElement;
  });
  assert.equal(fieldError.textContent, expected);
  assert.ok(screen.getByRole("textbox", { name: servedNameLabel }).closest(".ant-form-item-has-error"), "the served name field is the one marked");
  // The toast carries the same localized words, not the English fallback.
  await waitFor(() => assert.ok([...document.querySelectorAll(".ant-message-notice")].some((n) => n.textContent?.includes(expected))));
});

test("any other create failure leaves the served name field alone", async () => {
  api.createDeployment = (async () => { throw new APIError("DEPLOYMENT_NEVER_FITS", {}, "never fits", 400); }) as never;
  render(<I18nextProvider i18n={i18n}><SubmitHarness /></I18nextProvider>);
  await act(async () => { screen.getByRole("button", { name: "deploy" }).click(); });
  await waitFor(() => assert.ok(document.querySelector(".ant-message-notice")));
  assert.equal(document.querySelector(".ant-form-item-explain-error"), null);
});

function NameIdentity({ model }: { model: never }) {
  const form = Form.useFormInstance();
  useDeploymentIdentity(form, model);
  return (
    <Form.Item name="name" label="name">
      <Input />
    </Form.Item>
  );
}

const nameView = (model: unknown) => (
  <I18nextProvider i18n={i18n}>
    <Form>
      <NameIdentity model={model as never} />
    </Form>
  </I18nextProvider>
);

test("a deployment name the deployer cleared is never refilled by a proposal", async () => {
  const qwen = { id: 1, name: "Qwen/Qwen3-0.6B", source_id: "Qwen/Qwen3-0.6B" };
  const rendered = render(nameView(qwen));
  const input = screen.getByRole("textbox", { name: "name" }) as HTMLInputElement;
  // An untouched empty field still gets the proposal.
  await waitFor(() => assert.equal(input.value, "qwen3-0-6b"));
  const user = userEvent.setup({ document: dom.window.document });
  await user.clear(input);
  assert.equal(input.value, "");
  // The model list polling returns a new object for the same model: the
  // cleared field must stay empty, so a blank submit still fails validation.
  rendered.rerender(nameView({ ...qwen }));
  await act(async () => {});
  assert.equal(input.value, "");
  // Switching models does not refill it either: clearing was deliberate.
  rendered.rerender(nameView({ id: 2, name: "org/other", source_id: "org/other" }));
  await act(async () => {});
  assert.equal(input.value, "");
});
