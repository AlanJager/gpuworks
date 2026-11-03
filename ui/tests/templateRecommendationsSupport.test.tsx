import { dom, createI18n } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
const { render, screen, waitFor, cleanup, act } = await import("@testing-library/react");
const { api } = await import("../src/api.ts");
const { default: Recommendations } = await import("../src/pages/deploy/TemplateRecommendations.tsx");
const { i18n } = await createI18n(["deploy"]);
const original = api.inferenceTemplateIndex;
afterEach(() => { cleanup(); api.inferenceTemplateIndex = original; });
after(() => dom.window.close());
const model = { id: 1, source_id: "qwen/model", name: "Qwen" } as never;
const index = (compatible: boolean, arch = "gfx1151") => ({
  hardware: [{ vendor: "amd", name: "Radeon 8060S", arch_id: arch }],
  items: [{ id: 2, model_id: "qwen/model", display_title: "AMD profile", scenarios: ["balanced"], profiles: [{
    name: "amd-explicit", source: "user", engine: "vllm", scenario: "balanced", display_name_en: "AMD explicit spec",
    gpus: { vendor: "amd", name: "(?i)radeon", arch_id: "gfx1151" }, detected_compatible: compatible,
  }] }],
});
const view = (nodeUUID: string) => <I18nextProvider i18n={i18n}><Recommendations model={model} nodeUUID={nodeUUID} targetVendor="amd" onSelect={() => {}} /></I18nextProvider>;

test("recommendations pass node scope and trust server unsupported decisions", async () => {
  const requests: unknown[] = [];
  api.inferenceTemplateIndex = (async (scope) => { requests.push(scope); return index(false, "gfx942"); }) as never;
  render(view("mi300-node"));
  await screen.findByText("No chip-compatible inference profiles are available");
  assert.deepEqual(requests, [{ vendor: "amd", node_uuid: "mi300-node" }]);
  assert.equal(screen.queryByRole("button", { name: /AMD profile/ }) === null, true);
  assert.ok(screen.getByText(/incompatible profiles/));
});

test("a late response from another node cannot replace the current compatibility scope", async () => {
  let resolveOld!: (value: unknown) => void;
  api.inferenceTemplateIndex = ((scope) => scope?.node_uuid === "old-node"
    ? new Promise((resolve) => { resolveOld = resolve; })
    : Promise.resolve(index(true))) as never;
  const mounted = render(view("old-node"));
  await waitFor(() => assert.equal(typeof resolveOld, "function"));
  mounted.rerender(view("mixed-node"));
  assert.ok(await screen.findByRole("button", { name: /AMD profile/ }));
  await act(async () => { resolveOld(index(false, "gfx942")); await Promise.resolve(); });
  assert.ok(screen.getByRole("button", { name: /AMD profile/ }));
  assert.ok(screen.getByText(/assigned GPUs, memory/));
});

test("a failed scope query clears prior compatible recommendations", async () => {
  api.inferenceTemplateIndex = (async (scope) => {
    if (scope?.node_uuid === "missing-node") throw new Error("node not found");
    return index(true);
  }) as never;
  const mounted = render(view("mixed-node"));
  await screen.findByRole("button", { name: /AMD profile/ });
  mounted.rerender(view("missing-node"));
  await screen.findByText(/node not found/);
  assert.equal(screen.queryByRole("button", { name: /AMD profile/ }) === null, true);
});
