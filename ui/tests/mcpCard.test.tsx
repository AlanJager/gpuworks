import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

const closeChannels = trackMessageChannels();
const { i18n, locale } = await createI18n(["settings"]);
const { cleanup, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { MemoryRouter } = await import("react-router-dom");
const apiModule = await import("../src/api.ts");
const { APIError } = await import("../src/apiError.ts");
const { default: MCPCard } = await import("../src/pages/settings/MCPCard.tsx");
const m = locale.settings.mcp as Record<string, string>;

type Status = import("../src/api.ts").MCPStatus;
const original = { mcpStatus: apiModule.api.mcpStatus, putMCPSettings: apiModule.api.putMCPSettings, me: apiModule.api.me };
const status = (enabled: boolean): Status => ({ enabled, path: "/api/v1/mcp", protocol_version: "2025-06-18", tools: ["cluster_overview", "call_api"] });

function stub({ role = "admin", load }: { role?: string; load: () => Promise<Status> }) {
  const puts: boolean[] = [];
  let putError: Error | null = null;
  let loads = 0;
  apiModule.api.me = (async () => ({ id: 1, username: role, role })) as typeof apiModule.api.me;
  apiModule.api.mcpStatus = async () => { loads++; return load(); };
  apiModule.api.putMCPSettings = async (enabled) => {
    puts.push(enabled);
    if (putError) throw putError;
    return status(enabled);
  };
  return { puts, loads: () => loads, failPutWith: (e: Error) => { putError = e; } };
}

function renderCard() {
  const user = userEvent.setup();
  render(<I18nextProvider i18n={i18n}><MemoryRouter><MCPCard /></MemoryRouter></I18nextProvider>);
  return user;
}
const toggle = () => screen.getByRole("switch", { name: m.switch });

afterEach(() => {
  cleanup();
  Object.assign(apiModule.api, original);
});
after(() => closeChannels());

test("the card loads the status, then shows the endpoint, the tools and how to connect", async () => {
  let resolve!: (s: Status) => void;
  stub({ load: () => new Promise<Status>((r) => { resolve = r; }) });
  renderCard();
  assert.ok(toggle().classList.contains("ant-switch-loading"));
  assert.equal((toggle() as HTMLButtonElement).disabled, true);
  resolve(status(false));
  await waitFor(() => assert.ok(!toggle().classList.contains("ant-switch-loading")));
  assert.equal(toggle().getAttribute("aria-checked"), "false");
  assert.ok(screen.getByText(m.off));
  const endpoint = `${window.location.origin}/api/v1/mcp`;
  assert.ok(screen.getAllByText(endpoint, { exact: false }).length >= 1);
  assert.ok(screen.getByText("cluster_overview") && screen.getByText("call_api"));
  assert.ok(screen.getByText(/claude mcp add --transport http gpuworks/));
  assert.equal(screen.getByRole("link", { name: m.createToken }).getAttribute("href"), "/apikeys");
  assert.ok(screen.getByText(m.dataNotice));
});

test("an admin turns the service on from the keyboard", async () => {
  const api = stub({ load: async () => status(false) });
  const user = renderCard();
  await waitFor(() => assert.equal((toggle() as HTMLButtonElement).disabled, false));
  toggle().focus();
  await user.keyboard(" ");
  await waitFor(() => assert.deepEqual(api.puts, [true]));
  await waitFor(() => assert.equal(toggle().getAttribute("aria-checked"), "true"));
  assert.ok(await screen.findByText(m.turnedOn));
  assert.ok(screen.getByText(m.on));
});

test("a refused switch keeps the state and reports the reason", async () => {
  const api = stub({ load: async () => status(false) });
  api.failPutWith(new APIError("ADMINISTRATOR_REQUIRED", undefined, "administrator role required", 403));
  const user = renderCard();
  await waitFor(() => assert.equal((toggle() as HTMLButtonElement).disabled, false));
  await user.click(toggle());
  await waitFor(() => assert.equal(api.puts.length, 1));
  assert.ok(await screen.findByText(/administrator/i));
  assert.equal(toggle().getAttribute("aria-checked"), "false");
});

test("a non-admin sees the status but cannot switch it", async () => {
  stub({ role: "user", load: async () => status(true) });
  renderCard();
  assert.ok(await screen.findByText(m.adminOnly));
  await waitFor(() => assert.equal(toggle().getAttribute("aria-checked"), "true"));
  assert.equal((toggle() as HTMLButtonElement).disabled, true);
});

test("a failed load is reported and can be retried", async () => {
  let fail = true;
  const api = stub({ load: async () => {
    if (fail) throw new Error("status offline");
    return status(true);
  } });
  const user = renderCard();
  assert.ok(await screen.findByText(/status offline/));
  assert.equal((toggle() as HTMLButtonElement).disabled, true);
  fail = false;
  await user.click(screen.getByRole("button", { name: locale.common.actions.retry }));
  await waitFor(() => assert.equal(toggle().getAttribute("aria-checked"), "true"));
  assert.equal(api.loads(), 2);
  assert.equal(screen.queryByText(/status offline/), null);
});

test("copying generic JSON with Enter preserves the collapsed or expanded state", async () => {
  stub({ load: async () => status(false) });
  const user = renderCard();
  await waitFor(() => assert.equal((toggle() as HTMLButtonElement).disabled, false));
  const header = screen.getByRole("button", { name: new RegExp(m.otherClients) });
  const copy = within(header).getByRole("button", { name: "Copy" });
  assert.equal(header.getAttribute("aria-expanded"), "false");
  copy.focus();
  await user.keyboard("{Enter}");
  assert.equal(header.getAttribute("aria-expanded"), "false");
  await user.click(header);
  assert.equal(header.getAttribute("aria-expanded"), "true");
  copy.focus();
  await user.keyboard("{Enter}");
  assert.equal(header.getAttribute("aria-expanded"), "true");
  await user.click(header);
  assert.equal(header.getAttribute("aria-expanded"), "false");
});
