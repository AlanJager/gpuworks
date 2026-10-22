import assert from "node:assert/strict";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

const closeChannels = trackMessageChannels();
const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });

const deployLocale = JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/deploy.json", import.meta.url), "utf8"));
const i18n = createInstance();
await i18n.use(initReactI18next).init({ lng: "en", fallbackLng: "en", resources: { en: { deploy: deployLocale } }, defaultNS: "deploy", interpolation: { escapeValue: false } });

const { cleanup, fireEvent, render, screen, waitFor } = await import("@testing-library/react");
const { Form } = await import("antd");
const NodeSection = (await import("../src/pages/deploy/DeployNodeSection.tsx")).default;
const { nodeDisplayName, nodeRankAddress } = await import("../src/nodeDisplay.ts");
const { deploymentNodesLabel } = await import("../src/pages/DeploymentsPage.tsx");
const { api } = await import("../src/api.ts");
const { installFleet } = await import("./helpers/fakeFleet.ts");
const apiOriginal = { ...api };

afterEach(() => { cleanup(); Object.assign(api, apiOriginal); });
after(() => { closeChannels(); dom.window.close(); });
const node = deployLocale.workbench.node;

const gpu = { index: 0, name: "H200", vendor: "nvidia" };
const nodes = [
  { uuid: "aaaa1111", name: "nebius-h200-1", hostname: "pd-h200-1", state: "online", gpu_count: 8, gpus: [gpu], compute_network: "nebius-ib", compute_address: "10.96.0.56" },
  { uuid: "bbbb2222", name: "", hostname: "pd-h200-2", state: "online", gpu_count: 8, gpus: [gpu], management_address: "10.96.0.48" },
  { uuid: "cccc3333", name: "gpu3", hostname: "172-25-17-156", state: "online", gpu_count: 2, gpus: [gpu] },
] as never[];

test("display names prefer the operator's name, and rank addresses prefer the compute network", () => {
  assert.equal(nodeDisplayName({ uuid: "abcdef123456", name: " gpu3 ", hostname: "172-25-17-156" }), "gpu3");
  assert.equal(nodeDisplayName({ uuid: "abcdef123456", name: "", hostname: "pd-h200-2" }), "pd-h200-2");
  assert.equal(nodeDisplayName({ uuid: "abcdef123456" }), "abcdef12");
  assert.equal(nodeDisplayName(undefined), "");
  assert.equal(nodeRankAddress({ compute_address: "10.96.0.56", management_address: "1.2.3.4" }), "10.96.0.56");
  assert.equal(nodeRankAddress({ management_address: "1.2.3.4" }), "1.2.3.4");
  assert.equal(nodeRankAddress({}), "");
});

test("deployments name every rank's node", () => {
  assert.equal(deploymentNodesLabel({ node_name: "nebius-h200-1", node_names: ["nebius-h200-1", "nebius-h200-2"] } as never), "nebius-h200-1, nebius-h200-2");
  assert.equal(deploymentNodesLabel({ node_name: "gpu3" } as never), "gpu3");
  assert.equal(deploymentNodesLabel({} as never, "0123456789"), "01234567");
});

// The pickers search the online nodes on the server; the fake answers as it does.
function renderSection(values: Record<string, unknown>, modelReady = true) {
  installFleet(api as never, nodes as never);
  return render(
    <I18nextProvider i18n={i18n}>
      <Form initialValues={values}>
        <NodeSection fleet={null} knownNodes={new Map()} selection={undefined as never} modelReady={modelReady} result={null} onNodeMode={() => {}} />
      </Form>
    </I18nextProvider>,
  );
}

test("the multi-node picker names nodes and accepts compute-network ranks", async () => {
  renderSection({ node_selection_mode: "multi", target_vendor: "nvidia", nnodes: 2 });
  assert.ok(screen.getByText(node.multiPlaceholder));
  const picker = screen.getByText(node.multiPlaceholder).closest(".ant-select") as HTMLElement;
  fireEvent.mouseDown(picker.querySelector(".ant-select-selector") as HTMLElement);
  await waitFor(() => assert.ok(document.querySelectorAll(".ant-select-item-option").length >= 3));
  const labels = [...document.querySelectorAll(".ant-select-item-option")].map((el) => ({ text: el.textContent, disabled: el.classList.contains("ant-select-item-option-disabled"), title: el.getAttribute("title") }));
  assert.deepEqual(labels.map((l) => [l.text, l.disabled]), [
    ["nebius-h200-1 · 8 GPU", false],
    ["pd-h200-2 · 8 GPU", false],
    [`gpu3 · 2 GPU · ${node.noRankAddress}`, true],
  ]);
  assert.equal(labels[2].title, node.noRankAddressHint);
});

test("the automatic card waits for a model and accelerator without claiming no node matches", () => {
  renderSection({ node_selection_mode: "auto" }, false);
  assert.ok(screen.getByText(node.chooseModelFirst));
  assert.equal(screen.queryByText(node.noCompatibleOnline), null);
});
