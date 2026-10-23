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
const original = { apiKeys: api.apiKeys, platformTokens: api.platformTokens, createApiKey: api.createApiKey, revokeApiKey: api.revokeApiKey };
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

test("revoke confirmation names the token and explains the last-key anonymous-mode exception", async () => {
  const { user } = setup([{ name: "production-app", prefix: "gw-production", created_at: "2026-10-13T00:00:00Z" }]);
  const row = (await screen.findByText("production-app")).closest("tr")!;
  await user.click(within(row).getByRole("button", { name: k.actions.revoke }));
  const text = i18n.t("apikeys:revoke.confirm", { name: "production-app" });
  assert.match(text, /production-app/);
  assert.match(text, /lose access/);
  assert.match(text, /no active tokens remain.*anonymous access/);
  assert.ok(await screen.findByText(text));
});

test("revoking calls the API once and a revoked token is hidden until asked for", async () => {
  const revoked: string[] = [];
  const items: import("../src/api.ts").ApiKey[] = [
    { name: "live-app", prefix: "gw-live00000", created_at: "2026-10-13T00:00:00Z" },
    { name: "old-app", prefix: "gw-old000000", created_at: "2026-10-01T00:00:00Z", revoked_at: "2026-10-20T08:00:00Z" },
  ];
  const { user } = setup(items);
  api.revokeApiKey = async (prefix) => { revoked.push(prefix); };
  const row = (await screen.findByText("live-app")).closest("tr")!;
  assert.ok(within(row).getByText(k.status.active));
  assert.equal(screen.queryByText("old-app"), null);
  const toggle = screen.getByRole("switch", { name: i18n.t("apikeys:showRevoked", { count: 1 }) });
  await user.click(toggle);
  const old = (await screen.findByText("old-app")).closest("tr")!;
  assert.ok(within(old).getByText(k.status.revoked));
  assert.equal(within(old).queryByRole("button", { name: k.actions.revoke }), null);
  await user.click(within(row).getByRole("button", { name: k.actions.revoke }));
  // The confirmation's own button is the last one with that name.
  await waitFor(() => assert.ok(screen.getAllByRole("button", { name: k.actions.revoke }).length > 1));
  await user.click(screen.getAllByRole("button", { name: k.actions.revoke }).at(-1)!);
  await waitFor(() => assert.deepEqual(revoked, ["gw-live00000"]));
});

test("without revoked tokens there is no revoked toggle", async () => {
  setup([{ name: "live-app", prefix: "gw-live00000", created_at: "2026-10-13T00:00:00Z" }]);
  await screen.findByText("live-app");
  assert.equal(screen.queryByRole("switch"), null);
});
