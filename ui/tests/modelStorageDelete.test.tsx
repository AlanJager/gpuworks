import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import { createI18n } from "./helpers/dom.ts";
import React from "react";
import { I18nextProvider } from "react-i18next";

const { i18n, locale } = await createI18n(["models"]);
const models = locale.models;
const common = locale.common;
const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const userEventModule = await import("@testing-library/user-event");
const userEvent = userEventModule.default ?? userEventModule;
const apiModule = await import("../src/api.ts");
const { APIError } = await import("../src/apiError.ts");
const { ModelStorageFileDelete } = await import("../src/pages/models/ModelStorageFileDelete.tsx");

const api = apiModule.api;
const original = { deleteModelInStorage: api.deleteModelInStorage };
afterEach(() => { cleanup(); Object.assign(api, original); });

const fill = (template: string, values: Record<string, string | number>) => template.replace(/\{\{(\w+)\}\}/g, (_, key) => String(values[key]));
const model = { id: 9, source: "modelscope", source_id: "Qwen/Qwen3-8B", name: "Qwen3-8B" };
function presence(over: Record<string, unknown> = {}) {
  return { storage_id: 3, storage_name: "archive", storage_type: "nfs", path: "Qwen/Qwen3-8B", state: "present", acquisition: "registered", size_bytes: 1, downloaded: 1, progress: 1, rate_bps: 0, ready_nodes: 128, cached_nodes: 0, writable: true, ...over } as never;
}
function renderDelete(p = presence(), onDone = () => {}) {
  const row = p as unknown as Record<string, any>;
  return render(<I18nextProvider i18n={i18n}><ModelStorageFileDelete modelId={model.id} storageId={row.storage_id} storageName={row.storage_name} storageType={row.storage_type} writable={row.writable} cachedNodes={row.cached_nodes} onDone={onDone} /></I18nextProvider>);
}
const sharedByOthers = () => new APIError("STORAGE_SHARED_BY_OTHER_CONTROL_PLANES", {
  storage: "archive",
  control_planes: [{ id: "cp-a", name: "lab-east", last_seen_seconds: 120, fresh: true }, { id: "cp-b", name: "lab-west", last_seen_seconds: 7200, fresh: false }],
}, "other control planes use it", 409);
const dialog = () => screen.findByRole("dialog");

test("the confirmation states what an nfs delete reaches and what an nfs_cache delete also clears", async () => {
  api.deleteModelInStorage = (async () => {}) as never;
  const view = renderDelete();
  fireEvent.click(screen.getByRole("button", { name: models.action.deleteModelFiles }));
  assert.ok(within(await dialog()).getByText(fill(models.storageDelete.impactNfs, { storage: "archive" })));
  assert.equal(within(await dialog()).queryByRole("checkbox"), null, "no force option until the server asks for it");
  view.unmount();

  renderDelete(presence({ storage_name: "team-cache", storage_type: "nfs_cache", cached_nodes: 3 }));
  fireEvent.click(screen.getByRole("button", { name: models.action.deleteModelFiles }));
  assert.ok(within(await dialog()).getByText(fill(models.storageDelete.impactCache, { storage: "team-cache", count: 3 })));
});

test("a read-only storage offers no delete: the button is disabled and nothing is called", async () => {
  const calls: unknown[][] = [];
  api.deleteModelInStorage = (async (...args: unknown[]) => { calls.push(args); }) as never;
  renderDelete(presence({ storage_name: "library", writable: false }));
  const button = screen.getByRole("button", { name: models.action.deleteModelFiles }) as HTMLButtonElement;
  assert.equal(button.disabled, true);
  fireEvent.click(button);
  await new Promise((resolve) => setTimeout(resolve, 50));
  assert.equal(screen.queryByRole("dialog"), null);
  assert.deepEqual(calls, []);
});

test("a plain delete calls the API without force, then confirms and refreshes", async () => {
  const calls: unknown[][] = [];
  api.deleteModelInStorage = (async (...args: unknown[]) => { calls.push(args); }) as never;
  let done = 0;
  renderDelete(presence(), () => { done += 1; });
  fireEvent.click(screen.getByRole("button", { name: models.action.deleteModelFiles }));
  fireEvent.click(within(await dialog()).getByRole("button", { name: models.storageDelete.confirm }));
  await waitFor(() => assert.equal(done, 1));
  assert.deepEqual(calls, [[9, 3, false]]);
  assert.ok(await screen.findByText(fill(models.storageDelete.deleted, { storage: "archive" })));
});

test("when other gpuworks use the storage the dialog lists them and force must be ticked before the retry", async () => {
  const calls: unknown[][] = [];
  api.deleteModelInStorage = (async (...args: unknown[]) => {
    calls.push(args);
    if (!args[2]) throw sharedByOthers();
  }) as never;
  let done = 0;
  renderDelete(presence(), () => { done += 1; });
  fireEvent.click(screen.getByRole("button", { name: models.action.deleteModelFiles }));
  fireEvent.click(within(await dialog()).getByRole("button", { name: models.storageDelete.confirm }));

  const warning = await screen.findByText(models.storageDelete.sharedTitle);
  assert.ok(warning);
  const box = await dialog();
  assert.ok(within(box).getByText("lab-east"));
  assert.ok(within(box).getByText(new RegExp(fill(models.storageDelete.lastSeen, { ago: fill(common.ago.minutes, { value: 2 }) }))));
  assert.ok(within(box).getByText("lab-west"));
  assert.ok(within(box).getByText(new RegExp(fill(models.storageDelete.lastSeen, { ago: fill(common.ago.hours, { value: 2 }) }))));
  // antd keeps the spinner icon (and so "loading" in the accessible name) around after a request, jsdom never ends its exit animation.
  const forceName = new RegExp(`${models.storageDelete.forceConfirm}$`);
  const confirm = await within(box).findByRole("button", { name: forceName }) as HTMLButtonElement;
  assert.equal(confirm.disabled, true, "force is not on yet");
  fireEvent.click(confirm);
  assert.equal(calls.length, 1, "a disabled confirm does nothing");

  const force = within(box).getByRole("checkbox", { name: models.storageDelete.force });
  force.focus();
  await userEvent.keyboard(" ");
  await waitFor(() => assert.equal((within(box).getByRole("button", { name: forceName }) as HTMLButtonElement).disabled, false));
  fireEvent.click(within(box).getByRole("button", { name: forceName }));
  await waitFor(() => assert.equal(done, 1));
  assert.deepEqual(calls, [[9, 3, false], [9, 3, true]]);
});

test("any other error stays in the dialog as an alert and nothing is refreshed", async () => {
  api.deleteModelInStorage = (async () => { throw new APIError("MODEL_IN_USE", {}, "in use", 409); }) as never;
  let done = 0;
  renderDelete(presence(), () => { done += 1; });
  fireEvent.click(screen.getByRole("button", { name: models.action.deleteModelFiles }));
  fireEvent.click(within(await dialog()).getByRole("button", { name: models.storageDelete.confirm }));
  const alert = await within(await dialog()).findByRole("alert");
  assert.ok(alert.textContent?.includes(common.apiError.MODEL_IN_USE));
  assert.equal(done, 0);
  assert.equal(within(await dialog()).queryByRole("checkbox"), null, "an in-use refusal cannot be forced");
});

test("cancelling closes the dialog without deleting and a reopened dialog starts clean", async () => {
  let calls = 0;
  api.deleteModelInStorage = (async () => { calls += 1; throw sharedByOthers(); }) as never;
  renderDelete();
  fireEvent.click(screen.getByRole("button", { name: models.action.deleteModelFiles }));
  fireEvent.click(within(await dialog()).getByRole("button", { name: models.storageDelete.confirm }));
  await screen.findByText(models.storageDelete.sharedTitle);
  fireEvent.click(within(await dialog()).getByRole("button", { name: models.action.cancel }));
  fireEvent.click(screen.getByRole("button", { name: models.action.deleteModelFiles }));
  assert.ok(await within(await dialog()).findByRole("button", { name: new RegExp(`${models.storageDelete.confirm}$`) }), "back to the plain confirmation");
  assert.equal(screen.queryByText(models.storageDelete.sharedTitle), null, "the warning of the earlier attempt is gone");
  assert.equal(calls, 1);
});
