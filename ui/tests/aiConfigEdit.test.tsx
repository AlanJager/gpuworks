import { dom, createI18n } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { list } from "./helpers/listPage.ts";

const { i18n, locale } = await createI18n(["settings", "common"], "en");
const { cleanup, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { api } = await import("../src/api.ts");
const AICard = (await import("../src/pages/settings/AIConfigCard.tsx")).default;

const ai = locale.settings.ai as Record<string, any>;
const common = locale.settings.common as Record<string, any>;

/** stored is one service as the API lists it: six fields, no key. */
function stored(overrides: Record<string, unknown> = {}) {
  return {
    id: 1,
    name: "primary",
    preset: "deepseek",
    base_url: "https://api.deepseek.com/v1",
    model: "deepseek-v4-flash",
    is_default: true,
    ...overrides,
  };
}

/** stubAPI replaces the AI calls and records what the card sent. */
function stubAPI(rows: ReturnType<typeof stored>[]) {
  const original = {
    aiConfigs: api.aiConfigs,
    updateAIConfig: api.updateAIConfig,
    createAIConfig: api.createAIConfig,
    deleteAIConfig: api.deleteAIConfig,
  };
  const calls: {
    updated: { id: number; body: Record<string, unknown> }[];
    created: Record<string, unknown>[];
    deleted: number[];
  } = { updated: [], created: [], deleted: [] };
  let current = rows;
  api.aiConfigs = async () => list(current);
  api.updateAIConfig = async (id, body) => {
    calls.updated.push({ id, body: body as Record<string, unknown> });
    current = current.map((row) => (row.id === id ? ({ ...row, ...body } as ReturnType<typeof stored>) : row));
    return current.find((row) => row.id === id)!;
  };
  api.createAIConfig = async (body) => {
    calls.created.push(body as unknown as Record<string, unknown>);
    return stored(body as unknown as Record<string, unknown>);
  };
  api.deleteAIConfig = async (id) => {
    calls.deleted.push(id);
    current = current.filter((row) => row.id !== id);
  };
  return {
    calls,
    restore: () => Object.assign(api, original),
  };
}

const fakes: { restore(): void }[] = [];
afterEach(() => {
  cleanup();
  while (fakes.length) fakes.pop()!.restore();
});

function renderCard() {
  return render(
    <I18nextProvider i18n={i18n}>
      <AICard />
    </I18nextProvider>,
  );
}

test("the list shows each service's address beside its name, preset and model", async () => {
  fakes.push(stubAPI([stored()]));
  renderCard();
  // The address the assistant will call is part of the row, so an operator can
  // tell two services apart without opening them.
  assert.ok(await screen.findByText("https://api.deepseek.com/v1"));
  assert.ok(screen.getByText("primary"));
  assert.ok(screen.getByText(ai.model));
  assert.ok(screen.getByText(ai.presetName.deepseek));
});

test("editing a service prefills it and changes only what was edited", async () => {
  const fake = stubAPI([stored()]);
  fakes.push(fake);
  renderCard();
  const user = userEvent.setup({ document: dom.window.document });
  await screen.findByText("primary");
  await user.click(screen.getByRole("button", { name: common.edit }));
  const dialog = await screen.findByRole("dialog");
  // Prefilled from the stored service: the dialog opens on its values, so an
  // operator changes one field instead of retyping the service.
  const name = within(dialog).getByLabelText(ai.name) as HTMLInputElement;
  const address = within(dialog).getByLabelText(ai.baseUrl) as HTMLInputElement;
  const model = within(dialog).getByLabelText(ai.model) as HTMLInputElement;
  const key = within(dialog).getByLabelText(ai.upstreamKey) as HTMLInputElement;
  assert.equal(name.value, "primary");
  assert.equal(address.value, "https://api.deepseek.com/v1");
  assert.equal(model.value, "deepseek-v4-flash");
  // The key is never sent to the console, so the field starts blank and says
  // what blank means.
  assert.equal(key.value, "");
  assert.equal(key.getAttribute("placeholder"), ai.keyKeep);
  await user.clear(name);
  await user.type(name, "renamed");
  await user.click(within(dialog).getByRole("button", { name: common.save }));
  await waitFor(() => assert.equal(fake.calls.updated.length, 1));
  const { id, body } = fake.calls.updated[0];
  assert.equal(id, 1);
  assert.equal(body.name, "renamed");
  // A blank key is left out entirely, so the server keeps the stored key.
  assert.ok(!("api_key" in body), `an edit sent a blank key: ${JSON.stringify(body)}`);
  assert.equal(body.base_url, "https://api.deepseek.com/v1");
});

test("a new key typed on an edit is sent, and the row follows it", async () => {
  const fake = stubAPI([stored()]);
  fakes.push(fake);
  renderCard();
  const user = userEvent.setup({ document: dom.window.document });
  await screen.findByText("primary");
  await user.click(screen.getByRole("button", { name: common.edit }));
  const dialog = await screen.findByRole("dialog");
  await user.type(within(dialog).getByLabelText(ai.upstreamKey), "sk-replaced");
  await user.click(within(dialog).getByRole("button", { name: common.save }));
  await waitFor(() => assert.equal(fake.calls.updated.length, 1));
  assert.equal(fake.calls.updated[0].body.api_key, "sk-replaced");
});

test("a new service still requires a key", async () => {
  const fake = stubAPI([]);
  fakes.push(fake);
  renderCard();
  const user = userEvent.setup({ document: dom.window.document });
  await screen.findByText(ai.empty);
  await user.click(screen.getByRole("button", { name: common.add }));
  const dialog = await screen.findByRole("dialog");
  await user.type(within(dialog).getByLabelText(ai.name), "fresh");
  await user.click(within(dialog).getByRole("button", { name: common.add }));
  assert.ok(await within(dialog).findByText(ai.keyRequired));
  assert.equal(fake.calls.created.length, 0);
  await user.type(within(dialog).getByLabelText(ai.upstreamKey), "sk-fresh");
  await user.click(within(dialog).getByRole("button", { name: common.add }));
  await waitFor(() => assert.equal(fake.calls.created.length, 1));
  assert.deepEqual(fake.calls.created[0], {
    name: "fresh",
    preset: "deepseek",
    base_url: "https://api.deepseek.com",
    api_key: "sk-fresh",
    model: "deepseek-v4-flash",
    is_default: true,
  });
});

test("the form says where the address ends", async () => {
  fakes.push(stubAPI([]));
  renderCard();
  const user = userEvent.setup({ document: dom.window.document });
  await screen.findByText(ai.empty);
  await user.click(screen.getByRole("button", { name: common.add }));
  const dialog = await screen.findByRole("dialog");
  // A wrong address is the failure the bug reported; the hint is next to the
  // field, not only in a message after the assistant fails.
  assert.ok(within(dialog).getByText(ai.baseUrlHint));
});
