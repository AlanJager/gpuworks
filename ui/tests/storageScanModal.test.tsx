import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { createI18n, dom } from "./helpers/dom.ts";
import { nfsStorage } from "./helpers/storageFixtures.ts";
import { I18nextProvider } from "react-i18next";

const { i18n, locale } = await createI18n(["storage"]);
const { cleanup, render, screen, waitFor, within } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { api } = await import("../src/api.ts");
const { default: StorageScanModal, importItem } = await import("../src/pages/storage/StorageScanModal.tsx");

const s = locale.storage;
const originals = { scan: api.scanStorage, import: api.importStorage };
afterEach(() => { cleanup(); api.scanStorage = originals.scan; api.importStorage = originals.import; });

const candidate = (rel: string, extra: Record<string, unknown> = {}) => ({
  rel_path: rel, size_bytes: 15 * 1024 ** 3, files: 12, source: "modelscope", source_id: rel, basis: "modelscope", imported: false, ...extra,
});
const firstPage = {
  node_uuid: "n1",
  candidates: [
    candidate("Qwen/Qwen3-8B", { basis: "marker", revision: "main", subfolder: "fp8", format: "transformers", tasks: ["text-ranking"], bind_model_id: 7, bind_model_name: "Qwen3-8B" }),
    candidate("Qwen/Qwen3-14B"),
    candidate("acme/half-done", { incomplete: true, incomplete_reason: "*.part present", basis: "path", source: "local" }),
    candidate("acme/already", { imported: true, basis: "huggingface" }),
  ],
  skipped: [{ rel_path: "acme/empty", reason: "empty" }, { rel_path: "x/cache", reason: "hf_hub_cache_layout" }],
  next_cursor: "cursor-2",
  complete: false,
};

function renderModal(storage = nfsStorage()) {
  const calls = { closed: 0, imported: 0 };
  const view = render(
    <I18nextProvider i18n={i18n}>
      <StorageScanModal storage={storage} onClose={() => { calls.closed += 1; }} onImported={() => { calls.imported += 1; }} />
    </I18nextProvider>,
  );
  return { calls, view, user: userEvent.setup({ document: dom.window.document }) };
}
const row = (path: string) => screen.getAllByText(path).map((el) => el.closest("tr")).find(Boolean) as HTMLElement;
const checkbox = (path: string) => within(row(path)).getByRole("checkbox") as HTMLInputElement;
// A button that was loading keeps a "loading" icon in its accessible name for a moment, so match the label at the end.
const btn = (label: string) => screen.getByRole("button", { name: new RegExp(`${label}$`) }) as HTMLButtonElement;
const importButton = () => screen.getByRole("button", { name: /^Import selected/ }) as HTMLButtonElement;

test("while scanning it says so, then lists candidates with identity basis, binding and status", async () => {
  let release: (page: typeof firstPage) => void = () => {};
  api.scanStorage = (() => new Promise((resolve) => { release = resolve as never; })) as never;
  renderModal();
  assert.ok(screen.getAllByText(s.scan.scanning).length > 0);
  release(firstPage);
  assert.ok(await screen.findByText("Qwen/Qwen3-8B"));
  assert.ok(within(row("Qwen/Qwen3-8B")).getByText(s.scan.basis.marker));
  assert.ok(within(row("Qwen/Qwen3-8B")).getByText("transformers"));
  assert.ok(within(row("Qwen/Qwen3-8B")).getByText("text-ranking"));
  assert.ok(within(row("Qwen/Qwen3-14B")).getByText(s.scan.unknownTask));
  assert.ok(within(row("Qwen/Qwen3-8B")).getByText("Qwen3-8B"), "an existing model is the default binding");
  assert.ok(within(row("Qwen/Qwen3-14B")).getByText(s.scan.newModel));
  assert.ok(within(row("Qwen/Qwen3-14B")).getByText(s.scan.status.new));
  assert.ok(within(row("acme/half-done")).getByText(s.scan.status.incomplete));
  assert.ok(within(row("acme/already")).getByText(s.scan.status.imported));
  assert.ok(within(row("acme/already")).getByText(s.scan.basis.huggingface));
});

test("new complete directories are ticked by default; unfinished and imported ones are not", async () => {
  api.scanStorage = (async () => firstPage) as never;
  renderModal();
  await screen.findByText("Qwen/Qwen3-8B");
  assert.equal(checkbox("Qwen/Qwen3-8B").checked, true);
  assert.equal(checkbox("Qwen/Qwen3-14B").checked, true);
  assert.equal(checkbox("acme/half-done").checked, false, "a possibly unfinished download needs a deliberate tick");
  assert.equal(checkbox("acme/already").checked, false);
  assert.equal(checkbox("acme/already").disabled, true, "an imported directory cannot be imported again");
  assert.equal(importButton().textContent?.includes("(2)"), true);
});

test("select all, untick one by keyboard, tick an unfinished one, and only-new hides imported rows", async () => {
  api.scanStorage = (async () => firstPage) as never;
  const { user } = renderModal();
  await screen.findByText("Qwen/Qwen3-8B");
  const all = screen.getByRole("checkbox", { name: s.scan.selectAll.replace("{{count}}", "2") }) as HTMLInputElement;
  assert.equal(all.checked, true);
  await user.click(all);
  assert.equal(checkbox("Qwen/Qwen3-8B").checked, false);
  assert.equal(importButton().disabled, true, "nothing ticked, nothing to import");
  await user.click(all);
  assert.equal(checkbox("Qwen/Qwen3-14B").checked, true);

  const target = checkbox("Qwen/Qwen3-14B");
  target.focus();
  await user.keyboard(" ");
  assert.equal(checkbox("Qwen/Qwen3-14B").checked, false, "space toggles the focused row");
  await user.click(checkbox("acme/half-done"));
  assert.equal(checkbox("acme/half-done").checked, true);

  await user.click(screen.getByRole("switch", { name: s.scan.onlyNew }));
  assert.equal(screen.queryByText("acme/already"), null);
  assert.ok(screen.getByText("acme/half-done"));
});

test("skipped directories are listed with their reasons", async () => {
  api.scanStorage = (async () => firstPage) as never;
  const { user } = renderModal();
  await screen.findByText("Qwen/Qwen3-8B");
  await user.click(screen.getByText(s.scan.skipped.replace("{{count}}", "2")));
  assert.ok(await screen.findByText(s.scan.skipReason.empty));
  assert.ok(screen.getByText(s.scan.skipReason.hf_hub_cache_layout));
});

test("continue scanning asks for the next cursor and appends without duplicates", async () => {
  const calls: unknown[] = [];
  api.scanStorage = (async (id: number, body: { cursor?: string }) => {
    calls.push([id, body.cursor]);
    if (!body.cursor) return firstPage;
    return { node_uuid: "n1", candidates: [candidate("Qwen/Qwen3-14B"), candidate("meta/Llama-3-8B")], skipped: [], complete: true };
  }) as never;
  const { user } = renderModal();
  await screen.findByText("Qwen/Qwen3-8B");
  const more = btn(s.scan.continue);
  assert.equal(more.disabled, false);
  await user.click(more);
  assert.ok(await screen.findByText("meta/Llama-3-8B"));
  assert.equal(screen.getAllByText("Qwen/Qwen3-14B").length, 1);
  assert.equal(checkbox("meta/Llama-3-8B").checked, true, "newly found directories are ticked too");
  assert.equal((btn(s.scan.continue)).disabled, true, "no cursor left");
  assert.ok(screen.getByText(s.scan.complete));
  assert.equal(calls.length, 2);
  assert.deepEqual(calls[1], [2, "cursor-2"]);
});

test("importing sends the ticked directories with their binding, then shows what was created and what failed", async () => {
  const sent: unknown[] = [];
  api.scanStorage = (async () => ({ ...firstPage, next_cursor: undefined, complete: true })) as never;
  api.importStorage = (async (id: number, items: unknown) => {
    sent.push([id, items]);
    return {
      imported: [{ rel_path: "Qwen/Qwen3-8B", model_id: 7 }],
      failed: [{ rel_path: "Qwen/Qwen3-14B", error: "invalid config.json" }],
    };
  }) as never;
  const { user, calls } = renderModal();
  await screen.findByText("Qwen/Qwen3-8B");
  await user.click(importButton());
  await waitFor(() => assert.equal(sent.length, 1));
  assert.deepEqual(sent[0], [2, [
    { rel_path: "Qwen/Qwen3-8B", source: "modelscope", source_id: "Qwen/Qwen3-8B", size_bytes: 15 * 1024 ** 3, model_id: 7, revision: "main", subfolder: "fp8" },
    { rel_path: "Qwen/Qwen3-14B", source: "modelscope", source_id: "Qwen/Qwen3-14B", size_bytes: 15 * 1024 ** 3 },
  ]]);
  assert.ok(await screen.findByText(s.scan.result.replace("{{imported}}", "1").replace("{{created}}", "0").replace("{{failed}}", "1")));
  assert.ok(screen.getByText(/invalid config.json/));
  assert.equal(calls.imported, 1);
  assert.ok(within(row("Qwen/Qwen3-8B")).getByText(s.scan.status.imported), "the imported row flips to imported");
  assert.equal(checkbox("Qwen/Qwen3-14B").checked, true, "a failed one stays ticked for another try");
});

test("a scan error is shown with a retry that works", async () => {
  let attempts = 0;
  api.scanStorage = (async () => {
    attempts += 1;
    if (attempts === 1) throw new Error("the storage is not ready on any online node");
    return { ...firstPage, next_cursor: undefined, complete: true };
  }) as never;
  const { user } = renderModal();
  assert.ok(await screen.findByText(s.scan.failed));
  assert.ok(screen.getByText("the storage is not ready on any online node"));
  await user.click(btn(s.scan.retry));
  assert.ok(await screen.findByText("Qwen/Qwen3-8B"));
  assert.equal(screen.queryByText(s.scan.failed), null);
});

test("an empty storage says nothing was found", async () => {
  api.scanStorage = (async () => ({ node_uuid: "n1", candidates: [], skipped: [], complete: true })) as never;
  renderModal();
  assert.ok(await screen.findByText(s.scan.empty));
  assert.equal(importButton().disabled, true);
});

test("importItem leaves out fields the scan did not report", () => {
  assert.deepEqual(importItem(candidate("a/b") as never), { rel_path: "a/b", source: "modelscope", source_id: "a/b", size_bytes: 15 * 1024 ** 3 });
});


test("conflicting metadata and excluded components explain the scan result", async () => {
 api.scanStorage = (async () => ({node_uuid:"n1",candidates:[candidate("org/model", {format:"transformers",metadata_issues:["conflicting_tasks"]})],skipped:[{rel_path:"org/pipeline/audio_vae",reason:"model_component"}],complete:true})) as never;
 const { user }=renderModal();
 await screen.findByText("org/model");
 assert.ok(within(row("org/model")).getByText(s.scan.unknownTask));
 await user.hover(within(row("org/model")).getByText(s.scan.metadataWarning));
 assert.ok(await screen.findByText(s.scan.metadataIssue.conflicting_tasks));
 await user.click(screen.getByText(s.scan.skipped.replace("{{count}}","1")));
 assert.ok(await screen.findByText(s.scan.skipReason.model_component));
});

test("read limits explain an unfinished metadata parsing instead of missing or corrupt files", async () => {
  api.scanStorage = (async () => ({
    node_uuid: "n1",
    candidates: [candidate("org/gguf", { format: "gguf", incomplete: true, metadata_issues: ["metadata_read_limit"] })],
    skipped: [{ rel_path: "org/large-index", reason: "metadata_read_limit" }],
    complete: true,
  })) as never;
  const { user } = renderModal();
  await screen.findByText("org/gguf");
  assert.equal(checkbox("org/gguf").checked, false);
  await user.hover(within(row("org/gguf")).getByText(s.scan.metadataWarning));
  assert.ok(await screen.findByText(s.scan.metadataIssue.metadata_read_limit));
  await user.click(screen.getByText(s.scan.skipped.replace("{{count}}", "1")));
  assert.ok(screen.getByText("org/large-index"));
  assert.ok(screen.getAllByText(s.scan.skipReason.metadata_read_limit).length > 0);
});

test("unverified custom components explain why a pipeline is unfinished", async () => {
  api.scanStorage = (async () => ({
    node_uuid: "n1",
    candidates: [candidate("org/pipeline", { format: "diffusers", incomplete: true, metadata_issues: ["unverified_component"] })],
    skipped: [],
    complete: true,
  })) as never;
  const { user } = renderModal();
  await screen.findByText("org/pipeline");
  assert.equal(checkbox("org/pipeline").checked, false);
  await user.hover(within(row("org/pipeline")).getByText(s.scan.metadataWarning));
  assert.ok(await screen.findByText(s.scan.metadataIssue.unverified_component));
});
