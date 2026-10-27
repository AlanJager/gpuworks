import { dom, createI18n } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React, { useState } from "react";
import { I18nextProvider } from "react-i18next";
import type { AcquisitionResult } from "../src/api";

const { render, cleanup, screen, fireEvent, waitFor } = await import("@testing-library/react");
const { Form } = await import("antd");
const Fields = (await import("../src/pages/deploy/DeployWeightSourceFields.tsx")).default;
const { buildPayload } = await import("../src/pages/deploy/deployHelpers.ts");
const { i18n, locale } = await createI18n(["deploy"]);
afterEach(cleanup);
after(() => dom.window.close());

// Minimal facts from real evaluate response in readiness run
// 20261026T134103Z-23d9029a: A=2 is recommended/default; person picks B=3.
const acquisition: AcquisitionResult = {
  targets: [
    { key: "storage:2", label: "store-a", reason: "writable storage", recommended: true,
      storage_id: 2, storage_type: "local", default: true },
    { key: "storage:3", label: "store-b", reason: "writable storage", recommended: false,
      storage_id: 3, storage_type: "local" },
  ],
  providers: [
    { key: "public:huggingface:org/weights:pinned", label: "huggingface",
      reason: "configured model provider", recommended: true },
  ],
};

function Host({ choices = acquisition }: { choices?: AcquisitionResult }) {
  const [form] = Form.useForm();
  const [expert, setExpert] = useState(false);
  const [submitted, setSubmitted] = useState<Record<string, unknown>[]>([]);
  return <Form form={form} initialValues={{ model_id: 1, engine: "sglang" }}
    onFinish={() => setSubmitted(previous => [...previous, buildPayload(form.getFieldsValue(true)).payload])}>
    <button type="button" onClick={() => setExpert(!expert)}>{expert ? "Quick" : "Expert"}</button>
    {/* DeployPage conditionally mounts QuickForm; expert has no weight row. */}
    {!expert && <Fields acquisition={choices} modelSource="huggingface" />}
    <button type="submit">Create request</button>
    {submitted.length > 0 && <output data-testid="submitted">{JSON.stringify(submitted)}</output>}
  </Form>;
}

const host = (choices = acquisition) => <I18nextProvider i18n={i18n}><Host choices={choices} /></I18nextProvider>;
const mount = (choices = acquisition) => render(host(choices));
const storageLabel = (name: string) => `${name} (${locale.common.storageType.local})`;
async function chooseB() {
  fireEvent.mouseDown(screen.getByRole("combobox", { name: locale.deploy.workbench.acquisition.saveTo }));
  fireEvent.click(await screen.findByText(storageLabel("store-b"), { selector: ".ant-select-item-option-content span" }));
  await screen.findByText(storageLabel("store-b"), { selector: ".ant-select-selection-item" });
}
async function submit() {
  const before = JSON.parse(screen.queryByTestId("submitted")?.textContent || "[]").length;
  fireEvent.click(screen.getByRole("button", { name: "Create request" }));
  await waitFor(() => assert.equal(JSON.parse(screen.getByTestId("submitted").textContent!).length, before + 1));
  return JSON.parse(screen.getByTestId("submitted").textContent!).at(-1);
}

test("explicit save-to B survives the actual quick/expert unmount and request projection", async () => {
  mount();
  await screen.findByText(storageLabel("store-a"), { selector: ".ant-select-selection-item" });
  await chooseB();
  assert.deepEqual((await submit()).model_source, { provider: "huggingface", storage_id: 3 }, "user really chose B before switching");
  fireEvent.click(screen.getByRole("button", { name: "Expert" }));
  assert.equal(screen.queryByRole("combobox", { name: locale.deploy.workbench.acquisition.saveTo }), null);
  fireEvent.click(screen.getByRole("button", { name: "Quick" }));
  await screen.findByRole("combobox", { name: locale.deploy.workbench.acquisition.saveTo });
  assert.deepEqual((await submit()).model_source, { provider: "huggingface", storage_id: 3 }, "explicit storage B must not become recommended storage A after quick remount");
});


const changedTargets: AcquisitionResult = {
  ...acquisition,
  targets: [
    { key: "storage:3", label: "store-b", reason: "writable storage", recommended: true,
      storage_id: 3, storage_type: "local" },
    { key: "storage:4", label: "store-c", reason: "writable storage",
      storage_id: 4, storage_type: "local" },
  ],
};

test("an automatic storage that disappears follows the new recommendation", async () => {
  const view = mount();
  await screen.findByText(storageLabel("store-a"), { selector: ".ant-select-selection-item" });
  view.rerender(host(changedTargets));
  await screen.findByText(storageLabel("store-b"), { selector: ".ant-select-selection-item" });
  assert.deepEqual((await submit()).model_source, { provider: "huggingface", storage_id: 3 });
});

test("a user's now-unavailable storage stays explicit and displays the stale-choice error", async () => {
  const view = mount();
  await screen.findByText(storageLabel("store-a"), { selector: ".ant-select-selection-item" });
  await chooseB();
  view.rerender(host({ ...acquisition, targets: [acquisition.targets![0], changedTargets.targets![1]] }));
  assert.equal((await screen.findByRole("alert")).textContent, locale.deploy.workbench.acquisition.storageGone);
  assert.deepEqual((await submit()).model_source, { provider: "huggingface", storage_id: 3 });
});

test("a preserved automatic storage still follows changed candidates after quick remount", async () => {
  const view = mount();
  await screen.findByText(storageLabel("store-a"), { selector: ".ant-select-selection-item" });
  fireEvent.click(screen.getByRole("button", { name: "Expert" }));
  view.rerender(host(changedTargets));
  fireEvent.click(screen.getByRole("button", { name: "Quick" }));
  await screen.findByText(storageLabel("store-b"), { selector: ".ant-select-selection-item" });
  assert.deepEqual((await submit()).model_source, { provider: "huggingface", storage_id: 3 });
  view.rerender(host({ ...acquisition, targets: [acquisition.targets![0], changedTargets.targets![1]] }));
  await screen.findByText(storageLabel("store-a"), { selector: ".ant-select-selection-item" });
  assert.deepEqual((await submit()).model_source, { provider: "huggingface", storage_id: 2 }, "the automatic remount choice was not reclassified as explicit");
});
