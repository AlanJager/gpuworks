import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

const closeChannels = trackMessageChannels();
const { i18n, locale } = await createI18n(["apikeys"]);
const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { MemoryRouter } = await import("react-router-dom");
const { api } = await import("../src/api.ts");
const { default: APIKeysPage } = await import("../src/pages/APIKeysPage.tsx");
const k = locale.apikeys;
const original = { apiKeys: api.apiKeys, platformTokens: api.platformTokens, createApiKey: api.createApiKey, deleteApiKey: api.deleteApiKey };
function setup(items: import("../src/api.ts").ApiKey[] = []) {
  api.apiKeys = async () => ({ items, total: items.length });
  api.platformTokens = async () => ({ items: [], total: 0 });
  const created: string[] = [];
  api.createApiKey = async (name) => {
    created.push(name);
    return { key: "gw-ONE_TIME_SECRET", name, prefix: "gw-ONE_TIME", created_at: "2026-10-13T00:00:00Z" };
  };
  render(<MemoryRouter><I18nextProvider i18n={i18n}><APIKeysPage /></I18nextProvider></MemoryRouter>);
  return { created, user: userEvent.setup() };
}
afterEach(() => { cleanup(); Object.assign(api, original); });
after(() => closeChannels());

test("empty and whitespace inference token names do not mutate or reject outside the field", async () => {
  const { created, user } = setup();
  await user.click(screen.getByRole("button", { name: k.create.button }));
  const dialog = await screen.findByRole("dialog");
  for (const name of ["", "   "]) {
    const input = within(dialog).getByRole("textbox");
    await user.clear(input);
    if (name) await user.type(input, name);
    await user.click(within(dialog).getByRole("button", { name: k.actions.create }));
    assert.ok(await within(dialog).findByText(k.create.nameRequired));
    assert.equal(created.length, 0);
  }
});

test("keyboard creation remains single-flight and preserves the API name value", async () => {
  const { user } = setup();
  let calls = 0;
  let release!: () => void;
  const pending = new Promise<void>((resolve) => { release = resolve; });
  api.createApiKey = async (name) => {
    calls++;
    assert.equal(name, "my-app");
    await pending;
    return { key: "gw-ONE_TIME_SECRET", name, prefix: "gw-ONE_TIME", created_at: "2026-10-13T00:00:00Z" };
  };
  await user.click(screen.getByRole("button", { name: k.create.button }));
  const dialog = await screen.findByRole("dialog");
  const input = within(dialog).getByRole("textbox");
  await user.type(input, "my-app");
  fireEvent.keyDown(input, { key: "Enter", code: "Enter", keyCode: 13 });
  fireEvent.keyDown(input, { key: "Enter", code: "Enter", keyCode: 13 });
  await waitFor(() => assert.equal(calls, 1));
  release();
  assert.ok(await screen.findByText("gw-ONE_TIME_SECRET"));
});

test("server refusal preserves the inference token draft and allows a corrected retry", async () => {
  const { user } = setup();
  api.createApiKey = async () => { throw new Error("database is read-only"); };
  await user.click(screen.getByRole("button", { name: k.create.button }));
  const dialog = await screen.findByRole("dialog");
  const input = within(dialog).getByRole("textbox");
  await user.type(input, "retained");
  await user.click(within(dialog).getByRole("button", { name: k.actions.create }));
  assert.ok(await screen.findByText(/database is read-only/));
  assert.equal((input as HTMLInputElement).value, "retained");
  api.createApiKey = async (name) => ({ key: "gw-RETRIED_SECRET", name, prefix: "gw-RETRIED", created_at: "2026-10-13T00:00:00Z" });
  await user.click(within(dialog).getByRole("button", { name: k.actions.create }));
  assert.ok(await screen.findByText("gw-RETRIED_SECRET"));
});

test("delete confirmation names the token and explains the last-key anonymous-mode exception", async () => {
  const { user } = setup([{ name: "production-app", prefix: "gw-production", created_at: "2026-10-13T00:00:00Z" }]);
  const row = (await screen.findByText("production-app")).closest("tr")!;
  await user.click(within(row).getByRole("button", { name: k.actions.delete }));
  const text = i18n.t("apikeys:delete.confirm", { name: "production-app" });
  assert.match(text, /production-app/);
  assert.match(text, /revoked/);
  assert.match(text, /no tokens remain.*anonymous access/);
  assert.ok(await screen.findByText(text));
});
