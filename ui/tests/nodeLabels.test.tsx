import assert from "node:assert/strict";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

// rc-overflow schedules through real MessagePorts but never closes them.
// Track these test-owned channels so the Node runner can exit after cleanup.
const closeChannels = trackMessageChannels();
const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "HTMLTextAreaElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot", "HTMLInputElement"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });


const locale = JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/nodes.json", import.meta.url), "utf8"));
const i18n = createInstance();
await i18n.use(initReactI18next).init({ lng: "en", resources: { en: { nodes: locale } }, defaultNS: "nodes", interpolation: { escapeValue: false } });
const { cleanup, render, screen, waitFor, fireEvent } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { Form, message } = await import("antd");
const { default: NodePlacementLabels, matchedNames } = await import("../src/components/NodePlacementLabels.tsx");
const { installFleet } = await import("./helpers/fakeFleet.ts");
const { default: NodeLabelsModal } = await import("../src/components/NodeLabelsModal.tsx");
const { api } = await import("../src/api.ts");
type NodeType = import("../src/api").Node;
const nodes = [
  { uuid: "a", hostname: "worker-a", state: "online", labels: { pool: "inference", zone: "a", empty: "" } },
  { uuid: "b", hostname: "worker-b", state: "online", labels: { pool: "inference", zone: "b" } },
] as unknown as NodeType[];
const original = { ...api };
afterEach(() => { cleanup(); message.destroy(); Object.assign(api, original); });
after(() => { closeChannels(); dom.window.close(); });
const wrap = (child: React.ReactNode) => <I18nextProvider i18n={i18n}>{child}</I18nextProvider>;

// Which nodes carry the picked labels is the server's answer (GET /nodes with
// label filters, matched as placement matches them); the control shows its
// count and the first few names of the page it asked for.
test("a large match names a few nodes, not all of them", () => {
 const many = Array.from({ length: 5 }, (_, i) => ({ ...nodes[0], uuid: `n-${i}`, hostname: `gpu-${i}` }));
 assert.equal(matchedNames(many, 1000), "gpu-0, gpu-1, gpu-2, gpu-3, gpu-4, …");
 assert.equal(matchedNames(many.slice(0, 2)), "gpu-0, gpu-1");
});

test("label selector offers the scope's labels, asks the server which nodes match, and reports an empty match", async () => {
 const fleet = installFleet(api as never, [...nodes, { uuid: "c", hostname: "offline-c", state: "offline", labels: { pool: "inference", rack: "r9" } }] as never);
 const user = userEvent.setup({ document: dom.window.document });
 render(wrap(<Form initialValues={{ node_labels: { pool: "inference" } }}><NodePlacementLabels scope={{ state: ["online"] }} /></Form>));
 // The offline node carries pool=inference too, but the scope is the online nodes.
 assert.ok(await screen.findByText("Matching 2 nodes: worker-a, worker-b"));
 assert.deepEqual(fleet.pages.at(-1), { page: 1, page_size: 5, state: ["online"], label: ["pool=inference"] });
 const select = screen.getByRole("combobox");
 await user.click(select);
 // Options are the scope's label values from the server's facets.
 await waitFor(() => assert.ok(document.querySelector('.ant-select-item-option[title="zone=b"]')));
 assert.equal(document.querySelector('.ant-select-item-option[title="rack=r9"]'), null);
 await user.type(select, "zone=missing");
 await waitFor(() => assert.ok(screen.getByRole("option", { name: "zone=missing" })));
 fireEvent.keyDown(select, { key: "Enter", keyCode: 13, which: 13 });
 assert.ok(await screen.findByText(locale.labels.noMatch));
 assert.deepEqual(fleet.pages.at(-1)?.label, ["pool=inference", "zone=missing"]);
 assert.equal(screen.queryByText("Matching 2 nodes: worker-a, worker-b"), null);
});

test("node label editor retains failed edits and submits exact replacements", async () => {
 let saved: NodeType | undefined; const bodies: Record<string,string>[] = []; let fail = true;
 api.updateNodeLabels = async (uuid, labels) => {
   assert.equal(uuid, "a"); bodies.push(labels);
   if (fail) throw new Error("offline");
   return { ...nodes[0], labels };
 };
 render(wrap(<NodeLabelsModal node={nodes[0]} onClose={() => {}} onSaved={(value) => { saved = value; }} />));
 fireEvent.change(screen.getAllByLabelText("Label value")[0], { target: { value: "training" } });
 fireEvent.click(screen.getByRole("button", { name: "Save" }));
 await waitFor(() => assert.equal(bodies.length, 1));
 assert.equal(saved, undefined);
 assert.ok(await screen.findByText("Could not save labels: offline"));
 assert.equal((screen.getAllByLabelText("Label value")[0] as HTMLInputElement).value, "training");
 fail = false;
 fireEvent.click(screen.getByRole("button", { name: "Save" }));
 await waitFor(() => assert.ok(saved));
 assert.deepEqual(saved?.labels, { pool: "training", zone: "a", empty: "" });
});
