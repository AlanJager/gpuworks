import assert from "node:assert/strict";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

// ADR 0020: the launch configuration has one representation. The server
// materializes it and reports what it did — which dictionary flags it moved out
// of the argument box, what the selected catalog spec contributed, and which
// key two sources disagree about. These cases prove the form follows that
// report instead of parsing arguments or judging values of its own.

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
  fs.readFileSync(
    new URL("../src/i18n/locales/en/deploy.json", import.meta.url),
    "utf8",
  ),
);
const commonLocale = JSON.parse(
  fs.readFileSync(
    new URL("../src/i18n/locales/en/common.json", import.meta.url),
    "utf8",
  ),
);
const engineProfiles = JSON.parse(
  fs.readFileSync(new URL("./fixtures/engines.json", import.meta.url), "utf8"),
) as Array<{ name: string }>;
const i18n = createInstance();
// The page resolves product error codes through the common namespace, the way
// the application does.
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: { en: { deploy: deployLocale, common: commonLocale } },
  defaultNS: "common",
  interpolation: { escapeValue: false },
});
const { act, cleanup, fireEvent, render, screen, waitFor } =
  await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { Form } = await import("antd");
type FormInstance = import("antd").FormInstance;
const DeployMovedFlagsNotice = (
  await import("../src/pages/deploy/DeployMovedFlagsNotice.tsx")
).default;
const DeployParamField = (
  await import("../src/pages/deploy/DeployParamField.tsx")
).default;
const DeployQuickForm = (
  await import("../src/pages/deploy/DeployQuickForm.tsx")
).default;
const DeployPerformanceStep = (
  await import("../src/pages/deploy/DeployPerformanceStep.tsx")
).default;
const DeployStepSummary = (
  await import("../src/pages/deploy/DeployStepSummary.tsx")
).default;
const { applyEvaluateFormValues, buildPayload } =
  await import("../src/pages/deploy/deployHelpers.ts");
const { apiErrorText } = await import("../src/apiError.ts");
const { APIError } = await import("../src/apiError.ts");

const maxModelLenSpec = {
  key: "max_model_len",
  flag: "--max-model-len",
  type: "integer",
  control: "number",
  group: "model",
  automatic: true,
  label: { en: "Maximum context", zh: "最大上下文" },
} as never;
const maxNumSeqsSpec = {
  key: "max_num_seqs",
  flag: "--max-num-seqs",
  type: "integer",
  control: "number",
  group: "performance",
  label: { en: "Max sequences", zh: "最大并发序列数" },
} as never;
const kvCacheDtypeSpec = {
  key: "kv_cache_dtype",
  flag: "--kv-cache-dtype",
  type: "enum",
  control: "select",
  group: "performance",
  enum: ["auto", "fp8"],
  label: { en: "KV cache precision", zh: "KV 缓存精度" },
} as never;
const quickProfile = {
  name: "vllm",
  sleep: { supported: true },
  quick_form_keys: ["max_model_len", "max_num_seqs", "kv_cache_dtype"],
  param_specs: [maxModelLenSpec, maxNumSeqsSpec, kvCacheDtypeSpec],
} as never;
const prefixCachingSpec = {
  key: "enable_prefix_caching",
  flag: "--enable-prefix-caching",
  flag_off: "--no-enable-prefix-caching",
  type: "boolean",
  control: "switch",
  group: "performance",
  default: true,
  label: { en: "Prefix caching", zh: "前缀缓存" },
} as never;
const cacheReportSpec = {
  key: "enable_cache_report",
  flag: "--enable-prompt-tokens-details",
  type: "boolean",
  control: "switch",
  group: "performance",
  default: true,
  label: { en: "Cache hit reporting", zh: "缓存命中上报" },
} as never;
const enforceEagerSpec = {
  key: "enforce_eager",
  flag: "--enforce-eager",
  type: "boolean",
  control: "switch",
  group: "performance",
  default: false,
  label: { en: "Enforce eager", zh: "禁用 CUDA Graph" },
} as never;
const switchProfile = {
  name: "vllm",
  sleep: { supported: true },
  quick_form_keys: [
    "enable_prefix_caching",
    "enable_cache_report",
    "enforce_eager",
  ],
  param_specs: [prefixCachingSpec, cacheReportSpec, enforceEagerSpec],
} as never;
const model = {
  id: 1,
  name: "org/qwen",
  source_id: "org/qwen",
  input_modalities: ["text"],
} as never;
const breakdown = {
  weight_bytes: 8 * 1024 ** 3,
  kv_bytes_per_token: 2048,
  kv_total_bytes: 0,
  reserve_bytes: 1536 * 1024 ** 2,
  mm_encoder_bytes: 0,
  per_gpu_bytes: 10 * 1024 ** 3,
  startup_fixed_bytes: 10 * 1024 ** 3,
  total_bytes: 10 * 1024 ** 3,
  grade: "measured",
  tensor_parallel: 1,
  max_model_len: 4096,
  capacity_bytes: 14 * 1024 ** 3,
  kv_pool_bytes: 4 * 1024 ** 3,
  estimated_kv_tokens: 2_000_000,
  kv_estimate_confidence: "measured",
  fits: true,
} as never;

afterEach(cleanup);

function quickForm(
  initialValues: Record<string, unknown>,
  extra?: React.ReactNode,
  capture?: (form: FormInstance) => void,
  result: unknown = null,
  profile: unknown = quickProfile,
) {
  function Host() {
    const [instance] = Form.useForm();
    capture?.(instance);
    return (
      <Form form={instance} initialValues={initialValues}>
        <DeployQuickForm
          templates={[]}
          nodes={[]}
          selectedModel={model}
          profile={profile as never}
          engine="vllm"
          images={[]}
          result={result as never}
        />
        {extra}
      </Form>
    );
  }
  return render(
    <I18nextProvider i18n={i18n}>
      <Host />
    </I18nextProvider>,
  );
}

async function openAdvanced() {
  const user = userEvent.setup({ document: dom.window.document });
  await user.click(screen.getByText(deployLocale.workbench.advanced.title));
  return user;
}

// The KV Offload row reads its cards from the server's quick form whitelist:
// with the tier keys published, the memory card turns the tier on and the disk
// card unlocks behind it together with the companions the disk tier needs.
// The specs mirror internal/server/deploy/param_specs.go for these keys.
const memoryTierSpec = {
  key: "kv_offloading_size",
  flag: "--kv-offloading-size",
  type: "number",
  control: "tier",
  group: "performance",
  min: 1,
  step: 1,
  label: { en: "CPU KV offload size", zh: "CPU KV 卸载大小" },
} as never;
const diskTierSpec = {
  key: "kv_disk_offload",
  flag: "--kv-transfer-config",
  type: "boolean",
  control: "tier",
  group: "performance",
  default: false,
  depends_on: "kv_offloading_size",
  label: { en: "Disk KV offload", zh: "磁盘 KV 卸载" },
} as never;
const sharedDirSpec = {
  key: "shared_kv_dir",
  flag: "--kv-transfer-config",
  type: "string",
  control: "path",
  group: "performance",
  depends_on: "kv_disk_offload",
  label: { en: "Shared KV directory", zh: "共享 KV 目录" },
} as never;
const readThreadsSpec = {
  key: "kv_offload_read_threads",
  flag: "--kv-transfer-config",
  type: "integer",
  control: "number",
  group: "performance",
  default: 16,
  min: 1,
  step: 1,
  depends_on: "kv_disk_offload",
  label: { en: "Read threads", zh: "读线程" },
} as never;
const writeThreadsSpec = {
  key: "kv_offload_write_threads",
  flag: "--kv-transfer-config",
  type: "integer",
  control: "number",
  group: "performance",
  default: 16,
  min: 1,
  step: 1,
  depends_on: "kv_disk_offload",
  label: { en: "Write threads", zh: "写线程" },
} as never;
const diskLimitSpec = {
  key: "kv_disk_limit_gib",
  flag: "",
  type: "number",
  control: "number",
  group: "performance",
  automatic: true,
  min: 1,
  step: 1,
  depends_on: "kv_disk_offload",
  label: { en: "Disk cache limit", zh: "磁盘缓存上限" },
} as never;
const kvTierProfile = {
  name: "vllm",
  sleep: { supported: true },
  quick_form_keys: [
    "max_model_len",
    "kv_offloading_size",
    "kv_disk_offload",
    "shared_kv_dir",
    "kv_offload_read_threads",
    "kv_offload_write_threads",
    "kv_disk_limit_gib",
  ],
  param_specs: [
    maxModelLenSpec,
    memoryTierSpec,
    diskTierSpec,
    sharedDirSpec,
    readThreadsSpec,
    writeThreadsSpec,
    diskLimitSpec,
  ],
} as never;

// Absence is asserted through match counts, never by comparing a queried node
// against null: a rendered element carries React's fiber tree on itself, and an
// assertion that fails while holding one makes the runner's diff walk that
// whole graph until the process is killed.
const fieldCount = (label: string) => screen.queryAllByLabelText(label).length;
const cardFor = (title: string) =>
  screen
    .getAllByText(title)
    .map((element) => element.closest("button.deploy-choice-card"))
    .find(Boolean) as HTMLButtonElement;

// The boolean feature cards carry a small select. cardPosition reads the short
// name the closed control shows; pickCardPosition opens the card's select and
// picks an option. rc-select toggles on the selector's mousedown, the dropdown
// is portalled to the body, and the closed control shows the selected
// position's name too — so the open lands on .ant-select-selector and the
// option is located by its dropdown item container, not by text alone.
function cardPosition(cardLabel: string): string | undefined {
  const card = screen
    .getByText(cardLabel)
    .closest(".deploy-feature-card") as HTMLElement | null;
  return (
    card?.querySelector(".ant-select-selection-item")?.textContent ?? undefined
  );
}
function dropdownOption(optionLabel: string): HTMLElement {
  const option = screen
    .getAllByText(optionLabel)
    .find((element) => element.closest(".ant-select-item-option"));
  if (!option) throw new Error(`dropdown option not rendered: ${optionLabel}`);
  return option as HTMLElement;
}
function openSelect(selectRoot: HTMLElement) {
  fireEvent.mouseDown(
    (selectRoot.querySelector(".ant-select-selector") ??
      selectRoot) as HTMLElement,
  );
}
async function pickCardPosition(
  user: ReturnType<typeof userEvent.setup>,
  cardLabel: string,
  optionLabel: string,
) {
  const card = screen
    .getByText(cardLabel)
    .closest(".deploy-feature-card") as HTMLElement;
  openSelect(card.querySelector(".ant-select") as HTMLElement);
  await user.click(dropdownOption(optionLabel));
}

after(() => {
  closeChannels();
  dom.window.close();
});

test("the KV Offload cards turn the tiers on and submit them", async () => {
  let form: FormInstance | undefined;
  quickForm(
    { engine: "vllm", command_mode: "engine" },
    undefined,
    (instance) => {
      form = instance;
    },
    null,
    kvTierProfile,
  );
  const user = userEvent.setup({ document: dom.window.document });
  const memoryCard = cardFor(deployLocale.workbench.kv.memory);
  const diskCard = cardFor(deployLocale.workbench.kv.disk);
  // The card is live because the engine published the key; the row reads
  // "not provided by the current engine" only for keys the engine withheld.
  assert.equal(
    memoryCard.disabled,
    false,
    "a published memory tier key leaves its card clickable",
  );
  assert.equal(
    memoryCard.textContent?.includes(deployLocale.workbench.engineUnavailable),
    false,
  );
  assert.equal(
    diskCard.textContent?.includes(deployLocale.workbench.engineUnavailable),
    false,
  );
  assert.equal(
    diskCard.disabled,
    true,
    "the disk card waits for the memory tier",
  );
  assert.equal(
    diskCard.textContent?.includes(deployLocale.workbench.kv.memoryRequired),
    true,
  );
  assert.equal(fieldCount("CPU KV offload size"), 0);
  assert.equal(
    fieldCount("Shared KV directory"),
    0,
    "the disk companions stay out of the form until the tier asks for them",
  );

  memoryCard.focus();
  await user.keyboard("{Enter}");
  await waitFor(() => assert.equal(fieldCount("CPU KV offload size"), 1));
  assert.equal(
    form!.getFieldValue("kv_offloading_size"),
    undefined,
    "opening the card preserves an empty new draft",
  );
  assert.equal(
    cardFor(deployLocale.workbench.kv.disk).disabled,
    true,
    "an empty capacity cannot enable disk",
  );
  await user.type(screen.getByLabelText("CPU KV offload size"), "32");
  assert.deepEqual(
    buildPayload(form!.getFieldsValue(true), kvTierProfile).quick,
    { kv_offloading_size: 32 },
  );
  assert.equal(
    cardFor(deployLocale.workbench.kv.disk).disabled,
    false,
    "the memory tier unlocks the disk card",
  );

  cardFor(deployLocale.workbench.kv.disk).focus();
  await user.keyboard(" ");
  // The disk tier is one --kv-transfer-config blob, so its companions have to
  // be on screen with the tier rather than hidden in the advanced list.
  await waitFor(() => assert.equal(fieldCount("Shared KV directory"), 1));
  assert.equal(fieldCount("Read threads"), 1);
  assert.equal(fieldCount("Write threads"), 1);
  assert.equal(fieldCount("Disk cache limit"), 1);
  const kvRow = memoryCard.closest(".deploy-workbench-row") as HTMLElement;
  assert.equal(
    kvRow.contains(screen.getByLabelText("Shared KV directory")),
    true,
    "the directory sits under the tier cards, not in the advanced list",
  );
  await user.type(screen.getByLabelText("Shared KV directory"), "/mnt/kv");
  // The thread counts are left untouched, so they stay off the wire and the
  // engine keeps its own default; the tier itself and its directory travel.
  await waitFor(() =>
    assert.deepEqual(
      buildPayload(form!.getFieldsValue(true), kvTierProfile).quick,
      {
        kv_offloading_size: 32,
        kv_disk_offload: true,
        shared_kv_dir: "/mnt/kv",
      },
    ),
  );
  await user.clear(screen.getByLabelText("Read threads"));
  await user.type(screen.getByLabelText("Read threads"), "4");
  await user.clear(screen.getByLabelText("Write threads"));
  await user.type(screen.getByLabelText("Write threads"), "6");
  await user.type(screen.getByLabelText("Disk cache limit"), "96");
  await waitFor(() =>
    assert.deepEqual(
      buildPayload(form!.getFieldsValue(true), kvTierProfile).quick,
      {
        kv_offloading_size: 32,
        kv_disk_offload: true,
        shared_kv_dir: "/mnt/kv",
        kv_offload_read_threads: 4,
        kv_offload_write_threads: 6,
        kv_disk_limit_gib: 96,
      },
    ),
  );

  // Turning the tiers off drops every KV field from this request but retains
  // the shared Form draft so either deployment view can restore it exactly.
  await user.click(cardFor(deployLocale.workbench.kv.disabled));
  await waitFor(() => assert.equal(fieldCount("Shared KV directory"), 0));
  assert.equal(fieldCount("CPU KV offload size"), 0);
  assert.equal(form!.getFieldValue("kv_offloading_size"), 32);
  assert.equal(form!.getFieldValue("shared_kv_dir"), "/mnt/kv");
  assert.equal(form!.getFieldValue("kv_disk_limit_gib"), 96);
  const disabled = buildPayload(form!.getFieldsValue(true), kvTierProfile);
  assert.deepEqual(disabled.quick, {});
  assert.deepEqual(disabled.quickParamsUnset, [
    "kv_offloading_size",
    "kv_disk_offload",
    "shared_kv_dir",
    "kv_offload_read_threads",
    "kv_offload_write_threads",
    "kv_disk_limit_gib",
  ]);

  await user.click(cardFor(deployLocale.workbench.kv.memory));
  await waitFor(() => assert.equal(fieldCount("Shared KV directory"), 1));
  assert.equal(
    (screen.getByLabelText("CPU KV offload size") as HTMLInputElement).value,
    "32",
  );
  assert.equal(
    (screen.getByLabelText("Shared KV directory") as HTMLInputElement).value,
    "/mnt/kv",
  );
  assert.equal(
    (screen.getByLabelText("Read threads") as HTMLInputElement).value,
    "4",
  );
  assert.equal(
    (screen.getByLabelText("Write threads") as HTMLInputElement).value,
    "6",
  );
  assert.equal(
    (screen.getByLabelText("Disk cache limit") as HTMLInputElement).value,
    "96",
  );
});

test("Expert speculative controls project the real engine dictionary into the deployment request", async (t) => {
  for (const tc of [
    {
      engine: "vllm",
      algorithmLabel: "Draft Model",
      algorithm: "draft_model",
      draft: "/models/draft-vllm",
      tokens: "5",
      expected: {
        speculative_algorithm: "draft_model",
        speculative_draft_model: "/models/draft-vllm",
        speculative_tokens: 5,
      },
      absent: ["Speculative Steps", "EAGLE Top K"],
    },
    {
      engine: "sglang",
      algorithmLabel: "EAGLE 3",
      algorithm: "EAGLE3",
      draft: "/models/draft-sglang",
      tokens: "7",
      steps: "3",
      topK: "4",
      expected: {
        speculative_algorithm: "EAGLE3",
        speculative_draft_model: "/models/draft-sglang",
        speculative_tokens: 7,
        speculative_num_steps: 3,
        speculative_eagle_top_k: 4,
      },
      absent: [],
    },
  ] as const) {
    await t.test(tc.engine, async () => {
      const profile = engineProfiles.find((item) => item.name === tc.engine);
      assert.ok(profile, `missing ${tc.engine} from the real /engines fixture`);
      let form: FormInstance | undefined;
      function Host() {
        const [instance] = Form.useForm();
        form = instance;
        return (
          <Form
            form={instance}
            initialValues={{ engine: tc.engine, command_mode: "engine" }}
          >
            <DeployPerformanceStep
              profile={profile as never}
              engine={tc.engine}
              model={model}
            />
          </Form>
        );
      }
      render(
        <I18nextProvider i18n={i18n}>
          <Host />
        </I18nextProvider>,
      );
      const user = userEvent.setup({ document: dom.window.document });

      const algorithmItem = screen
        .getByText("Speculative Decoding Algorithm")
        .closest(".ant-form-item") as HTMLElement;
      openSelect(algorithmItem.querySelector(".ant-select") as HTMLElement);
      await user.click(dropdownOption(tc.algorithmLabel));
      await user.type(
        dom.window.document.getElementById(
          "speculative_draft_model",
        ) as HTMLInputElement,
        tc.draft,
      );
      await user.type(
        screen.getByLabelText("Speculative Tokens per Step"),
        tc.tokens,
      );
      if ("steps" in tc) {
        await user.type(screen.getByLabelText("Speculative Steps"), tc.steps);
        await user.type(screen.getByLabelText("EAGLE Top K"), tc.topK);
      }
      for (const label of tc.absent)
        assert.equal(
          screen.queryAllByLabelText(label).length,
          0,
          `${label} is not published for ${tc.engine}`,
        );

      const projection = buildPayload(
        form!.getFieldsValue(true),
        profile as never,
      );
      const speculative = Object.fromEntries(
        Object.entries(projection.quick).filter(([key]) =>
          key.startsWith("speculative_"),
        ),
      );
      assert.deepEqual(speculative, tc.expected);
      assert.deepEqual(
        projection.payload.quick_params,
        projection.quick,
        "the values visible in Expert are the exact quick_params request body",
      );
      assert.equal(projection.quick.speculative_algorithm, tc.algorithm);
      cleanup();
    });
  }
});

test("switching engine profiles filters the request while retaining the shared draft", async () => {
  let form: FormInstance | undefined;
  const vllmProfile = {
    name: "vllm",
    sleep: { supported: true },
    quick_form_keys: ["enable_prefix_caching", "kv_offloading_size"],
    param_specs: [prefixCachingSpec, memoryTierSpec],
  } as never;
  const sglangProfile = {
    name: "sglang",
    sleep: { supported: true },
    quick_form_keys: ["enforce_eager"],
    param_specs: [enforceEagerSpec],
  } as never;
  function Host() {
    const [instance] = Form.useForm();
    form = instance;
    const engine = Form.useWatch("engine", { form: instance }) || "vllm";
    return (
      <Form
        form={instance}
        initialValues={{
          engine: "vllm",
          command_mode: "engine",
          enable_prefix_caching: false,
        }}
      >
        <button
          type="button"
          onClick={() =>
            instance.setFieldValue(
              "engine",
              engine === "vllm" ? "sglang" : "vllm",
            )
          }
        >
          Switch engine
        </button>
        <DeployQuickForm
          templates={[]}
          nodes={[]}
          selectedModel={model}
          profile={engine === "vllm" ? vllmProfile : sglangProfile}
          engine={engine}
          images={[]}
          result={null}
          preflight={null}
          preflighting={false}
          runPreflight={() => {}}
        />
      </Form>
    );
  }
  render(
    <I18nextProvider i18n={i18n}>
      <Host />
    </I18nextProvider>,
  );
  const user = userEvent.setup({ document: dom.window.document });
  await user.click(cardFor(deployLocale.workbench.kv.memory));
  await user.type(screen.getByLabelText("CPU KV offload size"), "32");
  await pickCardPosition(
    user,
    "Prefix caching",
    deployLocale.workbench.triStateOn,
  );
  assert.deepEqual(
    buildPayload(form!.getFieldsValue(true), vllmProfile).quick,
    { kv_offloading_size: 32, enable_prefix_caching: true },
  );

  await user.click(screen.getByRole("button", { name: "Switch engine" }));
  assert.deepEqual(
    buildPayload(form!.getFieldsValue(true), sglangProfile).quick,
    {},
    "SGLang request projection excludes vLLM-only values; the untouched eager position is the server's default to state",
  );
  assert.equal(
    form!.getFieldValue("kv_offloading_size"),
    32,
    "the unsupported value remains in the shared draft",
  );
  assert.equal(form!.getFieldValue("enable_prefix_caching"), true);

  await user.click(screen.getByRole("button", { name: "Switch engine" }));
  assert.equal(
    (screen.getByLabelText("CPU KV offload size") as HTMLInputElement).value,
    "32",
  );
  assert.deepEqual(
    buildPayload(form!.getFieldsValue(true), vllmProfile).quick,
    { kv_offloading_size: 32, enable_prefix_caching: true },
  );
});

test("Quick and Expert share one KV draft across disable and restore", async () => {
  let form: FormInstance | undefined;
  function Host() {
    const [instance] = Form.useForm();
    const [expert, setExpert] = React.useState(false);
    form = instance;
    return (
      <Form
        form={instance}
        initialValues={{ engine: "vllm", command_mode: "engine" }}
      >
        <button type="button" onClick={() => setExpert((value) => !value)}>
          {expert ? "Show Quick" : "Show Expert"}
        </button>
        {expert ? (
          <DeployPerformanceStep
            profile={kvTierProfile}
            engine="vllm"
            model={model}
          />
        ) : (
          <DeployQuickForm
            templates={[]}
            nodes={[]}
            selectedModel={model}
            profile={kvTierProfile}
            engine="vllm"
            images={[]}
            result={null}
            preflight={null}
            preflighting={false}
            runPreflight={() => {}}
          />
        )}
      </Form>
    );
  }
  render(
    <I18nextProvider i18n={i18n}>
      <Host />
    </I18nextProvider>,
  );
  const user = userEvent.setup({ document: dom.window.document });

  await user.click(cardFor(deployLocale.workbench.kv.memory));
  await user.type(screen.getByLabelText("CPU KV offload size"), "40");
  await user.click(cardFor(deployLocale.workbench.kv.disk));
  await user.type(
    screen.getByLabelText("Shared KV directory"),
    "/mnt/cross-view",
  );
  await user.clear(screen.getByLabelText("Read threads"));
  await user.type(screen.getByLabelText("Read threads"), "5");
  await user.clear(screen.getByLabelText("Write threads"));
  await user.type(screen.getByLabelText("Write threads"), "7");
  await user.type(screen.getByLabelText("Disk cache limit"), "120");

  await user.click(screen.getByRole("button", { name: "Show Expert" }));
  await waitFor(() =>
    assert.equal(
      (screen.getByLabelText("CPU KV offload size") as HTMLInputElement).value,
      "40",
    ),
  );
  assert.equal(
    (screen.getByLabelText("Shared KV directory") as HTMLInputElement).value,
    "/mnt/cross-view",
  );
  assert.equal(
    (screen.getByLabelText("Read threads") as HTMLInputElement).value,
    "5",
  );
  assert.equal(
    (screen.getByLabelText("Write threads") as HTMLInputElement).value,
    "7",
  );
  assert.equal(
    (screen.getByLabelText("Disk cache limit") as HTMLInputElement).value,
    "120",
  );

  const [memorySwitch, diskSwitch] = Array.from(
    dom.window.document.querySelectorAll(".deploy-kv-tier [role=switch]"),
  ) as HTMLElement[];
  assert.equal(memorySwitch.getAttribute("aria-checked"), "true");
  assert.equal(diskSwitch.getAttribute("aria-checked"), "true");
  await user.click(memorySwitch);
  assert.equal(form!.getFieldValue("kv_offloading_size"), 40);
  assert.equal(form!.getFieldValue("shared_kv_dir"), "/mnt/cross-view");

  await user.click(screen.getByRole("button", { name: "Show Quick" }));
  await waitFor(() => assert.equal(fieldCount("CPU KV offload size"), 0));
  assert.equal(
    cardFor(deployLocale.workbench.kv.disabled).classList.contains("is-active"),
    true,
  );
  await user.click(cardFor(deployLocale.workbench.kv.memory));
  await waitFor(() => assert.equal(fieldCount("Shared KV directory"), 1));
  assert.equal(
    (screen.getByLabelText("CPU KV offload size") as HTMLInputElement).value,
    "40",
  );
  assert.equal(
    (screen.getByLabelText("Shared KV directory") as HTMLInputElement).value,
    "/mnt/cross-view",
  );
  assert.deepEqual(
    buildPayload(form!.getFieldsValue(true), kvTierProfile).quick,
    {
      kv_offloading_size: 40,
      kv_disk_offload: true,
      shared_kv_dir: "/mnt/cross-view",
      kv_offload_read_threads: 5,
      kv_offload_write_threads: 7,
      kv_disk_limit_gib: 120,
    },
  );
});

test("dictionary defaults render without mutating the draft and reach evaluation", async () => {
  // The shared request projection sends displayed defaults because the engine
  // launch assembler does not infer them from the dictionary.
  let form: FormInstance | undefined;
  quickForm(
    { engine: "vllm", command_mode: "engine" },
    undefined,
    (instance) => {
      form = instance;
    },
    null,
    switchProfile,
  );
  // The dictionary's defaults are the server's to state: the untouched draft
  // sends nothing, and the captured evaluation answers them in form_values.
  await waitFor(() =>
    assert.deepEqual(
      buildPayload(form!.getFieldsValue(true), switchProfile).quick,
      {},
    ),
  );
  assert.equal(form!.getFieldValue("enable_cache_report"), undefined);
  // The closed control shows the effective dictionary position.
  assert.equal(
    cardPosition("Cache hit reporting"),
    deployLocale.workbench.triStateOn,
  );
  assert.equal(
    cardPosition("Prefix caching"),
    deployLocale.workbench.triStateOn,
  );
  // Eager is a direct feature card and shares the same request projection.
  const user = userEvent.setup({ document: dom.window.document });
  assert.equal(
    cardPosition("Enforce eager"),
    deployLocale.workbench.triStateOff,
  );

  await pickCardPosition(
    user,
    "Cache hit reporting",
    deployLocale.workbench.triStateOff,
  );
  await waitFor(() =>
    assert.equal(
      cardPosition("Cache hit reporting"),
      deployLocale.workbench.triStateOff,
    ),
  );
  assert.deepEqual(
    buildPayload(form!.getFieldsValue(true), switchProfile).quick,
    { enable_cache_report: false },
    "turning a default-on position off submits false, never an absent key",
  );
  // A template's value arrives as the server's display instruction and wins
  // over the position the dictionary default put there.
  act(() => {
    form!.setFieldsValue({
      template_id: 9,
      template_spec_source: "upstream",
      template_spec_name: "latency",
    });
  });
  act(() => {
    applyEvaluateFormValues(
      form!,
      {
        form_values: {
          enable_prefix_caching: false,
          enable_cache_report: true,
        },
      } as never,
      [],
    );
  });
  await waitFor(() =>
    assert.equal(
      cardPosition("Prefix caching"),
      deployLocale.workbench.triStateOff,
    ),
  );
  assert.deepEqual(
    buildPayload(form!.getFieldsValue(true), switchProfile).quick,
    { enable_prefix_caching: false, enable_cache_report: true },
  );

  // Switching to a spec that says nothing about prefix caching drops the
  // previous spec's value; the select returns to the dictionary default
  // without filling the draft.
  act(() => {
    form!.setFieldValue("template_spec_name", "throughput");
  });
  act(() => {
    applyEvaluateFormValues(
      form!,
      { form_values: { enable_cache_report: true } } as never,
      ["enable_prefix_caching", "enable_cache_report"],
    );
  });
  await waitFor(() =>
    assert.equal(
      cardPosition("Prefix caching"),
      deployLocale.workbench.triStateOn,
    ),
  );
  assert.deepEqual(
    buildPayload(form!.getFieldsValue(true), switchProfile).quick,
    { enable_cache_report: true },
  );
});

// An engine-decided boolean carries no dictionary default, so the feature card
// offers a third position: auto leaves the key unstated and no flag is sent,
// which is what lets the engine keep — and re-evaluate — its own default (vLLM
// turns prefix caching off by itself for omni-class models). Only a position
// the deployer chose reaches the request.
test("a boolean without a dictionary default stays auto and submits nothing until chosen", async () => {
  const engineDecidedSpec = {
    key: "enable_prefix_caching",
    flag: "--enable-prefix-caching",
    flag_off: "--no-enable-prefix-caching",
    type: "boolean",
    control: "switch",
    group: "performance",
    label: { en: "Prefix caching", zh: "前缀缓存" },
  } as never;
  const engineDecidedProfile = {
    name: "vllm",
    sleep: { supported: true },
    quick_form_keys: ["enable_prefix_caching"],
    param_specs: [engineDecidedSpec],
  } as never;
  let form: FormInstance | undefined;
  quickForm(
    { engine: "vllm", command_mode: "engine" },
    undefined,
    (instance) => {
      form = instance;
    },
    null,
    engineDecidedProfile,
  );
  // Auto on mount: the closed select shows auto, no two-state switch is drawn,
  // the card is not lit, and the key stays off the wire.
  await waitFor(() =>
    assert.equal(
      cardPosition("Prefix caching"),
      deployLocale.workbench.triStateAuto,
    ),
  );
  assert.equal(screen.queryByRole("switch", { name: "Prefix caching" }), null);
  assert.equal(
    String(
      screen.getByText("Prefix caching").closest(".deploy-feature-card")
        ?.className,
    ).includes("is-active"),
    false,
  );
  await waitFor(() =>
    assert.deepEqual(
      buildPayload(form!.getFieldsValue(true), engineDecidedProfile).quick,
      {},
    ),
  );

  const user = userEvent.setup({ document: dom.window.document });
  await pickCardPosition(
    user,
    "Prefix caching",
    deployLocale.workbench.triStateOn,
  );
  await waitFor(() =>
    assert.equal(
      cardPosition("Prefix caching"),
      deployLocale.workbench.triStateOn,
    ),
  );
  await waitFor(() =>
    assert.deepEqual(
      buildPayload(form!.getFieldsValue(true), engineDecidedProfile).quick,
      { enable_prefix_caching: true },
    ),
  );

  await pickCardPosition(
    user,
    "Prefix caching",
    deployLocale.workbench.triStateOff,
  );
  await waitFor(() =>
    assert.deepEqual(
      buildPayload(form!.getFieldsValue(true), engineDecidedProfile).quick,
      { enable_prefix_caching: false },
    ),
  );

  await pickCardPosition(
    user,
    "Prefix caching",
    deployLocale.workbench.triStateAuto,
  );
  await waitFor(() =>
    assert.deepEqual(
      buildPayload(form!.getFieldsValue(true), engineDecidedProfile).quick,
      {},
    ),
  );
  // A template's value takes the card out of auto the way it takes a position
  // on a two-state switch, and a spec that says nothing about the key returns
  // the card to auto rather than inventing a value.
  act(() => {
    form!.setFieldsValue({
      template_id: 9,
      template_spec_source: "upstream",
      template_spec_name: "latency",
    });
  });
  act(() => {
    applyEvaluateFormValues(
      form!,
      { form_values: { enable_prefix_caching: true } } as never,
      [],
    );
  });
  await waitFor(() =>
    assert.deepEqual(
      buildPayload(form!.getFieldsValue(true), engineDecidedProfile).quick,
      { enable_prefix_caching: true },
    ),
  );
  act(() => {
    form!.setFieldValue("template_spec_name", "throughput");
  });
  act(() => {
    applyEvaluateFormValues(form!, { form_values: {} } as never, [
      "enable_prefix_caching",
    ]);
  });
  await waitFor(() =>
    assert.deepEqual(
      buildPayload(form!.getFieldsValue(true), engineDecidedProfile).quick,
      {},
    ),
  );
});

// The advanced list draws the same rule through DeployParamField: an
// engine-decided boolean never registers a form field (so "auto" is not read
// back as a value) and its control writes the form directly.
test("an advanced engine-decided boolean renders three positions and writes the form directly", async () => {
  const advancedSpec = {
    key: "enable_chunked_prefill",
    flag: "--enable-chunked-prefill",
    flag_off: "--no-enable-chunked-prefill",
    type: "boolean",
    control: "switch",
    group: "performance",
    label: { en: "Chunked prefill", zh: "分块预填充" },
  } as never;
  let form: FormInstance | undefined;
  function Host() {
    const [instance] = Form.useForm();
    form = instance;
    return (
      <Form form={instance}>
        <DeployParamField spec={advancedSpec} />
      </Form>
    );
  }
  render(
    <I18nextProvider i18n={i18n}>
      <Host />
    </I18nextProvider>,
  );
  const user = userEvent.setup({ document: dom.window.document });
  const fieldItem = () =>
    screen
      .getByText("Chunked prefill")
      .closest(".ant-form-item") as HTMLElement;
  const shown = () =>
    fieldItem().querySelector(".ant-select-selection-item")?.textContent;
  const pick = async (optionLabel: string) => {
    openSelect(fieldItem().querySelector(".ant-select") as HTMLElement);
    await user.click(dropdownOption(optionLabel));
  };
  // Auto is the selected position and the form holds no value for the key:
  // the control is the only writer, so "auto" can never be read back as one.
  assert.equal(
    shown(),
    deployLocale.workbench.triStateAuto,
    "auto is selected on mount",
  );
  assert.equal(form!.getFieldValue("enable_chunked_prefill"), undefined);
  await pick(deployLocale.workbench.triStateOn);
  assert.equal(form!.getFieldValue("enable_chunked_prefill"), true);
  await pick(deployLocale.workbench.triStateOff);
  assert.equal(form!.getFieldValue("enable_chunked_prefill"), false);
  await pick(deployLocale.workbench.triStateAuto);
  assert.equal(form!.getFieldValue("enable_chunked_prefill"), undefined);
});

test("a dictionary flag the server moved out of extra_args shows up in its field and leaves the box", async () => {
  let form: FormInstance | undefined;
  const moved = [
    {
      key: "max_model_len",
      flag: "--max-model-len",
      value: 8192,
      source: "extra_args",
    },
  ] as never;
  quickForm(
    {
      engine: "vllm",
      command_mode: "engine",
      extra_args: "--max-model-len 8192 --foo",
    },
    <DeployMovedFlagsNotice moved={moved} />,
    (instance) => {
      form = instance;
    },
  );
  // The server's display instruction carries both halves of the move: the
  // field's value and the argument text after the token left it.
  act(() => {
    applyEvaluateFormValues(
      form!,
      { form_values: { max_model_len: 8192, extra_args: "--foo" } } as never,
      [],
    );
  });
  const contextField = screen.getByRole("spinbutton", {
    name: "Maximum context",
  }) as HTMLInputElement;
  await waitFor(() =>
    assert.equal(
      contextField.value,
      "8192",
      "the moved value is shown in the field that owns it",
    ),
  );
  assert.ok(
    screen.getByText("Moved into parameter fields: max_model_len"),
    "the rewrite is announced, not silent",
  );
  const user = await openAdvanced();
  const argumentBox = (await screen.findByRole("textbox", {
    name: deployLocale.advanced.customArgs,
  })) as HTMLTextAreaElement;
  assert.equal(
    argumentBox.value,
    "--foo",
    "the tokens that expressed the moved flag are gone, the rest is untouched",
  );
  // The field, not the text, is what the deployment request carries.
  assert.deepEqual(
    buildPayload(form!.getFieldsValue(true), quickProfile).quick,
    { max_model_len: 8192 },
  );

  const dismiss = screen.getByRole("button", {
    name: deployLocale.launchConfig.movedDismiss,
  });
  dismiss.focus();
  await user.keyboard("[Enter]");
  await waitFor(() =>
    assert.equal(
      screen.queryByText("Moved into parameter fields: max_model_len"),
      null,
    ),
  );
  assert.equal(
    form!.getFieldValue("max_model_len"),
    8192,
    "dismissing the line keeps the value the server moved",
  );
});

test("the same moved report from a later evaluation is applied again", async () => {
  // Typing the flag back produces an identical report from the next
  // evaluation. Treating it as "already seen" would leave the token in the box
  // while its field already holds the value — two representations of one
  // setting, which is the thing ADR 0020 removes.
  let form: FormInstance | undefined;
  const moved = [
    {
      key: "max_model_len",
      flag: "--max-model-len",
      value: 8192,
      source: "extra_args",
    },
  ] as never;
  function Host({ seq }: { seq: number }) {
    const [instance] = Form.useForm();
    form = instance;
    return (
      <Form
        form={instance}
        initialValues={{
          engine: "vllm",
          command_mode: "engine",
          extra_args: "--max-model-len 8192 --foo",
        }}
      >
        <DeployQuickForm
          templates={[]}
          nodes={[]}
          selectedModel={model}
          profile={quickProfile}
          engine="vllm"
          images={[]}
          result={null}
          preflight={null}
          preflighting={false}
          runPreflight={() => {}}
        />
        <DeployMovedFlagsNotice moved={moved} responseSeq={seq} />
      </Form>
    );
  }
  const view = (seq: number) => (
    <I18nextProvider i18n={i18n}>
      <Host seq={seq} />
    </I18nextProvider>
  );
  render(view(1));
  const user = await openAdvanced();
  const argumentBox = (await screen.findByRole("textbox", {
    name: deployLocale.advanced.customArgs,
  })) as HTMLTextAreaElement;
  // The first response's display instruction already reflects the move.
  act(() => {
    applyEvaluateFormValues(
      form!,
      { form_values: { max_model_len: 8192, extra_args: "--foo" } } as never,
      [],
    );
  });
  await waitFor(() => assert.equal(argumentBox.value, "--foo"));

  await user.type(argumentBox, " --max-model-len 4096");
  await waitFor(() =>
    assert.equal(
      form!.getFieldValue("extra_args"),
      "--foo --max-model-len 4096",
    ),
  );
  // The next evaluation reports the same move again; its display instruction
  // is applied on every response, so the token leaves the box a second time.
  rendered2: {
    act(() => {
      applyEvaluateFormValues(
        form!,
        { form_values: { max_model_len: 8192, extra_args: "--foo" } } as never,
        ["max_model_len", "extra_args"],
      );
    });
  }
  await waitFor(() =>
    assert.equal(
      argumentBox.value,
      "--foo",
      "the second report is acted on, not skipped as a repeat",
    ),
  );
  assert.equal(form!.getFieldValue("max_model_len"), 8192);
});

test("a selected template's parameters are applied once, its residual arguments stay read-only, and a later edit survives", async () => {
  let form: FormInstance | undefined;
  const response = {
    form_values: { kv_cache_dtype: "fp8", max_num_seqs: 16, extra_args: "" },
    template_residual_args: ["--no-enable-flashinfer-autotune"],
  } as never;
  quickForm(
    {
      engine: "vllm",
      command_mode: "engine",
      extra_args: "",
      template_id: 7,
      template_spec_source: "upstream",
      template_spec_name: "fp8-latency",
    },
    undefined,
    (instance) => {
      form = instance;
    },
    response,
  );
  act(() => {
    applyEvaluateFormValues(form!, response, []);
  });
  const user = await openAdvanced();
  const sequences = (await screen.findByRole("spinbutton", {
    name: "Max sequences",
  })) as HTMLInputElement;
  await waitFor(() => assert.equal(sequences.value, "16"));
  assert.ok(
    screen.getByTitle("fp8"),
    "the precision the template asked for is on screen",
  );

  // The template's tokens outside the dictionary are shown, but they are the
  // template's: they stay out of the user's argument box, which the server
  // reads as user residual and would otherwise report as a deviation.
  const argumentBox = screen.getByRole("textbox", {
    name: deployLocale.advanced.customArgs,
  }) as HTMLTextAreaElement;
  assert.equal(
    argumentBox.value,
    "",
    "the argument box holds only the user's own tokens",
  );
  const templateArgs = screen
    .getByText(deployLocale.launchConfig.templateArgs)
    .closest(".deploy-template-args") as HTMLElement;
  assert.ok(
    templateArgs,
    "the template's arguments have their own block under the box",
  );
  assert.ok(
    templateArgs.textContent?.includes("--no-enable-flashinfer-autotune"),
    "the template's residual arguments are visible",
  );
  assert.ok(screen.getByText(deployLocale.launchConfig.templateArgsHint));
  assert.equal(
    templateArgs.querySelector("input, textarea"),
    null,
    "they are shown, not offered for editing",
  );
  assert.equal(
    buildPayload(form!.getFieldsValue(true), quickProfile).payload.extra_args,
    undefined,
  );

  await user.clear(sequences);
  await user.type(sequences, "32");
  await waitFor(() => assert.equal(form!.getFieldValue("max_num_seqs"), 32));
  // The next evaluation answers the draft the edit lives in, so its display
  // instruction echoes the edited value rather than throwing it away.
  act(() => {
    applyEvaluateFormValues(
      form!,
      {
        form_values: {
          kv_cache_dtype: "fp8",
          max_num_seqs: 32,
          extra_args: "",
        },
      } as never,
      ["kv_cache_dtype", "max_num_seqs", "extra_args"],
    );
  });
  assert.equal(
    form!.getFieldValue("max_num_seqs"),
    32,
    "an edit made after the template was applied survives",
  );
  assert.equal(
    (
      screen.getByRole("spinbutton", {
        name: "Max sequences",
      }) as HTMLInputElement
    ).value,
    "32",
  );

  // Selecting another spec answers with that spec's instruction instead, and a
  // key only the previous spec set is cleared rather than left behind.
  act(() => {
    form!.setFieldValue("template_spec_name", "bf16-throughput");
  });
  act(() => {
    applyEvaluateFormValues(
      form!,
      { form_values: { max_num_seqs: 64, extra_args: "" } } as never,
      ["kv_cache_dtype", "max_num_seqs", "extra_args"],
    );
  });
  assert.equal(
    form!.getFieldValue("kv_cache_dtype"),
    undefined,
    "the previous spec's precision does not survive the switch",
  );
  await waitFor(() =>
    assert.equal(
      (
        screen.getByRole("spinbutton", {
          name: "Max sequences",
        }) as HTMLInputElement
      ).value,
      "64",
    ),
  );
});

test("a launch configuration conflict is reported on the field the two sources disagree about", async () => {
  let form: FormInstance | undefined;
  function Host() {
    const [instance] = Form.useForm();
    form = instance;
    return (
      <Form
        form={instance}
        initialValues={{ engine: "vllm", max_model_len: 4096 }}
      >
        <DeployParamField spec={maxModelLenSpec} />
      </Form>
    );
  }
  render(
    <I18nextProvider i18n={i18n}>
      <Host />
    </I18nextProvider>,
  );
  // The evaluation answers 200 with the conflict as a field error; the hook
  // maps it beside the control exactly this way (useDeployEvaluation).
  const fieldError = {
    field: "max_model_len",
    code: "LAUNCH_CONFIG_CONFLICT",
    params: {
      key: "max_model_len",
      flag: "--max-model-len",
      field_value: 4096,
      extra_value: 8192,
    },
  };
  act(() => {
    form!.setFields([
      {
        name: fieldError.field,
        errors: [
          apiErrorText(
            (key: string, options?: Record<string, unknown>) =>
              i18n.t(key, options),
            new APIError(
              fieldError.code,
              fieldError.params,
              fieldError.code,
              200,
            ),
          ),
        ],
      },
    ]);
  });
  const expected =
    "The form sets max_model_len to 4096 while the extra arguments set --max-model-len to 8192. Keep only one of them.";
  await waitFor(() =>
    assert.ok(
      screen.getByText(expected),
      "both values are named, so the user can tell which one to drop",
    ),
  );
  const field = screen.getByText("Maximum context").closest(".ant-form-item");
  assert.ok(
    field?.textContent?.includes(expected),
    "the message sits next to the field it is about",
  );
  // The captured evaluate fixture carries the same shape for its own rules.
  const fieldErrorsFixture = JSON.parse(
    fs.readFileSync(
      new URL("./fixtures/evaluate/field_errors.json", import.meta.url),
      "utf8",
    ),
  ) as { errors?: Array<{ field: string; code: string }> };
  assert.equal(fieldErrorsFixture.errors?.length, 2);
  assert.deepEqual(
    fieldErrorsFixture.errors?.map((item) => item.code),
    ["REPLICAS_NEGATIVE", "MOUNT_TARGET_DUPLICATE"],
  );
  assert.equal(
    typeof fieldErrorsFixture.errors![0].field,
    "string",
    "every error names the control it belongs beside",
  );
});

test("a manual command greys out the dictionary fields and withdraws the estimate", async () => {
  let form: FormInstance | undefined;
  quickForm(
    { engine: "vllm", command_mode: "engine" },
    <DeployStepSummary
      result={{ ok: true, warnings: [], breakdown } as never}
      error={null}
      loading={false}
    />,
    (instance) => {
      form = instance;
    },
  );
  const contextField = screen.getByRole("spinbutton", {
    name: "Maximum context",
  });
  assert.equal((contextField as HTMLInputElement).disabled, false);
  assert.ok(
    screen.getByText(deployLocale.vram.title),
    "the estimate is shown while the form owns the configuration",
  );
  assert.equal(
    screen.queryByText(deployLocale.launchConfig.manualEstimate),
    null,
  );

  act(() => {
    form!.setFieldValue("command_mode", "manual");
  });
  await waitFor(() =>
    assert.equal(
      (
        screen.getByRole("spinbutton", {
          name: "Maximum context",
        }) as HTMLInputElement
      ).disabled,
      true,
    ),
  );
  assert.ok(
    screen.getByText(deployLocale.launchConfig.manualEstimate),
    "the estimate says it does not apply",
  );
  assert.equal(
    screen.queryByText(deployLocale.vram.title),
    null,
    "no numbers from a configuration the instance will not start with",
  );
  // Placement is not a dictionary parameter: it still comes from the form.
  assert.equal(
    (
      screen.getByRole("spinbutton", {
        name: /Tensor Parallelism TP/,
      }) as HTMLInputElement
    ).disabled,
    false,
  );

  act(() => {
    form!.setFieldValue("command_mode", "engine");
  });
  await waitFor(() =>
    assert.equal(
      (
        screen.getByRole("spinbutton", {
          name: "Maximum context",
        }) as HTMLInputElement
      ).disabled,
      false,
    ),
  );
  assert.ok(screen.getByText(deployLocale.vram.title));
  assert.equal(
    screen.queryByText(deployLocale.launchConfig.manualEstimate),
    null,
  );
});
