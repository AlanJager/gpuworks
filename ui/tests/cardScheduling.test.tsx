import assert from "node:assert/strict";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";

// An operator stops one card's scheduling or lets work back onto it from the
// card's row; what is offered is what the server says may be done, and letting
// work onto a card the platform judged unusable is confirmed first.

const closeChannels = trackMessageChannels();
const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot", "File", "Blob", "HTMLBodyElement"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });

const read = (ns: string) => JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/en/${ns}.json`, import.meta.url), "utf8"));
const i18n = createInstance();
await i18n.use(initReactI18next).init({ lng: "en", resources: { en: { nodes: read("nodes"), common: read("common") } }, interpolation: { escapeValue: false } });
const cs = i18n.getResourceBundle("en", "nodes").cardScheduling;

const { cleanup, render, screen, waitFor } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { api } = await import("../src/api.ts");
const { APIError } = await import("../src/apiError.ts");
const { CardSchedulingControl, CardSchedulingTag } = await import("../src/pages/nodes/CardScheduling.tsx");

const apiAny = api as unknown as Record<string, unknown>;
const saved = { setCardScheduling: apiAny.setCardScheduling };
afterEach(() => { cleanup(); Object.assign(apiAny, saved); });
after(() => { closeChannels(); dom.window.close(); });

const node = { uuid: "n-1", name: "gpu-1", gpus: [] } as never;
const card = (extra: object) => ({ index: 0, uuid: "GPU-a", pcie_bdf: "0000:3f:00.0", name: "H20", schedulable: true, ...extra }) as never;
const show = (gpu: never, onUpdated: (n: unknown) => void = () => {}) =>
  render(<I18nextProvider i18n={i18n}><CardSchedulingControl node={node} gpu={gpu} onUpdated={onUpdated} /></I18nextProvider>);

test("the tag shows only an operator's decision", () => {
  render(<I18nextProvider i18n={i18n}>
    <CardSchedulingTag gpu={card({})} />
    <CardSchedulingTag gpu={card({ scheduling_stopped: true, schedulable: false })} />
    <CardSchedulingTag gpu={card({ scheduling_allowed: true })} />
  </I18nextProvider>);
  assert.ok(screen.getByText(cs.stoppedTag));
  assert.ok(screen.getByText(cs.allowedTag));
  assert.equal(screen.getAllByText(/./, { selector: ".ant-tag" }).length, 2);
});

test("a schedulable card offers to stop it, and stopping sends its address and updates the node", async () => {
  const user = userEvent.setup({ document: dom.window.document });
  const sent: unknown[] = [];
  const updated = { uuid: "n-1", gpus: [] };
  apiAny.setCardScheduling = async (...args: unknown[]) => { sent.push(args); return updated; };
  let got: unknown = null;
  show(card({ scheduling_actions: ["stopped"] }), (n) => { got = n; });
  assert.ok(screen.getByText(cs.state.schedulable));
  assert.equal(screen.queryByRole("button", { name: cs.allow }), null);
  await user.click(screen.getByRole("button", { name: cs.stop }));
  await waitFor(() => assert.equal(got, updated));
  assert.deepEqual(sent, [["n-1", { pcie_bdf: "0000:3f:00.0", uuid: "GPU-a" }, "stopped"]]);
});

test("an operator's stop is lifted without asking", async () => {
  const user = userEvent.setup({ document: dom.window.document });
  const sent: string[] = [];
  apiAny.setCardScheduling = async (_n: string, _c: unknown, to: string) => { sent.push(to); return node; };
  show(card({ scheduling_stopped: true, schedulable: false, scheduling_actions: ["schedulable"] }));
  assert.ok(screen.getByText(cs.state.stopped));
  assert.equal(screen.queryByRole("button", { name: cs.stop }), null, "already stopped");
  await user.click(screen.getByRole("button", { name: cs.resume }));
  await waitFor(() => assert.deepEqual(sent, ["schedulable"]));
});

test("letting work onto a card the platform judged unusable asks first, and a refusal is shown", async () => {
  const user = userEvent.setup({ document: dom.window.document });
  const sent: string[] = [];
  apiAny.setCardScheduling = async (_n: string, _c: unknown, to: string) => {
    sent.push(to);
    throw new APIError("CARD_NOT_OWNED", {}, "the card is not this node's to use", 409);
  };
  show(card({ schedulable: false, scheduling_actions: ["stopped", "schedulable"] }));
  assert.ok(screen.getByText(cs.state.blocked));
  await user.click(screen.getByRole("button", { name: cs.allow }));
  assert.deepEqual(sent, [], "nothing is sent before the confirmation");
  await screen.findByText(cs.allowConfirm);
  const confirm = screen.getAllByRole("button", { name: cs.allow }).at(-1)!;
  await user.click(confirm);
  await waitFor(() => assert.deepEqual(sent, ["schedulable"]));
  await screen.findByText(read("common").apiError.CARD_NOT_OWNED);
});
