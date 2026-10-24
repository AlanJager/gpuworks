import assert from "node:assert/strict";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

const closeChannels = trackMessageChannels();
const dom = new JSDOM("<!doctype html><html><body></body></html>", {
  url: "http://localhost/",
});
for (const name of [
  "window",
  "document",
  "navigator",
  "HTMLElement",
  "HTMLInputElement",
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
const imagesLocale = JSON.parse(
  fs.readFileSync(
    new URL("../src/i18n/locales/en/images.json", import.meta.url),
    "utf8",
  ),
);
const commonLocale = JSON.parse(
  fs.readFileSync(
    new URL("../src/i18n/locales/en/common.json", import.meta.url),
    "utf8",
  ),
);
const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: {
    en: { deploy: deployLocale, images: imagesLocale, common: commonLocale },
  },
  defaultNS: "common",
  interpolation: { escapeValue: false },
});

const { cleanup, render, screen, waitFor } =
  await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { Form } = await import("antd");
type FormInstance = import("antd").FormInstance;
const DeployQuickForm = (
  await import("../src/pages/deploy/DeployQuickForm.tsx")
).default;
const DeployImageFields = (
  await import("../src/pages/deploy/DeployImageFields.tsx")
).default;
const { buildPayload } = await import("../src/pages/deploy/deployHelpers.ts");

afterEach(cleanup);
after(() => {
  closeChannels();
  dom.window.close();
});

const model = {
  id: 1,
  name: "org/model",
  source_id: "org/model",
  input_modalities: ["text"],
} as never;
const profile = {
  name: "vllm",
  sleep: { supported: false },
  quick_form_keys: [],
  param_specs: [],
} as never;

test("Quick and Expert expose one keyboard-editable runtime environment draft", async () => {
  let form: FormInstance | undefined;
  function Host() {
    const [instance] = Form.useForm();
    const [expert, setExpert] = React.useState(false);
    form = instance;
    return (
      <Form
        form={instance}
        initialValues={{
          engine: "vllm",
          image_mode: "auto",
          command_mode: "engine",
        }}
      >
        <button type="button" onClick={() => setExpert((value) => !value)}>
          {expert ? "Show Quick" : "Show Expert"}
        </button>
        {expert ? (
          <DeployImageFields
            runtimeOnly
            engines={{ data: [profile], loading: false }}
            models={{ data: [model] }}
            imageMode="auto"
            inferenceSeries={[]}
            imageSeriesLoading={false}
            catalogVersions={[]}
            autoResolutionText="automatic"
            selectedModel={model}
            compatibilityWarnings={[]}
            onlineNodes={[]}
          />
        ) : (
          <DeployQuickForm
            templates={[]}
            nodes={[]}
            selectedModel={model}
            profile={profile}
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

  assert.ok(
    screen.getByText(deployLocale.workbench.runtime.title),
    "the editor is visible without opening Advanced",
  );
  assert.equal(
    screen.queryByRole("textbox", { name: deployLocale.advanced.customArgs }),
    null,
    "Advanced stays collapsed",
  );

  const addEnvironment = screen.getByRole("button", {
    name: deployLocale.containerOverrides.addEnvironment,
  });
  addEnvironment.focus();
  await user.keyboard("{Enter}");
  await user.type(screen.getByPlaceholderText("KEY"), "CACHE_ROOT");
  await user.type(screen.getByPlaceholderText("VALUE"), "/srv/cache");

  const addMount = screen.getByRole("button", {
    name: deployLocale.containerOverrides.addMount,
  });
  addMount.focus();
  await user.keyboard("{Enter}");
  await user.type(
    screen.getByPlaceholderText(deployLocale.containerOverrides.hostPathPlaceholder),
    "/srv/data",
  );
  await user.type(
    screen.getByPlaceholderText(
      deployLocale.containerOverrides.containerPathPlaceholder,
    ),
    "/data",
  );
  assert.deepEqual(
    buildPayload(form!.getFieldsValue(true)).payload.container_overrides,
    {
      env: { CACHE_ROOT: "/srv/cache" },
      extra_mounts: [
        {
          host_path: "/srv/data",
          container_path: "/data",
          purpose: "data",
          read_only: false,
        },
      ],
    },
  );

  await user.click(screen.getByRole("button", { name: "Show Expert" }));
  await waitFor(() =>
    assert.equal(
      (screen.getByPlaceholderText("KEY") as HTMLInputElement).value,
      "CACHE_ROOT",
    ),
  );
  assert.equal(
    (screen.getByPlaceholderText("VALUE") as HTMLInputElement).value,
    "/srv/cache",
  );
  assert.equal(
    (
      screen.getByPlaceholderText(
        deployLocale.containerOverrides.hostPathPlaceholder,
      ) as HTMLInputElement
    ).value,
    "/srv/data",
  );
  assert.equal(
    (
      screen.getByPlaceholderText(
        deployLocale.containerOverrides.containerPathPlaceholder,
      ) as HTMLInputElement
    ).value,
    "/data",
  );

  const addSecondMount = screen.getByRole("button", {
    name: deployLocale.containerOverrides.addMount,
  });
  addSecondMount.focus();
  await user.keyboard(" ");
  const sources = screen.getAllByPlaceholderText(
    deployLocale.containerOverrides.hostPathPlaceholder,
  ) as HTMLInputElement[];
  const targets = screen.getAllByPlaceholderText(
    deployLocale.containerOverrides.containerPathPlaceholder,
  ) as HTMLInputElement[];
  await user.type(sources[1], "/srv/other");
  await user.type(targets[1], "/data");
  // Mount rules are the server's now: the duplicate target travels to
  // evaluate and comes back as the container_mount_rows field error, not a
  // client-side rejection. The rows themselves are what the draft stated.
  const duplicated = buildPayload(form!.getFieldsValue(true)).payload;
  assert.equal(
    (duplicated.container_overrides as Record<string, unknown>).extra_mounts
      ?.length,
    2,
    "the draft states both rows; the server answers the conflict",
  );
  await form!.validateFields();

  await user.clear(targets[1]);
  await user.type(targets[1], "/data/cache");

  await user.click(screen.getByRole("button", { name: "Show Quick" }));
  const quickTargets = screen.getAllByPlaceholderText(
    deployLocale.containerOverrides.containerPathPlaceholder,
  ) as HTMLInputElement[];
  assert.deepEqual(
    quickTargets.map((input) => input.value),
    ["/data", "/data/cache"],
    "both rows remain in the shared Form draft across views",
  );
});

test("a blank mount row is ignored while a half-filled row is rejected", async () => {
  let form: FormInstance | undefined;
  function Host() {
    const [instance] = Form.useForm();
    form = instance;
    return (
      <Form form={instance}>
        <DeployImageFields
          runtimeOnly
          engines={{ data: [profile], loading: false }}
          models={{ data: [model] }}
          imageMode="auto"
          inferenceSeries={[]}
          imageSeriesLoading={false}
          catalogVersions={[]}
          autoResolutionText="automatic"
          compatibilityWarnings={[]}
          onlineNodes={[]}
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
  await user.click(
    screen.getByRole("button", { name: deployLocale.containerOverrides.addMount }),
  );
  assert.equal(
    buildPayload(form!.getFieldsValue(true)).payload.container_overrides,
    undefined,
    "an empty editor row is not materialized",
  );
  await form!.validateFields();
  // A half-filled row is the one pure-UX rule kept client-side: it states
  // nothing and is not sent. The complete-row rules (duplicate, nested,
  // absolute paths) are the server's evaluate answer.
  await user.type(
    screen.getByPlaceholderText(deployLocale.containerOverrides.hostPathPlaceholder),
    "/srv/half",
  );
  assert.equal(
    buildPayload(form!.getFieldsValue(true)).payload.container_overrides,
    undefined,
    "a half-filled row is not sent for the server to misread",
  );
});

test("the start timeout field shows the cluster default and sends only a stated value", async () => {
  let form: FormInstance | undefined;
  function Host() {
    const [instance] = Form.useForm();
    form = instance;
    return (
      <Form form={instance} initialValues={{ engine: "vllm", command_mode: "engine" }}>
        <DeployQuickForm
          templates={[]}
          nodes={[]}
          selectedModel={model}
          profile={profile}
          startTimeoutDefaultSec={1800}
          engine="vllm"
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
  const field = screen.getByRole("spinbutton", {
    name: deployLocale.containerOverrides.startTimeout,
  }) as HTMLInputElement;
  assert.equal(field.value, "", "an untouched field states nothing");
  assert.equal(field.placeholder, "1800", "the cluster default is the placeholder");
  assert.ok(
    screen.getByText(
      deployLocale.containerOverrides.startTimeoutHelpDefault.replace("{{seconds}}", "1800"),
    ),
  );
  assert.equal(
    "start_timeout_sec" in buildPayload(form!.getFieldsValue(true)).payload,
    false,
    "the cluster default is the server's, not a value the form sends",
  );

  await user.type(field, "3600");
  await user.tab();
  const payload = buildPayload(form!.getFieldsValue(true)).payload;
  assert.equal(payload.start_timeout_sec, 3600);
  assert.equal(
    payload.container_overrides,
    undefined,
    "the start timeout is a deployment field, not a container override",
  );

  // A server-attributed field error lands beside the control.
  form!.setFields([
    { name: "start_timeout_sec", errors: ["start_timeout_sec must be 0 or between 60 and 86400 seconds"] },
  ]);
  await waitFor(() => assert.ok(screen.getByText(/between 60 and 86400 seconds/)));
});

test("the /dev/shm field shows the engine default and sends only a stated size", async () => {
  let form: FormInstance | undefined;
  const shmProfile = { ...(profile as object), shm_bytes: 16 * 2 ** 30 } as never;
  function Host() {
    const [instance] = Form.useForm();
    form = instance;
    return (
      <Form form={instance} initialValues={{ engine: "vllm", command_mode: "engine" }}>
        <DeployQuickForm
          templates={[]}
          nodes={[]}
          selectedModel={model}
          profile={shmProfile}
          engine="vllm"
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
  const shm = screen.getByRole("spinbutton", {
    name: deployLocale.containerOverrides.shm,
  }) as HTMLInputElement;
  assert.equal(shm.value, "", "an untouched field states nothing");
  assert.equal(shm.placeholder, "16", "the engine default is the placeholder");
  assert.ok(
    screen.getByText(
      deployLocale.containerOverrides.shmHelpDefault.replace("{{size}}", "16"),
    ),
  );
  assert.equal(
    buildPayload(form!.getFieldsValue(true)).payload.container_overrides,
    undefined,
    "the engine default is the server's, not a value the form sends",
  );

  await user.type(shm, "32");
  await user.tab();
  assert.deepEqual(
    buildPayload(form!.getFieldsValue(true)).payload.container_overrides,
    { shm_bytes: 32 * 2 ** 30 },
  );

  // A server-attributed field error lands beside the control.
  form!.setFields([
    {
      name: "container_shm_gib",
      errors: [
        commonLocale.apiError.SHM_SIZE_WITH_HOST_IPC.replaceAll(
          "{{arg}}",
          "--ipc=host",
        ),
      ],
    },
  ]);
  await waitFor(() =>
    assert.ok(screen.getByText(/has no effect with --ipc=host/)),
  );
});

// The profile's /dev/shm size is only the base: the server adds the CPU memory
// tier to it, so once an evaluation has resolved the launch the field states
// that size instead of promising the engine default.
test("the /dev/shm field shows the size the evaluated draft resolves to", async () => {
  const shmProfile = { ...(profile as object), shm_bytes: 16 * 2 ** 30 } as never;
  function Host({ result, loading }: { result: unknown; loading?: boolean }) {
    const [instance] = Form.useForm();
    return (
      <Form form={instance} initialValues={{ engine: "vllm", command_mode: "engine" }}>
        <DeployQuickForm
          templates={[]}
          nodes={[]}
          selectedModel={model}
          profile={shmProfile}
          engine="vllm"
          images={[]}
          result={result as never}
          loading={Boolean(loading)}
          preflight={null}
          preflighting={false}
          runPreflight={() => {}}
        />
      </Form>
    );
  }
  const shmField = () =>
    screen.getByRole("spinbutton", {
      name: deployLocale.containerOverrides.shm,
    }) as HTMLInputElement;

  // 16 GiB default + 19 GiB CPU tier.
  const tiered = { shm_bytes: 35 * 2 ** 30 };
  render(
    <I18nextProvider i18n={i18n}>
      <Host result={tiered} />
    </I18nextProvider>,
  );
  assert.equal(shmField().placeholder, "35", "the resolved size, not the default");
  assert.ok(
    screen.getByText(
      deployLocale.containerOverrides.shmHelpResolved.replace("{{size}}", "35"),
    ),
  );
  assert.equal(
    screen.queryByText(
      deployLocale.containerOverrides.shmHelpDefault.replace("{{size}}", "16"),
    ),
    null,
    "the 16 GiB default is not what an empty field gets",
  );

  // A stated size is itself the resolved size: it says nothing about what an
  // empty field would take, so the help falls back to the rule.
  const user = userEvent.setup({ document: dom.window.document });
  await user.type(shmField(), "64");
  await user.tab();
  assert.ok(
    screen.getByText(
      deployLocale.containerOverrides.shmHelpDefault.replace("{{size}}", "16"),
    ),
  );
  cleanup();

  // While a re-evaluation runs the result answers the previous draft.
  render(
    <I18nextProvider i18n={i18n}>
      <Host result={tiered} loading />
    </I18nextProvider>,
  );
  assert.equal(shmField().placeholder, "16");
  cleanup();

  // A PD draft resolves a size per role, so no single size is promised.
  render(
    <I18nextProvider i18n={i18n}>
      <Host result={{ shm_bytes: 16 * 2 ** 30, pd: { mode: "vllm-nixl", roles: [] } }} />
    </I18nextProvider>,
  );
  assert.equal(shmField().placeholder, "16");
  assert.ok(
    screen.getByText(
      deployLocale.containerOverrides.shmHelpDefault.replace("{{size}}", "16"),
    ),
  );
});

test("the SHM_BELOW_KV_CPU_TIER message interpolates the params the server sends, in both locales", () => {
  const params = { shm_gib: 16, tier_gib: 19 };
  const text = (locale: Record<string, any>) =>
    locale.apiError.SHM_BELOW_KV_CPU_TIER.replaceAll(
      "{{shm_gib}}",
      String(params.shm_gib),
    ).replaceAll("{{tier_gib}}", String(params.tier_gib));
  assert.match(text(commonLocale), /16 GiB.*19 GiB/s);
  const zhLocale = JSON.parse(
    fs.readFileSync(
      new URL("../src/i18n/locales/zh/common.json", import.meta.url),
      "utf8",
    ),
  );
  assert.match(text(zhLocale), /16 GiB.*19 GiB/s);
  assert.doesNotMatch(text(zhLocale), /\{\{/, "every parameter is interpolated");
  assert.doesNotMatch(text(commonLocale), /\{\{/);
});

test("Ascend compatibility errors interpolate the params in both locales", () => {
  const zhLocale = JSON.parse(
    fs.readFileSync(
      new URL("../src/i18n/locales/zh/common.json", import.meta.url),
      "utf8",
    ),
  );
  const cases = [
    { code: "ASCEND_V1_COMPAT_MULTI_NODE", params: { nnodes: 2 } },
    { code: "ASCEND_V1_COMPAT_PRE_START", params: { field: "pre_start_script" } },
    { code: "ENGRAM_SHARED_MEMORY_RUNTIME_UNSUPPORTED", params: { runtime: "udocker", flag: "--engram-config" } },
    { code: "ASCEND_COMPAT_PRE_START_ARGV", params: { field: "pre_start_argv", image: "quay.io/ascend/vllm-ascend:v0.30.0rc2-a3" } },
    { code: "PRE_START_SCRIPT_TOO_LARGE", params: { bytes: 65537, max_bytes: 65536 } },
    { code: "ASCEND_COMPAT_MANUAL_UNSUPPORTED", params: { image: "quay.io/ascend/vllm-ascend:v0.30.0rc2-a3", runtime: "udocker" } },
    { code: "ENGRAM_MANUAL_UNSUPPORTED", params: { flag: "--engram-config" } },
  ];
  for (const { code, params } of cases) {
    for (const locale of [commonLocale, zhLocale]) {
      let text = locale.apiError[code];
      assert.ok(text, `${code} must have a localized message`);
      for (const [key, value] of Object.entries(params)) {
        text = text.replaceAll(`{{${key}}}`, String(value));
      }
      assert.doesNotMatch(text, /\{\{/, `${code} has an unexpanded parameter`);
      for (const value of Object.values(params)) {
        assert.ok(text.includes(String(value)), `${code} must include ${value}`);
      }
    }
  }
});
