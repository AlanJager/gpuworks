import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";
import * as fx from "./helpers/analyticsFixtures.ts";
import type { GPUAnalytics, UsageAnalytics } from "../src/analyticsApi.ts";

const closeChannels = trackMessageChannels();
const { i18n, locale } = await createI18n(["analytics", "nodes"]);
fx.installCanvas(dom);
const { cleanup, render, screen, waitFor } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { MemoryRouter, Route, Routes } = await import("react-router-dom");
const { default: AnalyticsPage } = await import("../src/pages/analytics/AnalyticsPage.tsx");

const a = locale.analytics;
const originalFetch = globalThis.fetch;
const originalCreateURL = URL.createObjectURL;
const originalRevokeURL = URL.revokeObjectURL;
const originalClick = dom.window.HTMLAnchorElement.prototype.click;
const downloads: Blob[] = [];
const unexpected: string[] = [];
const usageRequests: string[] = [];

function mount(path: string, data: { gpus?: GPUAnalytics; usage?: UsageAnalytics; usageQuery?: Record<string, string> } = {}) {
  downloads.length = 0;
  unexpected.length = 0;
  usageRequests.length = 0;
  globalThis.fetch = (async (input, init) => {
    const url = new URL(String(input), "http://localhost");
    const route = `${init?.method ?? "GET"} ${url.pathname}`;
    if (url.pathname === "/api/v1/analytics/usage") usageRequests.push(url.search);
    if (url.pathname === "/api/v1/analytics/usage" && data.usageQuery) {
      for (const [key, value] of Object.entries(data.usageQuery)) {
        if (url.searchParams.get(key) !== value) {
          unexpected.push(`Expected ${key}=${value}, received ${url.search}`);
          return Response.json({ error: "Unexpected usage filter" }, { status: 400 });
        }
      }
    }
    const responses: Record<string, unknown> = {
      "GET /api/v1/data-retention": fx.retentionView(),
      "GET /api/v1/deployments": fx.deployments,
      "GET /api/v1/nodes/facets": fx.nodeFacets,
      "GET /api/v1/analytics/gpus": data.gpus ?? fx.gpusToday(),
      "GET /api/v1/analytics/usage": data.usage ?? fx.usageToday(),
    };
    if (!(route in responses)) {
      unexpected.push(route);
      return Response.json({ error: `Unexpected request: ${route}` }, { status: 599 });
    }
    return Response.json(responses[route]);
  }) as typeof fetch;
  URL.createObjectURL = (blob) => { downloads.push(blob as Blob); return "blob:analytics-export"; };
  URL.revokeObjectURL = () => {};
  dom.window.HTMLAnchorElement.prototype.click = function () {};
  render(
    <I18nextProvider i18n={i18n}>
      <MemoryRouter initialEntries={[path]}>
        <Routes><Route path="/analytics/:tab" element={<AnalyticsPage now={() => fx.NOW} />} /></Routes>
      </MemoryRouter>
    </I18nextProvider>,
  );
  return userEvent.setup();
}

async function exportRows(user: ReturnType<typeof userEvent.setup>) {
  const button = screen.getByRole("button", { name: new RegExp(a.toolbar.export) }) as HTMLButtonElement;
  await waitFor(() => assert.equal(button.disabled, false));
  await user.click(button);
  assert.deepEqual(unexpected, []);
  const blob = downloads.at(-1)!;
  assert.deepEqual([...new Uint8Array(await blob.arrayBuffer()).slice(0, 3)], [0xef, 0xbb, 0xbf]);
  const text = await blob.text();
  // Export quotes every cell; the parser retains embedded commas and quotes.
  return text.replace(/^\uFEFF/, "").split("\r\n").map((line) =>
    [...line.matchAll(/"((?:[^"]|"")*)"(?:,|$)/g)].map((match) => match[1].replaceAll('""', '"')),
  );
}

afterEach(() => {
  cleanup();
  globalThis.fetch = originalFetch;
  URL.createObjectURL = originalCreateURL;
  URL.revokeObjectURL = originalRevokeURL;
  dom.window.HTMLAnchorElement.prototype.click = originalClick;
});
after(() => closeChannels());

test("GPU CSV ends with the filtered range totals instead of averaging grouped means", async () => {
  const user = mount("/analytics/gpus");
  const rows = await exportRows(user);
  const total = rows.at(-1)!;
  assert.equal(total[0], a.table.total);
  assert.equal(total[2], "22.4");
  assert.equal(total[3], "97.0");
  assert.equal(total[4], "70.0");
  assert.equal(total[8], "160");
  assert.equal(total[9], "70");
  assert.equal(total.length, rows[0].length);
});

test("Token CSV follows the selected key, deployment and model groups without repeating totals", async () => {
  const user = mount("/analytics/usage");
  let rows = await exportRows(user);
  const count = (name: string, column: string) => rows.find((row) => row[0] === name)![rows[0].indexOf(column)];
  assert.equal(rows.filter((row) => row[0] === "111").length, 1, "one token used on two deployments exports one group");
  assert.equal(count("111", a.usage.table.requests), "1100");
  assert.equal(count("111", a.usage.table.input), "9800000");
  assert.equal(count("111", a.usage.table.hitRate), "80.0%");
  assert.equal(rows.length, 7, "header, five token groups, one total");

  await user.click(screen.getByRole("radio", { name: a.usage.group.deployment }).closest("label")!);
  rows = await exportRows(user);
  assert.equal(rows[0][0], a.usage.table.group.deployment);
  assert.equal(rows.length, 5, "header, three deployment groups, one total");
  assert.equal(count("glm-5-3-flash-nvfp4", a.usage.table.requests), "1215");
  assert.equal(count("glm-5-3-flash-nvfp4", a.usage.table.input), "10063000");

  await user.click(screen.getByRole("radio", { name: a.usage.group.model }).closest("label")!);
  rows = await exportRows(user);
  assert.equal(rows[0][0], a.usage.table.group.model);
  assert.equal(count("glm-5.3-flash", a.usage.table.requests), "1215");
  assert.equal(rows.length, 5);
  assert.equal(count(a.table.total, a.usage.table.requests), "1317");
  assert.equal(rows.filter((row) => row[0] === a.table.total).length, 1);
  assert.equal(usageRequests.length, 1, "grouping reuses the fetched range without new HTTP data");
});

test("GPU CSV also uses overall totals when the server returns groups without individual cards", async () => {
  const user = mount("/analytics/gpus", { gpus: fx.gpusFleet() });
  const rows = await exportRows(user);
  const total = rows.at(-1)!;
  assert.equal(total[0], a.table.total);
  assert.equal(total[2], "57.9", "range total is not the unweighted mean of the H100 and L40S groups");
  assert.equal(total[8], "298");
  assert.equal(total[5], "", "no cross-card memory percentage is inferred");
  assert.equal(total[7], "");
  assert.equal(total[11], "", "unknown per-card idle counts do not become a false total flag");
});

test("GPU CSV leaves unsampled totals unknown instead of inventing zero measurements", async () => {
  const gpus = fx.gpusToday();
  for (const key of Object.keys(gpus.totals) as (keyof typeof gpus.totals)[]) {
    gpus.totals[key] = key.endsWith("_samples") ? 0 : null;
  }
  gpus.cards = gpus.cards.map((card) => ({ ...card, ...gpus.totals }));
  const user = mount("/analytics/gpus", { gpus });
  const total = (await exportRows(user)).at(-1)!;
  assert.equal(total[0], a.table.total);
  assert.deepEqual(total.slice(2, 4), ["—", "—"]);
  assert.deepEqual(total.slice(4), ["", "", "", "", "", "", "", ""]);
});

test("Token CSV exports the requested source, key and deployment filter without unfiltered rows", async () => {
  const usage = fx.usageToday();
  usage.rows = [usage.rows[4]];
  usage.totals = fx.counts({
    requests: 8, prompt_tokens: 20_000, cached_tokens: 5_000, cache_reported: 8,
    cache_reported_prompt: 20_000, completion_tokens: 3_000, reasoning_tokens: 2_000, reasoning_reported: 8,
  });
  usage.series = usage.range.buckets.map((_, index) => index === 1 ? usage.totals : fx.counts());
  const user = mount("/analytics/usage?source=playground&key=none&deployment=1", {
    usage,
    usageQuery: { source: "playground", key: "none", deployment: "1", from: String(fx.TODAY_START), to: String(fx.NOW), granularity: "hour" },
  });
  const rows = await exportRows(user);
  assert.equal(rows.length, 3, "header, filtered keyless user group, total");
  assert.equal(rows[1][0], i18n.t("analytics:names.playground", { user: "admin" }));
  assert.equal(rows[1][rows[0].indexOf(a.usage.table.requests)], "8");
  assert.equal(rows[1][rows[0].indexOf(a.usage.table.hitRate)], "25.0%");
  assert.equal(rows[2][rows[0].indexOf(a.usage.table.input)], "20000");
  assert.equal(rows[2][rows[0].indexOf(a.usage.table.output)], "3000");
});
