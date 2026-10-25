import assert from "node:assert/strict";
import { test } from "node:test";
import "./helpers/dom.ts";
import * as fx from "./helpers/analyticsFixtures.ts";
import { analyticsQueryString } from "../src/analyticsApi.ts";
import { buildCsv } from "../src/pages/analytics/csv.ts";
import { heatData, heatHeight, heatScale, lineOption, shadeArea, stackedBarOption } from "../src/pages/analytics/chartOptions.ts";
import { DASH, fmtCompact, fmtPct, fmtSeconds, ratio } from "../src/pages/analytics/format.ts";
import { groupCards, groupRows, isIdle, mean, memPercent, summarize, summarizeGroups } from "../src/pages/analytics/gpuModel.ts";
import { buildUsageTree, hasKeylessRows, hitRate, keyIdentity, keyOptions, uncachedTokens, uncachedUnreported } from "../src/pages/analytics/usageModel.ts";
import {
  addDays, bucketLabel, compactDate, fullTime, isQuickKey, quickRange, shortTime, spansDays, startOfDay, usableZone, zonedParts, zonedToMs,
} from "../src/pages/analytics/zoned.ts";

const SH = "Asia/Shanghai";
const LA = "America/Los_Angeles";
const iso = (ms: number) => new Date(ms).toISOString();

// ------------------------------------------------------------------- time zones

test("the zone's clock reads the instant as its own wall time, and back", () => {
  assert.deepEqual(zonedParts(fx.NOW, SH), { y: 2026, m: 10, d: 22, h: 14, mi: 30, s: 0 });
  for (const zone of ["UTC", SH, LA, "Asia/Kolkata", "Pacific/Auckland"]) {
    for (const ms of [fx.NOW, Date.UTC(2026, 2, 8, 9, 59), Date.UTC(2026, 10, 1, 8, 30), Date.UTC(2026, 11, 31, 23, 59)]) {
      const p = zonedParts(ms, zone);
      assert.equal(zonedToMs(p.y, p.m, p.d, p.h, p.mi, zone), Math.floor(ms / 60_000) * 60_000, `${zone} ${iso(ms)}`);
    }
  }
});

test("a day is a day of the zone: 25 hours long when the clocks go back, and the half-hour zones start on the half hour", () => {
  // 2026-11-01 is the day US clocks go back: midnight PDT to midnight PST is 25 hours.
  const oct31 = Date.UTC(2026, 9, 31, 7);
  assert.equal(startOfDay(Date.UTC(2026, 9, 31, 20), LA), oct31);
  assert.equal(addDays(oct31, 1, LA), Date.UTC(2026, 10, 1, 7));
  assert.equal(addDays(oct31, 2, LA), Date.UTC(2026, 10, 2, 8));
  assert.equal(startOfDay(Date.UTC(2026, 9, 22, 6, 30), "Asia/Kolkata"), Date.UTC(2026, 9, 21, 18, 30));
});

test("quick ranges are computed on the statistics zone's calendar", () => {
  const D = (y: number, m: number, d: number) => Date.UTC(y, m - 1, d, -8);
  assert.deepEqual(quickRange("today", fx.NOW, SH), { from: D(2026, 10, 22), to: fx.NOW });
  assert.deepEqual(quickRange("yesterday", fx.NOW, SH), { from: D(2026, 10, 21), to: D(2026, 10, 22) });
  assert.deepEqual(quickRange("7d", fx.NOW, SH), { from: D(2026, 10, 16), to: fx.NOW });
  assert.deepEqual(quickRange("30d", fx.NOW, SH), { from: D(2026, 9, 23), to: fx.NOW });
  assert.deepEqual(quickRange("month", fx.NOW, SH), { from: D(2026, 10, 1), to: fx.NOW });
  assert.deepEqual(quickRange("lastmonth", fx.NOW, SH), { from: D(2026, 9, 1), to: D(2026, 10, 1) });
  // The same instant is still the 21st in Los Angeles: "today" starts a day earlier there.
  assert.equal(quickRange("today", fx.NOW, LA).from, Date.UTC(2026, 9, 21, 7));
  // And at a month's start the month and last month follow the zone, not UTC.
  const first = Date.UTC(2026, 10, 1, 17);
  assert.deepEqual(quickRange("month", first, LA), { from: Date.UTC(2026, 10, 1, 7), to: first });
  assert.deepEqual(quickRange("lastmonth", first, LA), { from: Date.UTC(2026, 9, 1, 7), to: Date.UTC(2026, 10, 1, 7) });
  // A year boundary.
  assert.deepEqual(quickRange("lastmonth", Date.UTC(2027, 0, 15), "UTC"), { from: Date.UTC(2026, 11, 1), to: Date.UTC(2027, 0, 1) });
});

test("labels, file-name dates and the multi-day test follow the zone", () => {
  const midnight = Date.UTC(2026, 9, 21, 16);
  assert.equal(bucketLabel(midnight, SH, "hour", false), "00:00");
  assert.equal(bucketLabel(midnight + 5 * fx.HOUR, SH, "hour", true), "10-22 05:00");
  assert.equal(bucketLabel(midnight, SH, "day", true), "10-22");
  assert.equal(shortTime(fx.NOW, SH), "10-22 14:30");
  assert.equal(fullTime(fx.NOW, SH), "2026-10-22 14:30");
  assert.equal(compactDate(fx.NOW, SH), "20261022");
  assert.equal(compactDate(fx.NOW, LA), "20261021");
  assert.equal(spansDays([midnight, midnight + fx.HOUR], SH), false);
  assert.equal(spansDays([midnight, midnight + 24 * fx.HOUR], SH), true);
  assert.equal(spansDays([midnight], SH), false);
});

test("an unknown zone name falls back to the browser's zone", () => {
  assert.equal(usableZone("Mars/Olympus"), Intl.DateTimeFormat().resolvedOptions().timeZone);
  assert.equal(usableZone(""), Intl.DateTimeFormat().resolvedOptions().timeZone);
  assert.equal(usableZone(SH), SH);
  assert.equal(isQuickKey("7d"), true);
  assert.equal(isQuickKey("custom"), false);
  assert.equal(isQuickKey(null), false);
});

// ---------------------------------------------------------------------- query

test("a query string carries only the parameters that are set", () => {
  assert.equal(analyticsQueryString({ from: 1, to: 2 }), "from=1&to=2");
  assert.equal(
    analyticsQueryString({ from: 1, to: 2, granularity: "day", deployment: 3, key: "none", source: "playground", groupBy: "gpu", metric: "temp", labels: ["pool=a b", "gpuworks.ai/node=n1"], model: "RTX 6000D" }),
    "from=1&to=2&granularity=day&deployment=3&key=none&source=playground&group_by=gpu&metric=temp&model=RTX%206000D&label=pool%3Da%20b&label=gpuworks.ai%2Fnode%3Dn1",
  );
  assert.equal(analyticsQueryString({ from: 1, to: 2, deployment: undefined, key: "", labels: [] }), "from=1&to=2");
});

// ---------------------------------------------------------------------- format

test("figures: a missing one is a dash, a ratio needs a denominator, a compact one follows the language", () => {
  assert.equal(fmtPct(null), DASH);
  assert.equal(fmtPct(12.345), "12.3%");
  assert.equal(fmtSeconds(null), DASH);
  assert.equal(fmtSeconds(1820), "1.82 s");
  assert.equal(ratio(1, 0), null);
  assert.equal(ratio(1, 4), 25);
  assert.equal(fmtCompact(12_345_678, "en"), "12.35M");
  assert.match(fmtCompact(12_345_678, "zh"), /万|亿/);
  assert.equal(fmtCompact(null, "en"), DASH);
});

// ----------------------------------------------------------------------- csv

test("CSV: a byte-order mark, CRLF rows, every cell quoted, quotes doubled and null empty", () => {
  const text = buildCsv({ name: "x", head: ["a", 'b "q"'], rows: [["1,5", null], [2, "line\nbreak"]] });
  assert.equal(text, '﻿"a","b ""q"""\r\n"1,5",""\r\n"2","line\nbreak"');
});

// -------------------------------------------------------------------- charts

test("the backfilled buckets are shaded from the first to the last of them, and nothing is shaded without any", () => {
  assert.equal(shadeArea(0, 10), undefined);
  assert.equal(shadeArea(undefined, 10), undefined);
  const area = shadeArea(3, 10, "filled in")!;
  assert.deepEqual(area.data, [[{ xAxis: -0.5 }, { xAxis: 2.5 }]]);
  assert.equal((area.label as { formatter: string }).formatter, "filled in");
  assert.deepEqual(shadeArea(30, 10)!.data, [[{ xAxis: -0.5 }, { xAxis: 9.5 }]], "never past the last bucket");
});

test("a stacked bar chart stacks its series, shades only on the first and escapes names in the tooltip", () => {
  const option = stackedBarOption({
    labels: ["a", "b"], series: [{ name: "<b>x</b>", color: "#111", data: [1, 2] }, { name: "y", color: "#222", data: [null, 3] }],
    valueFormat: (v) => `${v} tok`, totalLabel: "Total", shadeLeading: 1, shadeLabel: "filled",
  });
  const series = option.series as { stack: string; markArea?: unknown; name: string }[];
  assert.deepEqual(series.map((s) => s.stack), ["total", "total"]);
  assert.ok(series[0].markArea);
  assert.equal(series[1].markArea, undefined);
  const tooltip = option.tooltip as { formatter: (p: unknown) => string };
  const html = tooltip.formatter([
    { value: 2, marker: "", seriesName: "<b>x</b>", axisValueLabel: "b" },
    { value: null, marker: "", seriesName: "y", axisValueLabel: "b" },
  ]);
  assert.ok(html.includes("&lt;b&gt;x&lt;/b&gt;"));
  assert.ok(!html.includes("<b>x</b>"));
  assert.ok(html.includes("Total") && html.includes("2 tok"));
  assert.ok(!html.includes(">y<") && !html.includes("y<span"), "a series without a value is not listed");
});

test("many buckets get a zoom bar; few do not", () => {
  const few = stackedBarOption({ labels: ["a"], series: [], valueFormat: String, totalLabel: "t" });
  assert.deepEqual(few.dataZoom, []);
  const many = lineOption({ labels: Array.from({ length: 60 }, (_, i) => String(i)), series: [], valueFormat: String });
  assert.equal((many.dataZoom as unknown[]).length, 2);
});

test("a line chart keeps a gap where there is no figure", () => {
  const option = lineOption({ labels: ["a", "b", "c"], series: [{ name: "p50", color: "#111", data: [0.5, null, 0.7] }], valueFormat: (v) => `${v} s` });
  const s = (option.series as { data: (number | null)[]; connectNulls: boolean }[])[0];
  assert.deepEqual(s.data, [0.5, null, 0.7]);
  assert.equal(s.connectNulls, false);
});

test("heat map colour scales: percent and temperature fixed, watts and bytes fitted to the data", () => {
  const rows = [{ key: "a", label: "a", values: [10, null, 331.7] }, { key: "b", label: "b", values: [null, 120, 5] }];
  assert.deepEqual(heatScale("util", rows), [0, 100]);
  assert.deepEqual(heatScale("temp", [{ key: "a", label: "a", values: [40, null, 119.2] }]), [20, 120], "a temperature past 100 widens the scale");
  assert.deepEqual(heatScale("temp", [{ key: "a", label: "a", values: [40] }]), [20, 100]);
  assert.deepEqual(heatScale("power", rows), [0, 332]);
  assert.deepEqual(heatScale("mem", rows), [0, 331.7]);
  assert.deepEqual(heatScale("power", []), [0, 1], "an empty chart still has a scale");
});

test("heat map cells without samples are left out, so they stay blank", () => {
  assert.deepEqual(heatData([{ key: "a", label: "a", values: [1, null, 3] }, { key: "b", label: "b", values: [null, null, 0] }]), [[0, 0, 1], [2, 0, 3], [2, 1, 0]]);
  assert.equal(heatHeight(1), 220);
  assert.equal(heatHeight(10), 330);
  assert.equal(heatHeight(1000), 620);
});

// ---------------------------------------------------------------------- usage

test("a key is told apart by id; a keyless Playground request by its user", () => {
  const rows = fx.usageToday().rows;
  assert.deepEqual([...new Set(rows.map(keyIdentity))], ["k11", "k12", "k13", "pg1", "k7"]);
  assert.equal(keyIdentity({ ...rows[0], api_key_id: 0, source: "api" }), "nokey");
});

test("grouped by key, each key's rows merge across deployments and source, with the deployments beneath", () => {
  const tree = buildUsageTree(fx.usageToday().rows, "key");
  const k111 = tree.find((n) => n.id === "k:k11")!;
  assert.equal(k111.counts.requests, 1100);
  assert.equal(k111.counts.prompt_tokens, 9_800_000);
  assert.deepEqual(k111.children!.map((c) => c.deployment!.name), ["glm-5-3-flash-nvfp4", "qwen3-32b"]);
  assert.deepEqual(k111.children!.map((c) => c.counts.requests), [1000, 100]);
  const pg = tree.find((n) => n.key?.playground)!;
  assert.equal(pg.key!.userName, "admin");
  const mtg = tree.find((n) => n.key?.prefix === "gw-6455c72ca")!;
  assert.deepEqual(mtg.children!.map((c) => c.deployment!.deleted), [false, true]);
  assert.equal(mtg.counts.requests, 202);
});

test("grouped by deployment or model, the keys that used it are beneath, and a deployment known only by id still groups", () => {
  const rows = fx.usageToday().rows;
  const byDeployment = buildUsageTree(rows, "deployment");
  assert.deepEqual(byDeployment.map((n) => n.deployment!.id).sort(), [1, 3, 9]);
  const dep1 = byDeployment.find((n) => n.deployment!.id === 1)!;
  assert.equal(dep1.keyCount, 5);
  assert.equal(dep1.children!.length, 5);
  const byModel = buildUsageTree(rows, "model");
  assert.deepEqual(byModel.map((n) => n.model).sort(), ["glm-5.3-flash", "old-model", "qwen3-32b"]);
  const flash = byModel.find((n) => n.model === "glm-5.3-flash")!;
  assert.equal(flash.counts.requests, 1000 + 200 + 4 + 8 + 3);
});

test("the grand total of the tree is the total of the rows", () => {
  const d = fx.usageToday();
  for (const by of ["key", "deployment", "model"] as const) {
    assert.equal(buildUsageTree(d.rows, by).reduce((a, n) => a + n.counts.requests, 0), d.totals.requests, by);
  }
});

test("the hit rate divides by the prompt of the requests that reported a cache figure only", () => {
  const c = fx.counts({ prompt_tokens: 1000, cached_tokens: 300, cache_reported_prompt: 600 });
  assert.equal(hitRate(c), 50);
  assert.equal(uncachedTokens(c), 700);
  assert.equal(uncachedUnreported(c), 400);
  assert.equal(hitRate(fx.counts({ prompt_tokens: 1000 })), null);
  assert.equal(uncachedTokens(fx.counts({ prompt_tokens: 10, cached_tokens: 20 })), 0, "never negative");
});

test("the key filter lists each key once, by name, and knows whether keyless requests exist", () => {
  const rows = fx.usageToday().rows;
  assert.deepEqual(keyOptions(rows).map((o) => o.id), [11, 12, 13, 7].sort((x, y) => {
    const name = (id: number) => rows.find((r) => r.api_key_id === id)!.api_key_name;
    return name(x).localeCompare(name(y));
  }));
  assert.equal(hasKeylessRows(rows), true);
  assert.equal(hasKeylessRows(rows.filter((r) => r.api_key_id)), false);
});

// ------------------------------------------------------------------------ gpu

test("a card is idle below 5% mean utilization, and a card without a figure is not idle", () => {
  assert.equal(isIdle(4.99), true);
  assert.equal(isIdle(5), false);
  assert.equal(isIdle(null), false);
  assert.equal(mean([null, null]), null);
  assert.equal(mean([2, null, 4]), 3);
  assert.equal(memPercent(50, 200), 25);
  assert.equal(memPercent(null, 200), null);
  assert.equal(memPercent(50, 0), null);
});

test("the summary leaves cards without samples out of the means and counts the idle among the measured", () => {
  const d = fx.gpusToday();
  const s = summarize(d.cards, d.totals);
  // The deleted node's card is no card there now: three, as card_count says.
  assert.equal(s.cards, 3);
  assert.equal(s.cards, d.card_count);
  assert.deepEqual(s.byModel, [{ model: "RTX 6000D", count: 2 }, { model: "PPU 810E", count: 1 }]);
  assert.equal(s.powerTotal, 381);
  assert.equal(s.powerCards, 3);
  assert.equal(s.idle, 2);
  assert.equal(s.measured, 3);
  assert.ok(Math.abs(s.memPercentAvg! - (100 * 70 / 84 + 100 * 70 / 84 + 75) / 3) < 1e-9);
  assert.equal(summarize([], d.totals).powerTotal, null);
  // A gone card with samples over the range still counts in the means, not in
  // the count, the idle cards or the power drawn now.
  const gone = { ...d.cards[1], node_uuid: "node-c", node_name: "gone-node", node_deleted: true, present: false, gpu_uuid: "GPU-c1" };
  const withGone = summarize([...d.cards, gone], d.totals);
  assert.deepEqual([withGone.cards, withGone.idle, withGone.measured, withGone.powerTotal, withGone.powerCards], [3, 2, 3, 381, 3]);
  assert.ok(Math.abs(withGone.memPercentAvg! - (100 * 70 / 84 * 3 + 75) / 4) < 1e-9);
});

test("cards group by model or node with the deployments each holds, and count the ones no longer in the inventory", () => {
  const cards = fx.gpusToday().cards;
  const byModel = groupCards(cards, "model");
  // The deleted node's card is removed, not free: one of the three RTX cards is unassigned.
  assert.deepEqual(byModel.map((r) => [r.title, r.cardCount, r.unassigned, r.removed, r.nodeDeleted]), [["RTX 6000D", 3, 1, 1, false], ["PPU 810E", 1, 0, 0, false]]);
  assert.equal(byModel[0].utilAvg, (62.5 + 3.2) / 2);
  assert.equal(byModel[0].utilMax, 97);
  assert.deepEqual(byModel[0].deployments, [{ name: "glm-5-3-flash-nvfp4", cards: 1 }]);
  const byNode = groupCards(cards, "node");
  assert.deepEqual(byNode.map((r) => [r.title, r.nodeDeleted, r.unassigned]), [["6000*4", false, 1], ["PPU810E*16", false, 0], ["gone-node", true, 0]]);
  const byCard = groupCards(cards, "gpu");
  assert.equal(byCard.length, 4);
  assert.equal(byCard[3].title, "gone-node #0");
  assert.ok(byCard[3].nodeDeleted);
  // A card gone from a node still there is removed, its node not deleted.
  const [unplugged] = groupCards([{ ...cards[1], present: false }], "gpu");
  assert.deepEqual([unplugged.removed, unplugged.nodeDeleted, unplugged.unassigned], [1, false, 0]);
  assert.equal(byCard[3].utilAvg, null);
  assert.equal(byCard[3].powerAvg, null);
});

test("an answer without cards takes its rows and summary from the groups, without idle cards", () => {
  const d = fx.gpusFleet();
  const byModel = groupRows(d.groups, "model", (count) => `${count} nodes`);
  assert.deepEqual(byModel.map((r) => [r.title, r.sub, r.cardCount, r.unassigned, r.removed]), [["H100", "118 nodes", 944, 940, 0], ["L40S", "2 nodes", 16, 16, 0]]);
  assert.equal(byModel[0].utilAvg, 58.5);
  assert.ok(Math.abs(byModel[0].memAvgPercent! - 50) < 1e-9);
  assert.deepEqual(byModel[0].deployments, [{ name: "glm-5-3-flash-nvfp4", cards: 4 }]);
  const byNode = groupRows([{ ...d.groups[0], key: "node-a", label: "gpu-a", models: ["H100", "L40S"] }, { ...d.groups[0], key: "node-gone", label: "gpu-gone", node_deleted: true, cards: 0, nodes: 0, models: [], deployments: [], unassigned: 0 }], "node", () => "");
  assert.equal(byNode[0].sub, "H100 / L40S");
  assert.deepEqual(byNode.map((r) => [r.title, r.nodeDeleted, r.cardCount]), [["gpu-a", false, 944], ["gpu-gone", true, 0]]);
  const s = summarizeGroups(d);
  assert.equal(s.cards, 960);
  assert.deepEqual(s.byModel, [{ model: "H100", count: 944 }, { model: "L40S", count: 16 }]);
  assert.equal(s.idle, null);
  assert.equal(s.measured, null);
  assert.equal(s.powerTotal, 944 * 300 + 16 * 200);
  assert.equal(s.powerCards, 960);
  assert.ok(Math.abs(s.memPercentAvg! - (944 * 50 + 16 * 25) / 960) < 1e-9);
});

test("buckets before live accumulation count as filled in only when older history exists", async () => {
  const { leadingBackfilled } = await import("../src/pages/analytics/backfill.tsx");
  const H = 3_600_000;
  const base = { from_ms: 0, to_ms: 4 * H, granularity: "hour" as const, time_zone: "UTC", buckets: [0, H, 2 * H, 3 * H] };
  assert.equal(leadingBackfilled({ ...base, earliest_ms: H, live_since_ms: 2.5 * H }), 3);
  // A fresh installation started at 2:30: its oldest bucket is the 2:00 one it started in.
  assert.equal(leadingBackfilled({ ...base, earliest_ms: 2 * H, live_since_ms: 2.5 * H }), 0);
  assert.equal(leadingBackfilled({ ...base, earliest_ms: 0, live_since_ms: 2.5 * H }), 0);
  assert.equal(leadingBackfilled({ ...base, earliest_ms: H, live_since_ms: 0 }), 0);
  // By day: the day it started in is not history either.
  const D = 24 * H;
  const days = { ...base, granularity: "day" as const, buckets: [0, D, 2 * D] };
  assert.equal(leadingBackfilled({ ...days, earliest_ms: D, live_since_ms: D + 5 * H }), 0);
  assert.equal(leadingBackfilled({ ...days, earliest_ms: 0, live_since_ms: D + 5 * H }), 0);
  assert.equal(leadingBackfilled({ ...days, earliest_ms: 1, live_since_ms: 2 * D + 5 * H }), 3);
});
