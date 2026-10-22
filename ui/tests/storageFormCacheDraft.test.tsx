import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React, { useState } from "react";
import { createI18n, dom } from "./helpers/dom.ts";
import { node } from "./helpers/storageFixtures.ts";
import { I18nextProvider } from "react-i18next";
import type { Node } from "../src/api.ts";
import { emptyForm, type StorageFormValues } from "../src/pages/storage/storageForm.ts";

const { i18n, locale } = await createI18n(["storage"]);
const { cleanup, fireEvent, render, screen, within, waitFor } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { default: DraftTable, draftNodeApplies } = await import("../src/pages/storage/StorageCacheDraftTable.tsx");
const { default: TargetsEditor } = await import("../src/pages/storage/StorageTargetsEditor.tsx");
const { default: PolicyFields } = await import("../src/pages/storage/StorageCachePolicyFields.tsx");
const s = locale.storage;
afterEach(cleanup);
const GIB = 1024 ** 3;

function renderDraft(initial: StorageFormValues, nodes: Node[]) {
  let latest = initial;
  function Harness() {
    const [values, setValues] = useState(initial);
    latest = values;
    return <DraftTable values={values} nodes={nodes} onChange={(patch) => setValues((current) => ({ ...current, ...patch }))} />;
  }
  const view = render(<I18nextProvider i18n={i18n}><Harness /></I18nextProvider>);
  return { view, value: () => latest, user: userEvent.setup({ document: dom.window.document }) };
}

test("policy controls preserve explicit zero, inheritance and custom TiB values", async () => {
  let value: number | undefined;
  function Harness() {
    const [current, setCurrent] = useState<number | undefined>(undefined);
    value = current;
    return <PolicyFields value={current} onChange={setCurrent} allowInherit defaultBytes={200 * GIB} />;
  }
  render(<I18nextProvider i18n={i18n}><Harness /></I18nextProvider>);
  const user = userEvent.setup({ document: dom.window.document });
  fireEvent.click(screen.getByRole("radio", { name: s.cachePolicy.unlimited }));
  assert.equal(value, 0);
  fireEvent.click(screen.getByRole("radio", { name: s.cachePolicy.custom }));
  assert.ok(Number.isNaN(value));
  await user.type(screen.getByRole("spinbutton", { name: s.cachePolicy.limit }), "2");
  assert.equal(value, 2 * GIB);
  fireEvent.mouseDown(screen.getByRole("combobox", { name: s.cachePolicy.unit }));
  await user.click((await screen.findAllByText("TiB")).at(-1)!);
  assert.equal(value, 2 * GIB, "changing display unit preserves capacity");
  const input = screen.getByRole("spinbutton", { name: s.cachePolicy.limit });
  await user.clear(input);
  await user.type(input, "1");
  assert.equal(value, 1024 * GIB);
  fireEvent.click(screen.getByRole("radio", { name: s.cachePolicy.inherit }));
  assert.equal(value, undefined);
});

test("batch selection spans pages and modifies only the chosen field while preserving membership", async () => {
  const nodes = Array.from({ length: 26 }, (_, i) => node(`n${i}`, `gpu-${i.toString().padStart(2, "0")}`)) as Node[];
  const initial: StorageFormValues = { ...emptyForm("nfs_cache"), scope: "nodes", cacheMaxGiB: 200,
    nodes: nodes.map((n) => ({ node_uuid: n.uuid, path: "/mnt/own", cache_path: "/cache/own", existing: true, cache_max_bytes: 300 * GIB })) };
  const { user, value } = renderDraft(initial, nodes);
  assert.equal(screen.queryByText("gpu-25"), null, "only one page renders");
  await user.click(screen.getByRole("button", { name: s.cacheDraft.selectFiltered.replace("{{count}}", "26") }));
  await user.click(screen.getByRole("button", { name: s.cacheDraft.batch }));
  const editor = screen.getByRole("dialog");
  await user.click(within(editor).getByLabelText(s.cacheDraft.changeLimit));
  fireEvent.click(within(editor).getByRole("radio", { name: s.cachePolicy.unlimited }));
  await user.click(within(editor).getByRole("button", { name: s.cacheDraft.apply }));
  assert.equal(value().nodes.length, 26);
  assert.ok(value().nodes.every((n) => n.cache_max_bytes === 0 && n.cache_path === "/cache/own" && n.path === "/mnt/own" && n.existing && !n.override_only));
  await user.click(screen.getByRole("button", { name: s.cacheDraft.restoreLimit }));
  assert.ok(value().nodes.every((n) => n.cache_max_bytes === undefined && !n.override_only));
});

test("new label-scoped exceptions do not select nodes and invalid custom input cannot apply", async () => {
  const initial = { ...emptyForm("nfs_cache"), scope: "labels" as const, labels: [{ key: "pool", value: "gpu" }] };
  const { user, value } = renderDraft(initial, [node("n1", "gpu-01", { labels: { pool: "gpu" } }), node("n2", "other")] as Node[]);
  assert.equal(screen.queryByText("other"), null);
  await user.click(screen.getByRole("button", { name: s.cacheDraft.configure }));
  const editor = screen.getByRole("dialog");
  fireEvent.click(within(within(editor).getByRole("radiogroup", { name: s.cachePolicy.limit })).getByRole("radio", { name: s.cachePolicy.custom }));
  await user.click(within(editor).getByRole("button", { name: s.cacheDraft.apply }));
  assert.ok(within(editor).getByText(s.form.error.cacheLimit));
  assert.equal(value().nodes.length, 0);
  fireEvent.click(within(editor).getByRole("radio", { name: s.cachePolicy.unlimited }));
  await user.click(within(editor).getByRole("button", { name: s.cacheDraft.apply }));
  assert.deepEqual(value().nodes, [{ node_uuid: "n1", path: "", cache_path: "", existing: false, override_only: true, cache_max_bytes: 0 }]);
});

test("capacity is unknown for a changed path or offline node, and inactive settings remain discoverable", async () => {
  const initial = { ...emptyForm("nfs_cache"), scope: "labels" as const, labels: [{ key: "pool", value: "gpu" }], cachePath: "/new", nodes: [
    { node_uuid: "n1", path: "", cache_path: "", existing: false, override_only: true },
  ] };
  renderDraft(initial, [node("n1", "inactive", { path_usage: [{ path: "/old", available_bytes: 500 * GIB, total_bytes: 1024 * GIB }] })] as Node[]);
  assert.ok(screen.getByText(s.cacheDraft.inactive));
  assert.ok(screen.getByText(s.cacheDraft.unverified));
  assert.equal(screen.queryByText("500 GiB"), null);
  await waitFor(() => assert.ok(screen.getByText("inactive")));
});


test("switching to labels keeps legacy membership visible until explicitly removed", async () => {
  let latest: StorageFormValues;
  function Harness() {
    const [values, setValues] = useState<StorageFormValues>({ ...emptyForm("nfs_cache"), scope: "mixed", allNodes: true,
      labels: [{ key: "pool", value: "gpu" }], nodes: [{ node_uuid: "n1", path: "/legacy", cache_path: "/cache", cache_max_bytes: 0, existing: true }],
    });
    latest = values;
    return <TargetsEditor values={values} nodes={[node("n1", "gpu-01")] as Node[]} errors={{}} onChange={(patch) => setValues((current) => ({ ...current, ...patch }))} />;
  }
  render(<I18nextProvider i18n={i18n}><Harness /></I18nextProvider>);
  fireEvent.click(screen.getByRole("radio", { name: s.form.scope.labels }));
  assert.equal((screen.getByRole("radio", { name: s.form.scope.mixed }) as HTMLInputElement).checked, true);
  assert.equal(latest!.allNodes, false);
  const picker = screen.getByRole("combobox", { name: s.form.scope.chooseNodes });
  fireEvent.keyDown(picker, { key: "Backspace", code: "Backspace", keyCode: 8 });
  await waitFor(() => assert.equal(latest!.scope, "labels"));
  assert.deepEqual(latest!.nodes, [{ node_uuid: "n1", path: "/legacy", cache_path: "/cache", cache_max_bytes: 0, existing: true, override_only: true }]);
});


test("draft scope uses the same user labels as storage membership, not generated system labels", () => {
  const values: StorageFormValues = { ...emptyForm("nfs_cache"), scope: "labels", labels: [{ key: "gpuworks.io/vendor", value: "nvidia" }] };
  const generated = node("n1", "gpu-01", { system_labels: [{ key: "gpuworks.io/vendor", value: "nvidia", display: "NVIDIA" }] }) as Node;
  assert.equal(draftNodeApplies(values, generated), false);
  const userLabeled = node("n2", "gpu-02", { labels: { pool: "gpu" } }) as Node;
  assert.equal(draftNodeApplies({ ...values, labels: [{ key: "pool", value: "gpu" }] }, userLabeled), true);
  assert.equal(draftNodeApplies({ ...values, labels: [{ key: "pool", value: "other" }] }, userLabeled), false);
});
