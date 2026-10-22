import assert from "node:assert/strict";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";
import { fixturePdModes } from "./helpers/engineFixture.ts";

// pdModesForEngine's own unit coverage lives in deployPd.test.ts (pure
// function, profile in, mode list out). This file is the one place that
// checks the PD topology ChoiceCard's enabled/disabled state end to end
// against a real engine profile, for an engine with vllm's mode, one with
// sglang's mode, and one with none - the case the server's pd_modes field
// exists to answer (item 1 of the PD web UI plan).

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
  fs.readFileSync(new URL("../src/i18n/locales/en/deploy.json", import.meta.url), "utf8"),
);
const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: { en: { deploy: deployLocale } },
  defaultNS: "deploy",
  interpolation: { escapeValue: false },
});

const { Form } = await import("antd");
const { cleanup, fireEvent, render, screen, waitFor } = await import("@testing-library/react");
const { default: NodeSection } = await import("../src/pages/deploy/DeployNodeSection.tsx");
const { default: DeployPdSection } = await import("../src/pages/deploy/DeployPdSection.tsx");
import type { EngineProfile, Node } from "../src/api.ts";

afterEach(() => cleanup());
after(() => {
  closeChannels();
  dom.window.close();
});

function Harness({ profile, vendor = "nvidia" }: { profile?: EngineProfile; vendor?: string }) {
  const [form] = Form.useForm();
  return (
    <I18nextProvider i18n={i18n}>
      <Form form={form} initialValues={{ target_vendor: vendor, node_selection_mode: "auto" }}>
        <NodeSection
          nodes={[] as Node[]}
          eligibleNodes={[]}
          selection={undefined}
          selectedNode={undefined}
          modelReady
          result={null}
          profile={profile}
          onNodeMode={() => {}}
        />
      </Form>
    </I18nextProvider>
  );
}

function pdCardButton(): HTMLElement {
  return screen.getByText(deployLocale.workbench.node.pd).closest("button") as HTMLElement;
}

test("an engine profile listing vllm-nixl enables the PD topology card", () => {
  render(<Harness profile={{ name: "vllm", pd_modes: fixturePdModes("vllm").filter((mode) => mode.name === "vllm-nixl") } as EngineProfile} />);
  const card = pdCardButton();
  assert.equal(card.hasAttribute("disabled"), false);
  assert.ok(screen.getByText(deployLocale.workbench.node.pdDescription));
});

test("an engine profile listing sglang-nixl also enables the PD topology card", () => {
  render(<Harness profile={{ name: "sglang", pd_modes: fixturePdModes("sglang") } as EngineProfile} />);
  const card = pdCardButton();
  assert.equal(card.hasAttribute("disabled"), false);
  assert.ok(screen.getByText(deployLocale.workbench.node.pdDescription));
});

test("an engine profile with no pd_modes disables the PD topology card", () => {
  render(<Harness profile={{ name: "callable" } as EngineProfile} />);
  const card = pdCardButton();
  assert.equal(card.hasAttribute("disabled"), true);
  assert.ok(screen.getByText(deployLocale.workbench.node.pdEngineUnsupported));
});

test("no engine profile at all (still loading) disables the PD topology card", () => {
  render(<Harness profile={undefined} />);
  const card = pdCardButton();
  assert.equal(card.hasAttribute("disabled"), true);
});

// vllm's real profile lists two PD modes (vllm-ascend-mooncake and
// vllm-nixl) - the deploy form must not collapse an engine with several
// modes down to just the first one; it offers all of them as choices.
test("an engine with several PD modes offers every one of them as a choice, not just the first", () => {
  render(
    <I18nextProvider i18n={i18n}>
      <Form>
        <DeployPdSection
          nodes={[]}
          eligibleNodes={[]}
          result={null}
          profile={{ name: "vllm", pd_modes: fixturePdModes("vllm") } as EngineProfile}
        />
      </Form>
    </I18nextProvider>,
  );
  const combobox = screen.getByRole("combobox", { name: new RegExp(`^${deployLocale.pd.mode}$`) });
  fireEvent.mouseDown(combobox.closest(".ant-select")!.querySelector(".ant-select-selector") as HTMLElement);
  const options = [...document.querySelectorAll(".ant-select-dropdown .ant-select-item-option")].map(
    (el) => el.textContent,
  );
  assert.deepEqual(options, ["vllm-ascend-mooncake", "vllm-nixl"]);
});

function modeOptions(): (string | null)[] {
  const combobox = screen.getByRole("combobox", { name: new RegExp(`^${deployLocale.pd.mode}$`) });
  fireEvent.mouseDown(combobox.closest(".ant-select")!.querySelector(".ant-select-selector") as HTMLElement);
  return [...document.querySelectorAll(".ant-select-dropdown .ant-select-item-option")].map((el) => el.textContent);
}

test("the PD mode choice lists only the modes declared for the draft's vendor", () => {
  render(
    <I18nextProvider i18n={i18n}>
      <Form initialValues={{ target_vendor: "nvidia" }}>
        <DeployPdSection
          nodes={[]}
          eligibleNodes={[]}
          result={null}
          profile={{ name: "vllm", pd_modes: fixturePdModes("vllm") } as EngineProfile}
        />
      </Form>
    </I18nextProvider>,
  );
  assert.deepEqual(modeOptions(), ["vllm-nixl"]);
});

test("a PD mode picked for another vendor gives way to this vendor's default", async () => {
  let form: import("antd").FormInstance | undefined;
  function Host() {
    const [instance] = Form.useForm();
    form = instance;
    return (
      <I18nextProvider i18n={i18n}>
        <Form form={instance} initialValues={{ target_vendor: "nvidia", pd_mode: "vllm-ascend-mooncake" }}>
          <DeployPdSection
            nodes={[]}
            eligibleNodes={[]}
            result={null}
            profile={{ name: "vllm", pd_modes: fixturePdModes("vllm") } as EngineProfile}
          />
        </Form>
      </I18nextProvider>
    );
  }
  render(<Host />);
  await waitFor(() => assert.equal(form!.getFieldValue("pd_mode"), "vllm-nixl"));
});

test("an engine with PD modes, none for the draft's vendor, disables the PD card and names the vendor", () => {
  render(<Harness vendor="hygon" profile={{ name: "vllm", pd_modes: fixturePdModes("vllm") } as EngineProfile} />);
  assert.equal(pdCardButton().hasAttribute("disabled"), true);
  assert.ok(screen.getByText(deployLocale.workbench.node.pdVendorUnsupported.replace("{{vendor}}", "Hygon")));
});
