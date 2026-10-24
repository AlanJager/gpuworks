import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";
import * as fx from "./helpers/analyticsFixtures.ts";

const closeChannels = trackMessageChannels();
const { i18n, locale } = await createI18n(["apikeys"]);
const { cleanup, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { MemoryRouter, Route, Routes, useLocation } = await import("react-router-dom");
const { api } = await import("../src/api.ts");
const { analyticsApi } = await import("../src/analyticsApi.ts");
const { default: APIKeysPage } = await import("../src/pages/APIKeysPage.tsx");
const k = locale.apikeys;
const original = { apiKeys: api.apiKeys, platformTokens: api.platformTokens };
const originalKeyUsage = analyticsApi.keyUsage;

const keys = [
  { name: "live-app", prefix: "gw-live00000", created_at: "2026-10-01T00:00:00Z" },
  { name: "idle-app", prefix: "gw-idle00000", created_at: "2026-10-01T00:00:00Z" },
  { name: "unused-app", prefix: "gw-none00000", created_at: "2026-10-01T00:00:00Z" },
];

function Probe() {
  const location = useLocation();
  return <div data-testid="loc">{location.pathname}{location.search}</div>;
}

function mount(keyUsage: () => Promise<unknown>) {
  api.apiKeys = async () => ({ items: keys, total: keys.length });
  api.platformTokens = async () => ({ items: [], total: 0 });
  analyticsApi.keyUsage = keyUsage as typeof analyticsApi.keyUsage;
  render(
    <I18nextProvider i18n={i18n}>
      <MemoryRouter initialEntries={["/apikeys"]}>
        <Routes>
          <Route path="/apikeys" element={<APIKeysPage />} />
          <Route path="/analytics/:tab" element={<div>analytics page</div>} />
        </Routes>
        <Probe />
      </MemoryRouter>
    </I18nextProvider>,
  );
  return userEvent.setup();
}

const compact = (n: number) => new Intl.NumberFormat("en", { notation: "compact", maximumFractionDigits: 2 }).format(n);

// A key's cell under a column, found by the column's header.
function cell(rowText: string, header: string): HTMLElement {
  const table = (screen.getAllByRole("table").find((t) => t.textContent!.includes(rowText)))!;
  const heads = [...table.querySelectorAll("thead th")].map((th) => th.textContent!.trim());
  const col = heads.findIndex((h) => h.includes(header));
  assert.ok(col >= 0, `a ${header} column among ${heads.join("|")}`);
  const row = [...table.querySelectorAll("tbody tr")].find((r) => r.textContent!.includes(rowText))!;
  return row.querySelectorAll("td")[col] as HTMLElement;
}

afterEach(() => {
  cleanup();
  Object.assign(api, original);
  analyticsApi.keyUsage = originalKeyUsage;
});
after(() => closeChannels());

test("the table has today's and the last 7 days' tokens (input + output) per key, a dash for a key with no usage record", async () => {
  mount(async () => fx.keyUsage);
  await screen.findByText("live-app");
  await waitFor(() => assert.ok(within(cell("gw-live00000", k.table.tokensToday)).queryByRole("link")));
  assert.equal(cell("gw-live00000", k.table.tokensToday).textContent, compact(1_230_000));
  assert.equal(cell("gw-live00000", k.table.tokens7d).textContent, compact(9_250_000));
  // A key whose usage record shows nothing today has a real zero today, and a real figure for the week.
  assert.equal(cell("gw-idle00000", k.table.tokensToday).textContent, compact(0));
  assert.ok(within(cell("gw-idle00000", k.table.tokensToday)).queryByRole("link") === null, "nothing to open for a zero");
  assert.equal(cell("gw-idle00000", k.table.tokens7d).textContent, compact(3500));
  // No record at all is not a zero.
  assert.equal(cell("gw-none00000", k.table.tokensToday).textContent, "—");
  assert.equal(cell("gw-none00000", k.table.tokens7d).textContent, "—");
});

test("hovering a number breaks it into input, cache hits and output", async () => {
  const user = mount(async () => fx.keyUsage);
  await screen.findByText("live-app");
  const link = await within(cell("gw-live00000", k.table.tokens7d)).findByRole("link");
  await user.hover(link);
  const text = i18n.t("apikeys:tokens.tooltip", { input: "9,000,000", cached: "6,000,000", output: "250,000" });
  assert.ok(await screen.findByText(text));
});

test("a number opens that key in Usage analytics over the same period", async () => {
  const user = mount(async () => fx.keyUsage);
  await screen.findByText("live-app");
  const week = await within(cell("gw-live00000", k.table.tokens7d)).findByRole("link");
  assert.equal(week.getAttribute("href"), "/analytics/usage?keyPrefix=gw-live00000&range=7d");
  const today = within(cell("gw-live00000", k.table.tokensToday)).getByRole("link");
  assert.equal(today.getAttribute("href"), "/analytics/usage?keyPrefix=gw-live00000&range=today");
  await user.click(week);
  await screen.findByText("analytics page");
  assert.equal(screen.getByTestId("loc").textContent, "/analytics/usage?keyPrefix=gw-live00000&range=7d");
});

test("while the usage is loading, and when it fails, the keys table stays and the columns read as dashes", async () => {
  let release!: () => void;
  const gate = new Promise<void>((resolve) => { release = resolve; });
  let failed = false;
  mount(async () => { await gate; failed = true; throw new Error("stats unavailable"); });
  await screen.findByText("live-app");
  assert.equal(cell("gw-live00000", k.table.tokensToday).textContent, "—");
  release();
  await waitFor(() => assert.ok(failed, "the usage request failed"));
  // The failure changes nothing on screen: every key is still there, every figure a dash.
  await new Promise((resolve) => setTimeout(resolve, 50));
  for (const prefix of ["gw-live00000", "gw-idle00000", "gw-none00000"]) {
    assert.equal(cell(prefix, k.table.tokensToday).textContent, "—");
    assert.equal(cell(prefix, k.table.tokens7d).textContent, "—");
  }
  assert.ok(screen.getByText("idle-app"));
  assert.ok(screen.getByText("unused-app"));
  assert.ok(screen.queryByText(/stats unavailable/) === null, "the usage failure is not shown as the keys' error");
});
