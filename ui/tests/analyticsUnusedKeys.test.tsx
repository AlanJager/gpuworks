import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";
import * as fx from "./helpers/analyticsFixtures.ts";

const closeChannels = trackMessageChannels();
const { i18n, locale } = await createI18n(["analytics", "nodes"]);
fx.installCanvas(dom);
const { cleanup, fireEvent, render, screen, waitFor } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { MemoryRouter, Route, Routes } = await import("react-router-dom");
const { api } = await import("../src/api.ts");
const { default: AnalyticsPage } = await import("../src/pages/analytics/AnalyticsPage.tsx");
const original = { dataRetention: api.dataRetention, deployments: api.deployments };
const originalFetch = globalThis.fetch;
const calls: URL[] = [];
let keysStatus = 200;

function mount(url = "/analytics/usage") {
  api.dataRetention = async () => fx.retentionView();
  api.deployments = (async () => fx.deployments) as typeof api.deployments;
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    const request = new URL(String(input), "http://localhost");
    calls.push(request);
    if (request.pathname === "/api/v1/apikeys") return Response.json(keysStatus === 200 ? { items: [
      { id: 81, user_id: 7, name: "unused-all-models", prefix: "gw-unusedall", created_at: "2026-10-01T00:00:00Z", all_deployments: true, deployments: [] },
      { id: 82, user_id: 7, name: "unused-scoped", prefix: "gw-unusedscope", created_at: "2026-10-01T00:00:00Z", all_deployments: false, deployments: [{ id: 2, name: "qwen", served_name: "qwen" }] },
    ], total: 2 } : { error: "key metadata unavailable" }, { status: keysStatus });
    if (request.pathname === "/api/v1/analytics/usage") {
      const data = fx.usageToday();
      return Response.json(request.searchParams.has("key") ? { ...data, rows: [], totals: Object.fromEntries(Object.keys(data.totals).map((key) => [key, 0])), series: data.series.map(() => Object.fromEntries(Object.keys(data.totals).map((key) => [key, 0]))) } : data);
    }
    throw new Error(`unexpected request ${request.pathname}`);
  }) as typeof fetch;
  render(<I18nextProvider i18n={i18n}><MemoryRouter initialEntries={[url]}><Routes><Route path="/analytics/:tab?" element={<AnalyticsPage now={() => fx.NOW} />} /></Routes></MemoryRouter></I18nextProvider>);
  return userEvent.setup({ document: dom.window.document });
}

afterEach(() => { cleanup(); Object.assign(api, original); globalThis.fetch = originalFetch; calls.length = 0; keysStatus = 200; });
after(() => closeChannels());

test("unused all-model and scoped keys remain selectable beside historical usage and request genuine zero data", async () => {
  const user = mount();
  const selector = await screen.findByRole("combobox", { name: locale.analytics.usage.filters.key });
  fireEvent.mouseDown(selector);
  await user.click(await screen.findByText("unused-all-models", { selector: ".ant-select-item-option-content" }));
  await waitFor(() => assert.ok(calls.some((url) => url.pathname.endsWith("/analytics/usage") && url.searchParams.get("key") === "81")));
  await screen.findByText(locale.analytics.state.empty);
  assert.equal(calls.filter((url) => url.pathname.endsWith("/apikeys")).length, 1, "the role-aware metadata list is fetched once, not per key or filter");
  fireEvent.mouseDown(selector);
  assert.ok(await screen.findByText("unused-scoped", { selector: ".ant-select-item-option-content" }));
  assert.ok(await screen.findByText(i18n.t("analytics:names.deletedKey", { id: 7 }), { selector: ".ant-select-item-option-content" }));
  await user.click(screen.getByText("unused-scoped", { selector: ".ant-select-item-option-content" }));
  await waitFor(() => assert.ok(calls.some((url) => url.searchParams.get("key") === "82")));
  assert.ok(screen.getByText(locale.analytics.state.empty));
});

test("a prefix link resolves an unused authorized key without waiting for a usage row", async () => {
  mount("/analytics/usage?keyPrefix=gw-unusedall");
  await waitFor(() => assert.ok(calls.some((url) => url.searchParams.get("key") === "81")));
  await screen.findByText(locale.analytics.state.empty);
  assert.equal(screen.queryByText(i18n.t("analytics:usage.keyPrefixMissing", { prefix: "gw-unusedall" })), null);
});

test("metadata failure leaves historical usage usable and the page refresh retries the safe key list", async () => {
  keysStatus = 500;
  const user = mount();
  await screen.findByText(locale.analytics.state.loadFailed);
  await waitFor(() => assert.ok(document.querySelector('[data-stat="requests"]')));
  keysStatus = 200;
  await user.click(screen.getByRole("button", { name: locale.analytics.toolbar.refresh }));
  await waitFor(() => assert.equal(calls.filter((url) => url.pathname.endsWith("/apikeys")).length, 2));
  const selector = screen.getByRole("combobox", { name: locale.analytics.usage.filters.key });
  fireEvent.mouseDown(selector);
  await screen.findByText("unused-all-models", { selector: ".ant-select-item-option-content" });
  assert.equal(screen.queryByText(locale.analytics.state.loadFailed), null);
});
