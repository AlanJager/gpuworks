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
const apiModule = await import("../src/api.ts");
const { APIError } = await import("../src/apiError.ts");
const { ConfigProvider } = await import("antd");
const { default: SecretModal } = await import("../src/pages/apikeys/SecretModal.tsx");
const { default: PlatformTokensCard } = await import("../src/pages/apikeys/PlatformTokensCard.tsx");
const k = locale.apikeys as Record<string, any>;
const p = k.platform as Record<string, any>;

type Token = import("../src/api.ts").PlatformToken;
const original = { platformTokens: apiModule.api.platformTokens, createPlatformToken: apiModule.api.createPlatformToken, deletePlatformToken: apiModule.api.deletePlatformToken };
const token = (over: Partial<Token>): Token => ({ id: 1, name: "ci", prefix: "gwp_abcdefgh", scope: "read", created_at: "2026-10-11T08:00:00Z", expires_at: null, last_used_at: null, ...over });

function stub(list: () => Promise<Token[]>) {
  const created: unknown[] = [];
  const deleted: number[] = [];
  let createError: Error | null = null;
  apiModule.api.platformTokens = async () => { const items = await list(); return { items, total: items.length }; };
  apiModule.api.createPlatformToken = async (body) => {
    created.push(body);
    if (createError) throw createError;
    return { token: "gwp_FULLSECRETVALUE", item: token({ id: 9, name: body.name, scope: body.scope }) };
  };
  apiModule.api.deletePlatformToken = async (id) => { deleted.push(id); };
  return { created, deleted, failCreateWith: (e: Error | null) => { createError = e; } };
}

function renderCard() {
  const user = userEvent.setup();
  render(<I18nextProvider i18n={i18n}><PlatformTokensCard /></I18nextProvider>);
  return user;
}

afterEach(() => {
  cleanup();
  Object.assign(apiModule.api, original);
});
after(() => closeChannels());

test("with no tokens the card says so and explains what a platform token is", async () => {
  stub(async () => []);
  renderCard();
  assert.ok(await screen.findByText(p.empty));
  assert.ok(screen.getByText(p.description));
  assert.ok(screen.getByText(/\/api\/v1\/nodes/));
});

test("tokens list their scope, use and expiry", async () => {
  stub(async () => [token({ id: 2, name: "ops", scope: "write", last_used_at: "2026-10-11T09:00:00Z", expires_at: "2027-01-01T00:00:00Z" }), token({})]);
  renderCard();
  const ops = (await screen.findByText("ops")).closest("tr")!;
  assert.ok(within(ops).getByText(p.scopes.write));
  const ci = screen.getByText("ci").closest("tr")!;
  assert.ok(within(ci).getByText(p.scopes.read));
  assert.ok(within(ci).getByText(k.never));
  assert.ok(within(ci).getByText(p.noExpiry));
  assert.ok(within(ci).getByText("gwp_abcdefgh"));
});

test("a new token defaults to read and is shown once", async () => {
  const api = stub(async () => []);
  const user = renderCard();
  await screen.findByText(p.empty);
  await user.click(screen.getByRole("button", { name: p.create }));
  const dialog = await screen.findByRole("dialog");
  await user.type(within(dialog).getByRole("textbox"), "claude-code");
  assert.equal((within(dialog).getByRole("radio", { name: p.scopes.read }) as HTMLInputElement).checked, true);
  await user.click(within(dialog).getByRole("button", { name: k.actions.create }));
  await waitFor(() => assert.deepEqual(api.created, [{ name: "claude-code", scope: "read", expires_in_days: undefined }]));
  assert.ok(await screen.findByText("gwp_FULLSECRETVALUE"));
  assert.ok(screen.getByText(k.secret.warning));
});

test("a write token with a lifetime", async () => {
  const api = stub(async () => []);
  const user = renderCard();
  await screen.findByText(p.empty);
  await user.click(screen.getByRole("button", { name: p.create }));
  const dialog = await screen.findByRole("dialog");
  await user.type(within(dialog).getByRole("textbox"), "ops");
  await user.click(within(dialog).getByRole("radio", { name: p.scopes.write }));
  await user.click(within(dialog).getByRole("combobox", { name: p.expiresAt }));
  await user.click(await screen.findByText(i18n.t("apikeys:platform.days", { count: 30 })));
  await user.click(within(dialog).getByRole("button", { name: k.actions.create }));
  await waitFor(() => assert.deepEqual(api.created, [{ name: "ops", scope: "write", expires_in_days: 30 }]));
});

test("the dialog submits from the keyboard", async () => {
  const api = stub(async () => []);
  const user = renderCard();
  await screen.findByText(p.empty);
  await user.click(screen.getByRole("button", { name: p.create }));
  const dialog = await screen.findByRole("dialog");
  await user.type(within(dialog).getByRole("textbox"), "kb");
  within(dialog).getByRole("radio", { name: p.scopes.write }).focus();
  await user.keyboard(" ");
  assert.equal((within(dialog).getByRole("radio", { name: p.scopes.write }) as HTMLInputElement).checked, true);
  within(dialog).getByRole("textbox").focus();
  await user.keyboard("{Enter}");
  await waitFor(() => assert.deepEqual(api.created, [{ name: "kb", scope: "write", expires_in_days: undefined }]));
});

test("a refused create says why and keeps the dialog", async () => {
  const api = stub(async () => []);
  api.failCreateWith(new APIError("PLATFORM_TOKEN_NOT_ALLOWED", undefined, "platform tokens are managed from a login session", 403));
  const user = renderCard();
  await screen.findByText(p.empty);
  await user.click(screen.getByRole("button", { name: p.create }));
  const dialog = await screen.findByRole("dialog");
  await user.type(within(dialog).getByRole("textbox"), "x");
  await user.click(within(dialog).getByRole("button", { name: k.actions.create }));
  assert.ok(await screen.findByText(i18n.t("common:apiError.PLATFORM_TOKEN_NOT_ALLOWED")));
  assert.ok(screen.getByRole("dialog"));
  assert.equal((within(dialog).getByRole("textbox") as HTMLInputElement).value, "x");
  api.failCreateWith(null);
  await user.click(within(dialog).getByRole("button", { name: k.actions.create }));
  assert.ok(await screen.findByText("gwp_FULLSECRETVALUE"));
  assert.equal(api.created.length, 2);
});

test("revoking asks first, then removes the token", async () => {
  let items = [token({ id: 5, name: "old" })];
  const api = stub(async () => items);
  const user = renderCard();
  const row = (await screen.findByText("old")).closest("tr")!;
  await user.click(within(row).getByRole("button", { name: p.revoke }));
  assert.ok(await screen.findByText(i18n.t("apikeys:platform.revokeConfirm", { name: "old" })));
  assert.match(i18n.t("apikeys:platform.revokeConfirm", { name: "old" }), /old/);
  items = [];
  await user.click(screen.getAllByRole("button", { name: p.revoke }).at(-1)!);
  await waitFor(() => assert.deepEqual(api.deleted, [5]));
  assert.ok(await screen.findByText(p.empty));
});

test("a failed list is shown as an error", async () => {
  stub(async () => { throw new Error("tokens offline"); });
  renderCard();
  assert.ok(await screen.findByText(/tokens offline/));
});


test("empty and whitespace names stay in the form without issuing a mutation", async () => {
  const api = stub(async () => []);
  const user = renderCard();
  await user.click(screen.getByRole("button", { name: p.create }));
  const dialog = await screen.findByRole("dialog");
  for (const name of ["", "   "]) {
    const input = within(dialog).getByRole("textbox");
    await user.clear(input);
    if (name) await user.type(input, name);
    await user.click(within(dialog).getByRole("button", { name: k.actions.create }));
    assert.ok(await within(dialog).findByText(k.create.nameRequired));
    assert.equal(api.created.length, 0);
  }
});

test("Chinese names use the trimmed UTF-8 byte limit, not character count", async () => {
  const api = stub(async () => []);
  const { i18n: zh, locale: zhLocale } = await createI18n(["apikeys"], "zh");
  const labels = zhLocale.apikeys;
  const user = userEvent.setup();
  render(<I18nextProvider i18n={zh}><PlatformTokensCard /></I18nextProvider>);
  await user.click(screen.getByRole("button", { name: labels.platform.create }));
  const dialog = await screen.findByRole("dialog");
  const input = within(dialog).getByRole("textbox");
  await user.type(input, "中".repeat(43));
  await user.click(within(dialog).getByRole("button", { name: /^创\s*建$/ }));
  assert.ok(await within(dialog).findByText("名称不能超过 128 个 UTF-8 字节（中文通常占 3 个字节）"));
  assert.equal(api.created.length, 0);
  await user.clear(input);
  const boundary = "中".repeat(42) + "ab";
  await user.type(input, `  ${boundary}  `);
  await user.click(within(dialog).getByRole("button", { name: /^创\s*建$/ }));
  await waitFor(() => assert.deepEqual(api.created, [{ name: boundary, scope: "read", expires_in_days: undefined }]));
});

test("the one-time secret surface inherits theme colors and spacing", async () => {
  render(<I18nextProvider i18n={i18n}><ConfigProvider theme={{ token: { colorFillTertiary: "#123456", paddingSM: 19, borderRadius: 7, marginSM: 17, fontFamilyCode: "Courier" } }}>
    <SecretModal secret="gwp_THEME_CHECK" onClose={() => {}} />
  </ConfigProvider></I18nextProvider>);
  const secret = await screen.findByText("gwp_THEME_CHECK");
  const style = getComputedStyle(secret);
  assert.equal(style.backgroundColor, "rgb(18, 52, 86)");
  assert.equal(style.padding, "19px");
  assert.equal(style.borderRadius, "7px");
  assert.equal(style.fontFamily, "Courier");
  assert.ok(screen.getByRole("button", { name: "Copy API token" }));
});


test("repeated Enter while creation is in flight issues one mutation", async () => {
  stub(async () => []);
  let calls = 0;
  let release!: () => void;
  const pending = new Promise<void>((resolve) => { release = resolve; });
  apiModule.api.createPlatformToken = async (body) => {
    calls++;
    await pending;
    return { token: "gwp_ONCE", item: token({ name: body.name }) };
  };
  const user = renderCard();
  await user.click(screen.getByRole("button", { name: p.create }));
  const dialog = await screen.findByRole("dialog");
  const input = within(dialog).getByRole("textbox");
  await user.type(input, "once");
  fireEvent.keyDown(input, { key: "Enter", code: "Enter", keyCode: 13 });
  fireEvent.keyDown(input, { key: "Enter", code: "Enter", keyCode: 13 });
  await waitFor(() => assert.equal(calls, 1));
  await user.keyboard("{Enter}{Enter}");
  assert.equal(calls, 1);
  release();
  assert.ok(await screen.findByText("gwp_ONCE"));
});


for (const secret of ["gwp_CLOSED_PLATFORM_SECRET", "gw-CLOSED_INFERENCE_SECRET"]) {
  test(`closing the shared one-time modal removes ${secret.startsWith("gwp_") ? "platform" : "inference"} secret from hidden DOM`, async () => {
    function Owner() {
      const [value, setValue] = React.useState<string | null>(secret);
      return <SecretModal secret={value} onClose={() => setValue(null)} />;
    }
    const user = userEvent.setup();
    render(<I18nextProvider i18n={i18n}><ConfigProvider theme={{ token: { motion: false } }}><Owner /></ConfigProvider></I18nextProvider>);
    assert.ok(await screen.findByText(secret));
    await user.click(screen.getByRole("button", { name: k.secret.saved }));
    await waitFor(() => assert.equal(document.body.textContent?.includes(secret), false));
    assert.equal(document.body.innerHTML.includes(secret), false);
  });
}
