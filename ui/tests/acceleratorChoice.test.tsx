import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { createI18n } from "./helpers/dom.ts";
import { I18nextProvider } from "react-i18next";

const { i18n } = await createI18n(["nodes"]);
const { cleanup, fireEvent, render, screen, waitFor } = await import("@testing-library/react");
const { api } = await import("../src/api.ts");
const { default: AcceleratorChoice, hasAcceleratorChoice } = await import("../src/components/AcceleratorChoice.tsx");
import type { Node } from "../src/api";

const realUpdate = api.updateNodeAccelerator;
afterEach(() => {
  cleanup();
  api.updateNodeAccelerator = realUpdate;
});

// The host 172.25.115.0: an Atlas 310P3 the node manages, and an RTX 4090.
const mixed = {
  uuid: "mixed-node",
  gpu_vendor: "ascend",
  accelerator_vendors: [
    { vendor: "ascend", cards: 1, model: "310P3" },
    { vendor: "nvidia", cards: 1, model: "AD102 [GeForce RTX 4090]" },
  ],
};

const renderChoice = (node: Parameters<typeof AcceleratorChoice>[0]["node"], onUpdated?: (node: Node) => void) =>
  render(<I18nextProvider i18n={i18n}><AcceleratorChoice node={node} onUpdated={onUpdated} /></I18nextProvider>);

async function chooseOption(label: string) {
  fireEvent.mouseDown(screen.getByRole("combobox", { name: "Accelerator" }));
  let option: Element | undefined;
  await waitFor(() => {
    option = [...document.querySelectorAll(".ant-select-item-option")].find((el) => el.textContent === label);
    if (!option) throw new Error(`option ${label} not offered`);
  });
  fireEvent.click(option!);
}

test("the choice exists only on a host with cards of more than one vendor", () => {
  assert.equal(hasAcceleratorChoice({}), false);
  assert.equal(hasAcceleratorChoice({ accelerator_vendors: [] }), false);
  assert.equal(hasAcceleratorChoice({ accelerator_vendors: [{ vendor: "nvidia", cards: 8 }] }), false);
  // A vendor with no usable card is no choice.
  assert.equal(hasAcceleratorChoice({ accelerator_vendors: [{ vendor: "nvidia", cards: 8 }, { vendor: "ascend", cards: 0 }] }), false);
  assert.equal(hasAcceleratorChoice(mixed), true);

  const { container } = renderChoice({ uuid: "plain", gpu_vendor: "nvidia", accelerator_vendors: [{ vendor: "nvidia", cards: 8 }] });
  assert.equal(container.textContent, "");
  assert.equal(screen.queryByRole("combobox"), null);
});

test("automatic says which vendor it picked and which cards it leaves unmanaged", async () => {
  renderChoice(mixed);
  assert.equal(document.querySelector(".ant-select-selection-item")?.textContent, "Auto (now Ascend)");
  const alert = screen.getByRole("alert");
  assert.equal(alert.textContent, "This machine also has NVIDIA ×1 (AD102 [GeForce RTX 4090]), not managed by this node; you can switch. A node manages one vendor's cards at a time.");
  assert.equal(screen.queryByRole("status"), null);

  fireEvent.mouseDown(screen.getByRole("combobox", { name: "Accelerator" }));
  await waitFor(() => assert.equal(document.querySelectorAll(".ant-select-item-option").length, 3));
  assert.deepEqual([...document.querySelectorAll(".ant-select-item-option")].map((el) => el.textContent), [
    "Auto (now Ascend)", "Ascend ×1 (310P3)", "NVIDIA ×1 (AD102 [GeForce RTX 4090])",
  ]);
});

test("a card without a model name is named by its vendor alone, and every other vendor is listed", () => {
  renderChoice({
    uuid: "three", gpu_vendor: "nvidia",
    accelerator_vendors: [{ vendor: "ascend", cards: 2 }, { vendor: "hygon", cards: 1, model: "K100_AI" }, { vendor: "nvidia", cards: 4 }],
  });
  assert.equal(screen.getByRole("alert").textContent,
    "This machine also has Ascend ×2, Hygon ×1 (K100_AI), not managed by this node; you can switch. A node manages one vendor's cards at a time.");
});

test("a chosen vendor that the node manages shows no warning", () => {
  renderChoice({ ...mixed, gpu_vendor: "nvidia", accelerator_preference: "nvidia" });
  assert.equal(document.querySelector(".ant-select-selection-item")?.textContent, "NVIDIA ×1 (AD102 [GeForce RTX 4090])");
  assert.equal(screen.queryByRole("alert"), null);
  assert.equal(screen.queryByRole("status"), null);
});

test("until the agent restarts onto the chosen vendor the node says it is switching", () => {
  renderChoice({ ...mixed, accelerator_preference: "nvidia" });
  assert.equal(screen.getByRole("status").textContent, "Switching to NVIDIA: the agent restarts and then manages its cards.");
  assert.equal(screen.queryByRole("alert"), null);
});

test("choosing the other vendor asks the server and hands back the updated node", async () => {
  const calls: string[] = [];
  let resolve!: (node: Node) => void;
  api.updateNodeAccelerator = (uuid, accelerator) => {
    calls.push(`${uuid} ${accelerator}`);
    return new Promise<Node>((r) => { resolve = r; });
  };
  const updated: Node[] = [];
  renderChoice(mixed, (node) => updated.push(node));
  await chooseOption("NVIDIA ×1 (AD102 [GeForce RTX 4090])");
  assert.deepEqual(calls, ["mixed-node nvidia"]);
  // Busy while the request is out: no second request can start.
  await waitFor(() => assert.ok(document.querySelector(".ant-select-disabled")));
  resolve({ ...mixed, accelerator_preference: "nvidia" } as unknown as Node);
  await waitFor(() => assert.equal(updated.length, 1));
  assert.equal(updated[0].accelerator_preference, "nvidia");
  await screen.findByText("Switched. The agent is restarting; the new cards appear shortly");
  await waitFor(() => assert.equal(document.querySelector(".ant-select-disabled"), null));
});

test("choosing what is already chosen sends nothing", async () => {
  const calls: string[] = [];
  api.updateNodeAccelerator = async (uuid, accelerator) => { calls.push(`${uuid} ${accelerator}`); return mixed as unknown as Node; };
  renderChoice(mixed);
  await chooseOption("Auto (now Ascend)");
  assert.deepEqual(calls, []);
});

test("a refused switch says why and leaves the choice as it was", async () => {
  api.updateNodeAccelerator = async () => {
    throw new Error("1 instance(s) still run on this node; stop or move them before switching accelerators.");
  };
  const updated: Node[] = [];
  renderChoice(mixed, (node) => updated.push(node));
  await chooseOption("NVIDIA ×1 (AD102 [GeForce RTX 4090])");
  await screen.findByText("Could not switch accelerators: 1 instance(s) still run on this node; stop or move them before switching accelerators.");
  assert.equal(updated.length, 0);
  assert.equal(document.querySelector(".ant-select-selection-item")?.textContent, "Auto (now Ascend)");
});
