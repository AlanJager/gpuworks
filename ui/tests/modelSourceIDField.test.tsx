import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import { createI18n } from "./helpers/dom.ts";
import React from "react";
import { I18nextProvider } from "react-i18next";

const { i18n, locale } = await createI18n(["models"]);
const models = locale.models;
const { cleanup, fireEvent, render, screen, waitFor } = await import("@testing-library/react");
const apiModule = await import("../src/api.ts");
const { default: ModelSourceIDField } = await import("../src/pages/models/ModelSourceIDField.tsx");

const api = apiModule.api;
const original = { updateModelSourceID: api.updateModelSourceID };
afterEach(() => { cleanup(); Object.assign(api, original); });

const fill = (template: string, values: Record<string, string | number>) => template.replace(/\{\{(\w+)\}\}/g, (_, key) => String(values[key]));

function local(over: Record<string, unknown> = {}) {
  return { id: 4, source: "local", source_id: "Qwen3-0.6B", source_id_editable: true, revision: "local", name: "qwen", arch: "", quant_label: "", weight_gib: 1, ...over } as never;
}

function mount(model: unknown, onChanged: (model: unknown) => void = () => {}) {
  return render(<I18nextProvider i18n={i18n}><ModelSourceIDField model={model as never} onChanged={onChanged as never} /></I18nextProvider>);
}

function edit(value: string, key: "Enter" | "Escape" = "Enter") {
  fireEvent.click(screen.getByRole("button", { name: models.detail.editSourceID }));
  const box = screen.getByRole("textbox");
  fireEvent.change(box, { target: { value } });
  fireEvent.keyDown(box, { key, keyCode: key === "Enter" ? 13 : 27 });
  fireEvent.keyUp(box, { key, keyCode: key === "Enter" ? 13 : 27 });
}

test("a model whose source id is a repository identity shows it without an edit control", () => {
  mount(local({ source: "huggingface", source_id: "Qwen/Qwen3-0.6B", source_id_editable: undefined }));
  assert.ok(screen.getByText("Qwen/Qwen3-0.6B"));
  assert.equal(screen.queryByRole("button", { name: models.detail.editSourceID }), null);
  assert.equal(screen.queryByText(models.detail.editSourceIDHint), null);
});

test("Enter saves the trimmed source id and hands the updated model back", async () => {
  const calls: Array<[number, string]> = [];
  api.updateModelSourceID = (async (id: number, sourceID: string) => {
    calls.push([id, sourceID]);
    return { ...(local() as object), source_id: sourceID };
  }) as never;
  const changed: unknown[] = [];
  mount(local(), (model) => changed.push(model));
  assert.ok(screen.getByText(models.detail.editSourceIDHint));
  edit("  Qwen/Qwen3-0.6B  ");
  await waitFor(() => assert.equal(changed.length, 1));
  assert.deepEqual(calls, [[4, "Qwen/Qwen3-0.6B"]]);
  assert.equal((changed[0] as { source_id: string }).source_id, "Qwen/Qwen3-0.6B");
  await waitFor(() => assert.ok(screen.getByText(models.detail.message.sourceIDUpdated)));
});

test("Escape, an unchanged value and an empty value send nothing", async () => {
  let calls = 0;
  api.updateModelSourceID = (async () => { calls += 1; return local(); }) as never;
  mount(local());
  edit("Qwen/Qwen3-0.6B", "Escape");
  edit("Qwen3-0.6B");
  edit("   ");
  await new Promise((resolve) => setTimeout(resolve, 20));
  assert.equal(calls, 0);
  assert.ok(screen.getByText("Qwen3-0.6B"));
});

test("a refused change reports the server's reason and keeps the old id", async () => {
  api.updateModelSourceID = (async () => { throw new Error("Another model already uses this source ID (model 7)"); }) as never;
  const changed: unknown[] = [];
  mount(local(), (model) => changed.push(model));
  edit("Acme/Taken");
  const expected = fill(models.detail.message.sourceIDFailed, { detail: "Another model already uses this source ID (model 7)" });
  await waitFor(() => assert.ok(screen.getByText(expected)));
  assert.equal(changed.length, 0);
  assert.ok(screen.getByText("Qwen3-0.6B"));
});
