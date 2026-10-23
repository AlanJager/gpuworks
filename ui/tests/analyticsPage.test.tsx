import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";
import * as fx from "./helpers/analyticsFixtures.ts";

const closeChannels = trackMessageChannels();
const { i18n, locale } = await createI18n(["analytics"]);
fx.installCanvas(dom);
const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { MemoryRouter, Route, Routes, useLocation } = await import("react-router-dom");
const { api } = await import("../src/api.ts");
const { analyticsApi } = await import("../src/analyticsApi.ts");
const { APIError } = await import("../src/apiError.ts");
const { default: AnalyticsPage } = await import("../src/pages/analytics/AnalyticsPage.tsx");
type Params = import("../src/analyticsApi.ts").AnalyticsParams;

const a = locale.analytics;
const originalAnalytics = { ...analyticsApi };
const originalApi = { dataRetention: api.dataRetention, deployments: api.deployments };

const calls = { usage: [] as Params[], quality: [] as Params[], gpus: [] as Params[], gpuTime: [] as Params[] };
let retention = fx.retentionView();

function stub() {
  for (const list of Object.values(calls)) list.length = 0;
  retention = fx.retentionView();
  api.dataRetention = (async () => retention) as typeof api.dataRetention;
  api.deployments = (async () => fx.deployments) as unknown as typeof api.deployments;
  analyticsApi.usage = async (p) => { calls.usage.push(p); return fx.usageToday(); };
  analyticsApi.quality = async (p) => { calls.quality.push(p); return fx.qualityToday(); };
  analyticsApi.gpus = async (p) => { calls.gpus.push(p); return fx.gpusToday(); };
  analyticsApi.gpuTime = async (p) => { calls.gpuTime.push(p); return fx.gpuTimeToday(); };
}

function Probe() {
  const location = useLocation();
  return <div data-testid="loc">{location.pathname}{location.search}</div>;
}

function mount(url = "/analytics/usage") {
  render(
    <I18nextProvider i18n={i18n}>
      <MemoryRouter initialEntries={[url]}>
        <Routes>
          <Route path="/analytics/:tab?" element={<AnalyticsPage now={() => fx.NOW} />} />
          <Route path="*" element={<div>elsewhere</div>} />
        </Routes>
        <Probe />
      </MemoryRouter>
    </I18nextProvider>,
  );
  return userEvent.setup();
}

const where = () => screen.getByTestId("loc").textContent;
const stat = (key: string) => within(document.querySelector(`[data-stat="${key}"]`) as HTMLElement);
const choose = (user: ReturnType<typeof userEvent.setup>, name: string) => user.click(screen.getByRole("radio", { name }).closest("label")!);
const last = <T,>(list: T[]) => list[list.length - 1];
const compact = (n: number) => new Intl.NumberFormat("en", { notation: "compact", maximumFractionDigits: 2 }).format(n);

afterEach(() => {
  cleanup();
  Object.assign(analyticsApi, originalAnalytics);
  Object.assign(api, originalApi);
});
after(() => closeChannels());

test("a skeleton shows until the first answer, then the usage tab's cards", async () => {
  stub();
  let release!: () => void;
  const gate = new Promise<void>((resolve) => { release = resolve; });
  analyticsApi.usage = async (p) => { calls.usage.push(p); await gate; return fx.usageToday(); };
  mount();
  await waitFor(() => assert.ok(document.querySelector(".ant-skeleton")));
  assert.ok(document.querySelector("[data-stat]") === null);
  release();
  await waitFor(() => assert.ok(document.querySelector('[data-stat="requests"]')));
  assert.ok(document.querySelector(".ant-skeleton") === null);

  assert.ok(stat("requests").getByText("1,317"));
  assert.ok(stat("requests").getByText(i18n.t("analytics:usage.cards.withUsage", { count: 1287 })));
  // The exact figure is the title of the abbreviated one.
  assert.ok(stat("input").getByTitle("10,865,000"));
  assert.ok(stat("cached").getByTitle("7,315,000"));
  // Hit rate: cached tokens over the prompt of the requests that reported a cache figure only.
  assert.ok(stat("cached").getByText(i18n.t("analytics:usage.cards.hitRate", { rate: "76.5%" })));
  assert.ok(stat("uncached").getByTitle("3,550,000"));
  // Some requests reported no cache figure: the card says how much of the "uncached" input they are.
  assert.ok(stat("uncached").getByText(i18n.t("analytics:usage.cards.uncachedUnreported", { tokens: compact(1_305_000) })));
  assert.ok(stat("output").getByTitle("375,500"));
  assert.ok(stat("output").getByText(i18n.t("analytics:usage.cards.thinking", { tokens: compact(122_000), count: 998 })));
  assert.ok(stat("noUsage").getByText("30"));
  assert.ok(screen.getByRole("tab", { name: a.tabs.usage, selected: true }));
});

test("the cache hit rate is a dash when no request reported a cache figure", async () => {
  stub();
  analyticsApi.usage = async () => {
    const d = fx.usageToday();
    d.totals = fx.counts({ requests: 5, prompt_tokens: 500, completion_tokens: 50 });
    d.rows = d.rows.slice(0, 1).map((r) => ({ ...r, ...fx.counts({ requests: 5, prompt_tokens: 500, completion_tokens: 50 }) }));
    return d;
  };
  mount();
  await waitFor(() => assert.ok(document.querySelector('[data-stat="cached"]')));
  assert.ok(stat("cached").getByText(a.usage.cards.hitUnknown));
  assert.ok(stat("output").getByText(a.usage.cards.thinkingUnknown));
  // Nothing was unreported-versus-reported: all 500 prompt tokens are the unreported ones.
  assert.ok(stat("uncached").getByText(i18n.t("analytics:usage.cards.uncachedUnreported", { tokens: "500" })));
});

test("the first request asks for today in the statistics zone, by hour", async () => {
  stub();
  mount();
  await waitFor(() => assert.equal(calls.usage.length, 1));
  assert.deepEqual(calls.usage[0], { from: fx.TODAY_START, to: fx.NOW, granularity: "hour", deployment: undefined, key: undefined, source: undefined });
  assert.ok(screen.getByRole("button", { name: a.toolbar.ranges.today, pressed: true }));
});

test("each quick range sends its bounds in the statistics zone and a granularity that fits", async () => {
  stub();
  const user = mount();
  await waitFor(() => assert.equal(calls.usage.length, 1));
  const D = (y: number, m: number, d: number) => Date.UTC(y, m - 1, d, -8); // local midnight in Asia/Shanghai
  const cases: [string, string, number, number, string][] = [
    [a.toolbar.ranges.yesterday, "yesterday", D(2026, 10, 21), D(2026, 10, 22), "hour"],
    [a.toolbar.ranges["7d"], "7d", D(2026, 10, 16), fx.NOW, "day"],
    [a.toolbar.ranges["30d"], "30d", D(2026, 9, 23), fx.NOW, "day"],
    [a.toolbar.ranges.month, "month", D(2026, 10, 1), fx.NOW, "day"],
    [a.toolbar.ranges.lastmonth, "lastmonth", D(2026, 9, 1), D(2026, 10, 1), "day"],
    [a.toolbar.ranges.today, "today", D(2026, 10, 22), fx.NOW, "hour"],
  ];
  for (const [label, key, from, to, granularity] of cases) {
    const before = calls.usage.length;
    await user.click(screen.getByRole("button", { name: label }));
    await waitFor(() => assert.ok(calls.usage.length > before, `${key} asked`));
    const p = last(calls.usage);
    assert.deepEqual([p.from, p.to, p.granularity], [from, to, granularity], key);
    assert.ok(screen.getByRole("button", { name: label, pressed: true }));
    assert.match(where()!, new RegExp(`range=${key}`));
  }
});

test("by hour is disabled, with the reason, when the range outlasts the hourly data", async () => {
  stub();
  const user = mount();
  await waitFor(() => assert.equal(calls.usage.length, 1));
  const hour = () => screen.getByRole("radio", { name: a.toolbar.hour }) as HTMLInputElement;
  assert.equal(hour().disabled, false);
  assert.ok(screen.queryByText(i18n.t("analytics:toolbar.hourDisabled", { days: 7 })) === null);
  // 30 days is more than the 7 days the server answers by hour.
  await user.click(screen.getByRole("button", { name: a.toolbar.ranges["30d"] }));
  await waitFor(() => assert.equal(hour().disabled, true));
  assert.ok(screen.getByText(i18n.t("analytics:toolbar.hourDisabled", { days: 7 })));
  assert.equal(last(calls.usage).granularity, "day");
});

test("an hourly range the URL asks for falls back to days when it is too long", async () => {
  stub();
  mount("/analytics/usage?range=30d&gran=hour");
  await waitFor(() => assert.equal(calls.usage.length, 1));
  assert.equal(calls.usage[0].granularity, "day");
});

test("the hour retention caps the hourly range below a week", async () => {
  stub();
  retention = fx.retentionView({ usage_hourly_days: 3 });
  mount("/analytics/usage?range=7d");
  await waitFor(() => assert.equal(calls.usage.length, 1));
  assert.equal(calls.usage[0].granularity, "day");
  assert.equal((screen.getByRole("radio", { name: a.toolbar.hour }) as HTMLInputElement).disabled, true);
  assert.ok(screen.getByText(i18n.t("analytics:toolbar.hourDisabled", { days: 3 })));
});

test("picking by hour for a week asks for hours and lands in the URL", async () => {
  stub();
  const user = mount("/analytics/usage?range=7d");
  await waitFor(() => assert.equal(calls.usage.length, 1));
  assert.equal(calls.usage[0].granularity, "day");
  await choose(user, a.toolbar.hour);
  await waitFor(() => assert.equal(last(calls.usage).granularity, "hour"));
  assert.match(where()!, /gran=hour/);
});

test("a custom range in the URL is asked for as it is, and no quick range is pressed", async () => {
  stub();
  const from = Date.UTC(2026, 9, 20, 2, 0);
  const to = Date.UTC(2026, 9, 20, 14, 30);
  mount(`/analytics/usage?range=custom&from=${from}&to=${to}`);
  await waitFor(() => assert.equal(calls.usage.length, 1));
  assert.deepEqual([calls.usage[0].from, calls.usage[0].to, calls.usage[0].granularity], [from, to, "hour"]);
  for (const key of ["today", "yesterday", "7d", "30d", "month", "lastmonth"] as const) {
    assert.ok(screen.getByRole("button", { name: a.toolbar.ranges[key], pressed: false }));
  }
  // The picker shows the range on the statistics zone's clock, whatever the browser's.
  await waitFor(() => assert.deepEqual([...document.querySelectorAll<HTMLInputElement>(".ant-picker-input input")].map((i) => i.value), ["2026-10-20 10:00", "2026-10-20 22:30"]));
});

test("switching tabs changes the URL, keeps the shared filters and drops the tab's own", async () => {
  stub();
  const user = mount("/analytics/usage?range=7d&deployment=1&key=11&source=api");
  await waitFor(() => assert.equal(calls.usage.length, 1));
  assert.equal(calls.usage[0].key, "11");
  assert.equal(calls.usage[0].source, "api");
  await user.click(screen.getByRole("tab", { name: a.tabs.quality }));
  await waitFor(() => assert.equal(where(), "/analytics/quality?range=7d&deployment=1"));
  await waitFor(() => assert.equal(calls.quality.length, 1));
  assert.equal(calls.quality[0].deployment, 1);
  assert.equal(calls.quality[0].granularity, "day");
  await user.click(screen.getByRole("tab", { name: a.tabs.gpus }));
  await waitFor(() => assert.equal(where(), "/analytics/gpus?range=7d&deployment=1"));
  await user.click(screen.getByRole("tab", { name: a.tabs["gpu-time"] }));
  await waitFor(() => assert.equal(where(), "/analytics/gpu-time?range=7d&deployment=1"));
  await waitFor(() => assert.equal(calls.gpuTime.length, 1));
  assert.ok(screen.getByRole("tab", { name: a.tabs["gpu-time"], selected: true }));
});

test("the GPU tab leaves the deployment filter off: the API has none for it", async () => {
  stub();
  mount("/analytics/gpus?deployment=1");
  await waitFor(() => assert.equal(calls.gpus.length, 1));
  assert.equal(calls.gpus[0].granularity, "hour");
  assert.equal((screen.getByRole("combobox", { name: a.toolbar.deployment }) as HTMLInputElement).disabled, true);
});

test("a tab that does not exist, or none at all, goes to the usage tab", async () => {
  stub();
  mount("/analytics/nope?range=7d");
  await waitFor(() => assert.equal(where(), "/analytics/usage?range=7d"));
  cleanup();
  stub();
  mount("/analytics");
  await waitFor(() => assert.equal(where(), "/analytics/usage"));
});

test("a deployment filter is sent and kept in the URL", async () => {
  stub();
  const user = mount();
  await waitFor(() => assert.equal(calls.usage.length, 1));
  await waitFor(() => assert.ok(document.querySelector(".ant-select")));
  const select = screen.getByRole("combobox", { name: a.toolbar.deployment });
  fireEvent.mouseDown(select);
  let option: Element | null = null;
  await waitFor(() => assert.ok((option = document.querySelector('.ant-select-item-option[title="qwen3-32b"]'))));
  await user.click(option!);
  await waitFor(() => assert.equal(last(calls.usage).deployment, 3));
  assert.match(where()!, /deployment=3/);
});

test("the key filter lists the keys of the unfiltered answer and keeps them after filtering", async () => {
  stub();
  analyticsApi.usage = async (p) => {
    calls.usage.push(p);
    const d = fx.usageToday();
    if (p.key) d.rows = d.rows.filter((r) => String(r.api_key_id) === p.key);
    return d;
  };
  const user = mount();
  await waitFor(() => assert.equal(calls.usage.length, 1));
  await screen.findByRole("combobox", { name: a.usage.filters.key });
  const open = async () => {
    fireEvent.mouseDown(screen.getByRole("combobox", { name: a.usage.filters.key }));
    await waitFor(() => assert.ok(document.querySelector(".ant-select-dropdown:not(.ant-select-dropdown-hidden) .ant-select-item-option")));
  };
  await open();
  const titles = () => [...document.querySelectorAll(".ant-select-dropdown:not(.ant-select-dropdown-hidden) .ant-select-item-option")].map((o) => o.getAttribute("title"));
  await waitFor(() => assert.ok(titles().includes("MarketToGetherGroup")));
  assert.ok(titles().includes(a.usage.filters.allKeys));
  assert.ok(titles().includes(`trial-2026q3 (${a.names.revoked})`), "a revoked key is offered, marked");
  assert.ok(titles().includes(a.usage.filters.noKey), "requests without a key can be filtered too");
  await user.click(document.querySelector('.ant-select-item-option[title="MarketToGetherGroup"]')!);
  await waitFor(() => assert.equal(last(calls.usage).key, "12"));
  assert.match(where()!, /key=12/);
  await waitFor(() => assert.ok(!document.querySelector("tbody")!.textContent!.includes("gw-5fd30737e"), "only the chosen key's rows remain"));
  // The filtered answer holds one key, but the list still has the others.
  await open();
  await waitFor(() => assert.ok(titles().includes("111")));
});

test("the key filter can ask for the requests without a key", async () => {
  stub();
  const user = mount("/analytics/usage");
  await waitFor(() => assert.equal(calls.usage.length, 1));
  await screen.findByRole("combobox", { name: a.usage.filters.key });
  fireEvent.mouseDown(screen.getByRole("combobox", { name: a.usage.filters.key }));
  await waitFor(() => assert.ok(document.querySelector(`.ant-select-item-option[title="${a.usage.filters.noKey}"]`)));
  await user.click(document.querySelector(`.ant-select-item-option[title="${a.usage.filters.noKey}"]`)!);
  await waitFor(() => assert.equal(last(calls.usage).key, "none"));
});

test("a token link from the API tokens page is turned into the key's id once the rows are in", async () => {
  stub();
  mount("/analytics/usage?keyPrefix=gw-6455c72ca&range=7d");
  await waitFor(() => assert.equal(calls.usage.length >= 1, true));
  assert.equal(calls.usage[0].key, undefined, "the first answer is the unfiltered one the id comes from");
  await waitFor(() => assert.equal(where(), "/analytics/usage?range=7d&key=12"));
  await waitFor(() => assert.equal(last(calls.usage).key, "12"));
});

test("a token link whose token has no usage in the range says so instead of filtering silently", async () => {
  stub();
  mount("/analytics/usage?keyPrefix=gw-nobody000");
  const text = i18n.t("analytics:usage.keyPrefixMissing", { prefix: "gw-nobody000" });
  await screen.findByText(text);
  assert.equal(calls.usage.length, 1);
});

test("a range with no statistics shows the empty state with the hint that statistics accumulate", async () => {
  stub();
  analyticsApi.usage = async () => ({ range: fx.range(fx.TODAY_START, 3, "hour"), totals: fx.counts(), series: [], rows: [] });
  mount();
  await screen.findByText(a.state.empty);
  assert.ok(screen.getByText(a.state.emptyHint));
  assert.ok(document.querySelector("[data-stat]") === null);
});

test("a failure shows the product error's text with a retry that asks again", async () => {
  stub();
  let attempts = 0;
  analyticsApi.usage = async (p) => {
    calls.usage.push(p);
    attempts += 1;
    if (attempts === 1) throw new APIError("ANALYTICS_RANGE_TOO_LONG", { field: "granularity", detail: "an hourly range spans at most 7 days" }, "range too long", 400);
    return fx.usageToday();
  };
  const user = mount();
  const alert = await screen.findByRole("alert");
  assert.ok(within(alert).getByText(a.state.loadFailed));
  assert.ok(within(alert).getByText(i18n.t("common:apiError.ANALYTICS_RANGE_TOO_LONG", { detail: "an hourly range spans at most 7 days" })));
  await user.click(within(alert).getByRole("button", { name: "Retry" }));
  await waitFor(() => assert.ok(document.querySelector('[data-stat="requests"]')));
  assert.equal(calls.usage.length, 2);
  assert.ok(screen.queryByRole("alert") === null);
});

test("an unexpected failure shows the plain message", async () => {
  stub();
  analyticsApi.quality = async () => { throw new Error("network down"); };
  mount("/analytics/quality");
  const alert = await screen.findByRole("alert");
  assert.ok(within(alert).getByText("network down"));
});

test("the backfill note and the zone appear only when the range starts before live accumulation did", async () => {
  stub();
  mount();
  await waitFor(() => assert.ok(document.querySelector('[data-stat="requests"]')));
  assert.ok(screen.getByText(a.backfill.usage));
  assert.ok(screen.getByText(i18n.t("analytics:zoneNote", { zone: fx.ZONE })));
  cleanup();
  stub();
  analyticsApi.usage = async () => ({ ...fx.usageToday(), range: { ...fx.usageToday().range, live_since_ms: fx.TODAY_START } });
  mount();
  await waitFor(() => assert.ok(document.querySelector('[data-stat="requests"]')));
  assert.ok(screen.queryByText(a.backfill.usage) === null);
});

test("the subtitle names the retention of the tab and links to the setting", async () => {
  stub();
  retention = fx.retentionView({ usage_hourly_days: 14, usage_daily_days: 500, stats_hourly_days: 9, stats_daily_days: 600 });
  const user = mount();
  const link = await screen.findByRole("link", { name: /Data retention/ });
  assert.equal(link.getAttribute("href"), "/settings#data-retention");
  await waitFor(() => assert.match(link.closest(".ant-typography")!.textContent!, /14 days by hour and 500 days by day/));
  await user.click(screen.getByRole("tab", { name: a.tabs.quality }));
  await waitFor(() => assert.match(screen.getByRole("link", { name: /Data retention/ }).closest(".ant-typography")!.textContent!, /9 days by hour and 600 days by day/));
});

test("without the retention setting the page still works, in a week's hours and the browser's zone", async () => {
  stub();
  api.dataRetention = (async () => { throw new Error("denied"); }) as typeof api.dataRetention;
  mount("/analytics/quality?range=7d");
  await waitFor(() => assert.equal(calls.quality.length, 1));
  await waitFor(() => assert.ok(document.querySelector('[data-stat="requests"]')));
});

test("refresh asks again for the same range", async () => {
  stub();
  const user = mount();
  await waitFor(() => assert.equal(calls.usage.length, 1));
  await user.click(screen.getByRole("button", { name: a.toolbar.refresh }));
  await waitFor(() => assert.equal(calls.usage.length, 2));
});

test("export is off until a table is on screen, then downloads the rows with a localized header", async () => {
  stub();
  const created: Blob[] = [];
  const downloads: string[] = [];
  const anchor = dom.window.HTMLAnchorElement.prototype;
  const savedClick = anchor.click;
  anchor.click = function click(this: HTMLAnchorElement) { downloads.push(this.download); };
  const url = globalThis.URL as unknown as { createObjectURL?: unknown; revokeObjectURL?: unknown };
  const saved = { create: url.createObjectURL, revoke: url.revokeObjectURL };
  url.createObjectURL = (b: Blob) => { created.push(b); return "blob:test"; };
  url.revokeObjectURL = () => {};
  try {
    let release!: () => void;
    const gate = new Promise<void>((resolve) => { release = resolve; });
    analyticsApi.usage = async (p) => { calls.usage.push(p); await gate; return fx.usageToday(); };
    const user = mount();
    const button = screen.getByRole("button", { name: new RegExp(a.toolbar.export) }) as HTMLButtonElement;
    assert.equal(button.disabled, true);
    release();
    await waitFor(() => assert.equal(button.disabled, false));
    await user.click(button);
    assert.deepEqual(downloads, ["ops-usage-20261022_20261022.csv"]);
    assert.equal(created.length, 1);
    const text = await created[0].text();
    const lines = text.replace(/^﻿/, "").split("\r\n");
    assert.equal(lines[0], [a.usage.export.key, a.usage.export.prefix, a.usage.export.status, a.usage.export.deployment, a.usage.export.model, a.usage.export.source,
      a.usage.table.requests, a.usage.table.input, a.usage.table.cached, a.usage.table.hitRate, a.usage.table.uncached, a.usage.table.output, a.usage.table.reasoning, a.usage.table.noUsage,
      a.usage.export.first, a.usage.export.last].map((c) => `"${c}"`).join(","));
    assert.equal(lines.length, 1 + 7 + 1, "header, one line per row, a total");
    assert.match(lines[1], /^"111","gw-5fd30737e","","glm-5-3-flash-nvfp4","glm-5.3-flash","API","1000","9000000","7200000","80.0%","1800000","300000","120000","10","2026-10-22 01:00","2026-10-22 13:00"$/);
    assert.match(lines.at(-1)!, new RegExp(`^"${a.table.total}"`));
    assert.match(lines.at(-1)!, /"1317","10865000","7315000","76\.5%","3550000","375500","122000","30"/);
    assert.match(lines.find((l) => l.includes("trial-2026q3"))!, new RegExp(`"${a.names.revoked}"`));
  } finally {
    anchor.click = savedClick;
    url.createObjectURL = saved.create;
    url.revokeObjectURL = saved.revoke;
  }
});
