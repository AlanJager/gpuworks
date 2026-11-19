import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";

const { i18n, locale } = await createI18n(["common", "deploy"], "en");
const { cleanup, render, screen, waitFor } = await import("@testing-library/react");
const { api } = await import("../src/api.ts");
const { installFleet } = await import("./helpers/fakeFleet.ts");
const { nodeOption, nodeOptions, openNodePicker, unavailable } = await import("./helpers/nodePicker.ts");
const { default: EligibleNodePicker } = await import("../src/pages/deploy/EligibleNodePicker.tsx");
type EligibleNode = import("../src/api.ts").EligibleNode;

const original = { nodes: api.nodes, nodesPage: api.nodesPage, node: api.node, nodeFacets: api.nodeFacets };
afterEach(() => { cleanup(); Object.assign(api, original); });

const deploy = locale.deploy as Record<string, any>;
const fleet = [
  { uuid: "big", name: "h20-8x140", state: "online", gpu_count: 8 },
  { uuid: "small", name: "rtx3090-8x24", state: "online", gpu_count: 8 },
  { uuid: "other", name: "amd-box", state: "online", gpu_count: 2 },
  { uuid: "down", name: "h20-spare", state: "online", gpu_count: 8 },
];
// The server's answers as evaluate sends them (eligible_nodes): a node of the
// right chip whose cards are too small, a node of another chip, and a node
// that went offline since the list was read.
const judged: EligibleNode[] = [
  { uuid: "big", name: "h20-8x140", eligible: true },
  { uuid: "small", name: "rtx3090-8x24", eligible: false, reasons: [{ code: "no_fit", params: { gpus: 1, tp: 1, pp: 1, gpu_count: 8, required_gib: "204.0", available_gib: "21.6", gpu_memory_utilization: 0.9 } }] },
  { uuid: "other", name: "amd-box", eligible: false, reasons: [{ code: "chip_mismatch", params: { predicate: "nvidia cc>=7.5" } }] },
  { uuid: "down", name: "h20-spare", eligible: false, reasons: [{ code: "node_offline" }] },
] as never;

const fill = (text: string, params: Record<string, unknown>) => text.replace(/\{\{(\w+)\}\}/g, (_, key) => String(params[key] ?? ""));

test("a node the draft cannot run on says why in its own words, not that its chip differs", async () => {
  installFleet(api as never, fleet as never);
  render(<I18nextProvider i18n={i18n}><EligibleNodePicker aria-label="Node" eligibleNodes={judged} /></I18nextProvider>);
  await openNodePicker(screen.getByRole("combobox", { name: "Node" }));
  await waitFor(() => assert.equal(nodeOptions().length, 4));

  const fits = nodeOption("h20-8x140 · 8 GPU");
  assert.equal(unavailable(fits), false, "a node that fits is offered as it is, with no reason");

  // Too small: the scheduler's sentence with the node's own figures.
  const tooSmall = nodeOption(/^rtx3090-8x24 · 8 GPU · /);
  assert.equal(unavailable(tooSmall), true);
  const noFit = fill(deploy.schedule.no_fit, judged[1].reasons![0].params as Record<string, unknown>);
  assert.ok(tooSmall!.textContent!.endsWith(` · ${noFit}`), tooSmall!.textContent ?? "");
  assert.ok(noFit.includes("204.0") && noFit.includes("21.6"), noFit);
  assert.ok(!/chip/i.test(tooSmall!.textContent ?? ""), "a node of the right chip is not called a chip mismatch");

  // Another chip: that is what it says.
  const otherChip = nodeOption(/^amd-box · 2 GPU · /);
  assert.equal(unavailable(otherChip), true);
  assert.ok(otherChip!.textContent!.endsWith(` · ${fill(deploy.placementReason.chip_mismatch, { predicate: "nvidia cc>=7.5" })}`), otherChip!.textContent ?? "");

  // Offline: likewise.
  const offline = nodeOption(/^h20-spare · 8 GPU · /);
  assert.equal(unavailable(offline), true);
  assert.ok(offline!.textContent!.endsWith(` · ${deploy.placementReason.node_offline}`), offline!.textContent ?? "");
});

test("before the first evaluation every node is offered with no reason", async () => {
  installFleet(api as never, fleet as never);
  render(<I18nextProvider i18n={i18n}><EligibleNodePicker aria-label="Node" /></I18nextProvider>);
  await openNodePicker(screen.getByRole("combobox", { name: "Node" }));
  await waitFor(() => assert.equal(nodeOptions().length, 4));
  for (const option of nodeOptions()) {
    assert.equal(unavailable(option), false);
    assert.match(option.textContent ?? "", /^[\w-]+ · \d GPU$/);
  }
});
