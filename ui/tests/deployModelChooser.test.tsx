import assert from "node:assert/strict";
import fs from "node:fs";
import { after, afterEach, beforeEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";

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
const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: { en: { deploy: deployLocale } },
  defaultNS: "deploy",
  interpolation: { escapeValue: false },
});
const { act, cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { Form } = await import("antd");
const { MemoryRouter } = await import("react-router-dom");
const { api } = await import("../src/api.ts");
const useDeployPrefill = (await import("../src/pages/deploy/useDeployPrefill.ts")).default;
const useDeployFormChanges = (await import("../src/pages/deploy/useDeployFormChanges.ts")).default;
import { installModels } from "./helpers/fakeModels.ts";
type FormInstance = import("antd").FormInstance;
const DeployBasicStep = (
  await import("../src/pages/deploy/DeployBasicStep.tsx")
).default;
const { matchingProfiles } = await import(
  "../src/pages/deploy/TemplateRecommendations.tsx"
);

const models = [
  { id: 7, name: "Quantized A", source_id: "quant/a", source: "huggingface" },
] as never;
// The registry the picker searches: the selected model (7) is not on the
// first page, so its name comes from the page's own read of it.
const registry = Array.from({ length: 60 }, (_, i) => ({
  id: 100 + i, name: `listed-${String(i).padStart(2, "0")}`, source_id: `org/listed-${i}`, source: "modelscope",
}));
const index = [
  {
    id: 1,
    model_id: "org/a",
    display_title: "Model A",
    source: "upstream",
    engines: ["vllm"],
    scenarios: ["balanced"],
    profiles: [
      {
        name: "scheme-a",
        source: "upstream",
        engine: "vllm",
        scenario: "balanced",
        image: "image/a",
        weight_repo_ids: ["quant/a"],
        gpus: { vendor: "nvidia" },
        detected_compatible: true,
      },
      {
        name: "scheme-b",
        source: "modelscope",
        engine: "sglang",
        scenario: "balanced",
        image: "image/b",
        weight_repo_ids: ["quant/a"],
        gpus: { vendor: "nvidia" },
        detected_compatible: true,
      },
    ],
  },
] as never;

const originals = { models: api.models, modelsPage: api.modelsPage, model: api.model, inferenceTemplateIndex: api.inferenceTemplateIndex };
let fake: ReturnType<typeof installModels>;
beforeEach(() => {
  api.inferenceTemplateIndex = (async () => ({ items: index, hardware: [{ vendor: "nvidia", name: "H100" }] })) as never;
  fake = installModels(api as never, [...registry, ...(models as unknown as never[])]);
});
afterEach(() => {
  cleanup();
  Object.assign(api, originals);
});
after(() => dom.window.close());

function mount(initialValues: Record<string, unknown>) {
  let form: FormInstance | undefined;
  function Host() {
    const [instance] = Form.useForm();
    form = instance;
    return (
      <Form form={instance} initialValues={initialValues}>
        <DeployBasicStep
          templateIndex={index}
          chips={[{ vendor: "nvidia", name: "H100" }]}
          selectedModel={models[0]}
        />
      </Form>
    );
  }
  render(
    <I18nextProvider i18n={i18n}>
      <MemoryRouter>
        <Host />
      </MemoryRouter>
    </I18nextProvider>,
  );
  return () => form!;
}

test("model selection is shown directly with matching template recommendations", async () => {
  assert.equal(
    matchingProfiles(index, models[0], "balanced", "nvidia")
      .length,
    2,
  );
  mount({ model_id: 7, target_vendor: "nvidia", scenario: "balanced" });
  await waitFor(() => {
    assert.ok(screen.getByText("Quantized A"));
    assert.ok(screen.getByText(/scheme-a/));
    assert.ok(screen.getByText(/scheme-b/));
    assert.equal(screen.queryByRole("dialog") === null, true);
  });
});

test("selecting a recommendation writes the complete template identity", async () => {
  const form = mount({ model_id: 7, target_vendor: "nvidia", scenario: "balanced" });
  const user = userEvent.setup({ document: dom.window.document });
  const recommendation = await screen.findByRole("button", { name: /scheme-b/ });
  await user.click(recommendation);
  assert.deepEqual(
    [
      form().getFieldValue("template_id"),
      form().getFieldValue("template_spec_source"),
      form().getFieldValue("template_spec_name"),
      form().getFieldValue("engine"),
      form().getFieldValue("target_vendor"),
      form().getFieldValue("image"),
    ],
    [1, "modelscope", "scheme-b", "sglang", "nvidia", "image/b"],
  );
});

const modelSelect = () => screen.getAllByRole("combobox")[0];

test("the model picker searches the server instead of listing every model", async () => {
  mount({ model_id: 7, target_vendor: "nvidia" });
  await waitFor(() => assert.deepEqual(fake.pages[0], { page: 1, page_size: 50, q: "" }));
  assert.ok(screen.getByText("Quantized A"), "the chosen model off the first page keeps its name");
  fireEvent.mouseDown(modelSelect());
  fireEvent.change(modelSelect(), { target: { value: "listed-5" } });
  await waitFor(() => assert.equal(fake.pages.at(-1)?.q, "listed-5"));
  await screen.findByText("listed-50");
  const listbox = document.querySelector(".ant-select-dropdown") as HTMLElement;
  assert.equal(within(listbox).queryByText("listed-03") === null, true, "the server narrowed the options");
});

test("picking a model clears the template choice and what it wrote", async () => {
  const form = mount({ model_id: 7, target_vendor: "nvidia", template_id: 1, template_spec_source: "modelscope", template_spec_name: "scheme-b", engine: "sglang", image: "image/b" });
  await waitFor(() => assert.ok(fake.pages.length));
  fireEvent.mouseDown(modelSelect());
  fireEvent.click(await screen.findByText("listed-03"));
  assert.equal(form().getFieldValue("model_id"), 103);
  assert.deepEqual(
    [
      form().getFieldValue("template_id"),
      form().getFieldValue("template_spec_source"),
      form().getFieldValue("template_spec_name"),
      form().getFieldValue("engine"),
      form().getFieldValue("image"),
    ],
    [undefined, undefined, undefined, undefined, undefined],
  );
});

test("picking a model keeps the deployer's own engine and image when no template is chosen", async () => {
  const form = mount({ model_id: 7, target_vendor: "nvidia", engine: "sglang", image: "my/image" });
  await waitFor(() => assert.ok(fake.pages.length));
  fireEvent.mouseDown(modelSelect());
  fireEvent.click(await screen.findByText("listed-03"));
  assert.deepEqual(
    [form().getFieldValue("engine"), form().getFieldValue("image")],
    ["sglang", "my/image"],
  );
});

test("a vendor that drops the template drops its engine and image too", () => {
  const values: Record<string, unknown> = {
    target_vendor: "hygon",
    template_id: 1,
    template_spec_source: "modelscope",
    template_spec_name: "scheme-b",
    engine: "sglang",
    image: "image/b",
    node_uuid: "n1",
  };
  const form = {
    getFieldValue: (name: string) => values[name],
    setFieldValue: (name: string, value: unknown) => {
      values[name] = value;
    },
    setFieldsValue: (next: Record<string, unknown>) => Object.assign(values, next),
  } as unknown as FormInstance;
  useDeployFormChanges({ form, templates: index, images: [] })({ target_vendor: "hygon" });
  assert.deepEqual(
    [values.template_id, values.template_spec_name, values.engine, values.image, values.node_uuid],
    [undefined, undefined, undefined, undefined, undefined],
  );
});

test("with no registered model the picker links to registering one", async () => {
  fake.models.length = 0;
  mount({});
  await waitFor(() => assert.ok(fake.pages.length));
  fireEvent.mouseDown(modelSelect());
  const link = await screen.findByRole("link", { name: deployLocale.basic.registerModel });
  assert.equal(link.getAttribute("href"), "/models");
});

function prefill(queryModelID: number, initial: Record<string, unknown> = {}) {
  let form: FormInstance | undefined;
  function Host() {
    const [instance] = Form.useForm();
    form = instance;
    useDeployPrefill({ form: instance, queryModelID, nodeSelectionMode: "manual" });
    return <Form form={instance} initialValues={initial} />;
  }
  render(<Host />);
  return () => form!;
}

test("?model= is filled once the model is read to exist, and not when it is gone (R9)", async () => {
  const known = prefill(7);
  await waitFor(() => assert.equal(known().getFieldValue("model_id"), 7));
  cleanup();
  const gone = prefill(999);
  await waitFor(() => assert.ok(fake.reads.includes(999)));
  await act(() => new Promise((resolve) => setTimeout(resolve, 20)));
  assert.equal(gone().getFieldValue("model_id"), undefined);
  cleanup();
  const chosen = prefill(7, { model_id: 103 });
  await act(() => new Promise((resolve) => setTimeout(resolve, 20)));
  assert.equal(chosen().getFieldValue("model_id"), 103, "a model already on the form stays");
});

test("a cleared engine falls back to the server default again", async () => {
  let form: FormInstance | undefined;
  function Host() {
    const [instance] = Form.useForm();
    form = instance;
    useDeployPrefill({ form: instance, nodeSelectionMode: "manual", defaultsEngine: "vllm" });
    return <Form form={instance} initialValues={{ engine: "sglang" }} />;
  }
  render(<Host />);
  await act(() => new Promise((resolve) => setTimeout(resolve, 20)));
  assert.equal(form!.getFieldValue("engine"), "sglang", "an engine on the form stays");
  act(() => form!.setFieldsValue({ engine: undefined }));
  await waitFor(() => assert.equal(form!.getFieldValue("engine"), "vllm"));
});

test("same-name registrations expose full revision and ready file locations before keyboard selection", async () => {
  const downloadedRevision = "release-b-1234567890abcdef1234567890abcdef12345678";
  fake.models.splice(0, fake.models.length,
    { id: 901, name: "Same registered name", source: "huggingface", source_id: "org/same", revision: "release-a", presence: { ready_nodes: 0, ready_storages: 0 } } as never,
    { id: 902, name: "Same registered name", source: "huggingface", source_id: "org/same", revision: downloadedRevision, presence: { ready_nodes: 2, ready_storages: 1 } } as never,
  );
  const form = mount({ target_vendor: "nvidia" });
  fireEvent.mouseDown(modelSelect());
  await screen.findAllByText("Same registered name");
  const dropdown = document.querySelector(".ant-select-dropdown") as HTMLElement;
  assert.ok(within(dropdown).queryByText(`Revision: ${downloadedRevision}`), "full revision must distinguish otherwise identical options");
  assert.ok(within(dropdown).getByText("2 node(s) · 1 shared storage(s)"));
  assert.ok(within(dropdown).getByText(deployLocale.weightSourceModal.noFiles));
  assert.ok(dropdown.querySelector(`[title="${downloadedRevision}"]`), "long revision remains available as a full title");
  const user = userEvent.setup({ document: dom.window.document });
  await user.type(modelSelect(), downloadedRevision);
  await waitFor(() => assert.equal(fake.pages.at(-1)?.q, downloadedRevision));
  await waitFor(() => assert.equal(within(dropdown).queryAllByText("Revision: release-a").length, 0));
  await within(dropdown).findByText(`Revision: ${downloadedRevision}`);
  // rc-select reads the browser's numeric key codes as well as key names.
  fireEvent.keyDown(modelSelect(), { key: "ArrowDown", code: "ArrowDown", keyCode: 40, which: 40 });
  fireEvent.keyDown(modelSelect(), { key: "Enter", code: "Enter", keyCode: 13, which: 13 });
  await waitFor(() => assert.equal(form().getFieldValue("model_id"), 902));
  const selection = modelSelect().closest(".ant-select")!.querySelector(".ant-select-selection-item") as HTMLElement;
  assert.ok(within(selection).getByText("Same registered name"));
  assert.ok(within(selection).getByText(`Revision: ${downloadedRevision}`));
  assert.ok(selection.querySelector("[title]")!.getAttribute("title")!.includes("Same registered name"));
  assert.ok(selection.querySelector("[title]")!.getAttribute("title")!.includes(downloadedRevision), "the full revision remains available when the summary is ellipsized");
  assert.equal(within(selection).queryAllByText("2 node(s) · 1 shared storage(s)").length, 0, "file locations belong in the dropdown, not the single-line selection");
  assert.equal(fake.listedAll, 0);
  assert.deepEqual(fake.reads, [], "no per-option detail queries");
  assert.ok(fake.pages.every((query) => query.page_size === 50));
});

test("an off-page selected model without a summary reports unknown file location", async () => {
  mount({ model_id: 7, target_vendor: "nvidia" });
  await waitFor(() => assert.ok(fake.pages.length));
  const selection = modelSelect().closest(".ant-select")!.querySelector(".ant-select-selection-item") as HTMLElement;
  assert.ok(within(selection).getByText("Quantized A"));
  assert.ok(within(selection).getByText(deployLocale.weightSourceModal.revisionUnknown));
  assert.equal(within(selection).queryAllByText(deployLocale.weightSourceModal.locationUnknown).length, 0);
  fireEvent.mouseDown(modelSelect());
  const dropdown = document.querySelector(".ant-select-dropdown") as HTMLElement;
  assert.ok(within(dropdown).getByText(deployLocale.weightSourceModal.locationUnknown), "selected ModelCore fallback must not invent zero ready files");
  const selectedOption = within(dropdown).getByText("Quantized A").closest(".ant-select-item-option") as HTMLElement;
  assert.equal(within(selectedOption).queryAllByText(deployLocale.weightSourceModal.noFiles).length, 0);
});

test("selected fallback keeps unknown location while the page loads or fails", async () => {
  let rejectPage: (error: Error) => void = () => {};
  api.modelsPage = () => new Promise((_resolve, reject) => { rejectPage = reject; });
  mount({ model_id: 7, target_vendor: "nvidia" });
  fireEvent.mouseDown(modelSelect());
  const dropdown = document.querySelector(".ant-select-dropdown") as HTMLElement;
  assert.ok(within(dropdown).getByText(deployLocale.weightSourceModal.locationUnknown));
  await waitFor(() => assert.ok(modelSelect().closest(".ant-select-loading")));
  await act(async () => rejectPage(new Error("synthetic registry unavailable")));
  await waitFor(() => assert.equal(modelSelect().closest(".ant-select-loading"), null));
  const selection = modelSelect().closest(".ant-select")!.querySelector(".ant-select-selection-item") as HTMLElement;
  assert.ok(within(selection).getByText("Quantized A"));
  assert.ok(within(dropdown).getByText(deployLocale.weightSourceModal.locationUnknown));
  assert.equal(within(dropdown).queryAllByText(deployLocale.weightSourceModal.noFiles).length, 0);
  assert.deepEqual(fake.reads, []);
});

test("a selected registration keeps its full identity when a later search omits it", async () => {
  const revision = "release-1234567890abcdef1234567890abcdef12345678";
  fake.models[0] = { id: 100, name: "", source: "huggingface", source_id: "org/long-model-name", revision, presence: { ready_nodes: 1, ready_storages: 2 } } as never;
  const form = mount({ model_id: 100, target_vendor: "nvidia" });
  const select = modelSelect().closest(".ant-select")!;
  await waitFor(() => assert.ok(within(select as HTMLElement).getByText(`Revision: ${revision}`)));
  const selection = select.querySelector(".ant-select-selection-item") as HTMLElement;
  assert.ok(within(selection).getByText("org/long-model-name"));
  assert.ok(selection.querySelector("[title]")!.getAttribute("title")!.includes("org/long-model-name"));
  assert.ok(selection.querySelector("[title]")!.getAttribute("title")!.includes(revision));
  assert.equal(within(selection).queryAllByText("1 node(s) · 2 shared storage(s)").length, 0);
  fireEvent.mouseDown(modelSelect());
  fireEvent.change(modelSelect(), { target: { value: "listed-5" } });
  await waitFor(() => assert.equal(fake.pages.at(-1)?.q, "listed-5"));
  const dropdown = document.querySelector(".ant-select-dropdown") as HTMLElement;
  await within(dropdown).findByText("listed-50");
  assert.equal(within(dropdown).queryAllByText("org/long-model-name").length, 0);
  assert.ok(within(selection).getByText("org/long-model-name"));
  assert.ok(within(selection).getByText(`Revision: ${revision}`));
  assert.ok(selection.querySelector("[title]")!.getAttribute("title")!.includes(revision));
  assert.equal(form().getFieldValue("model_id"), 100);
});
