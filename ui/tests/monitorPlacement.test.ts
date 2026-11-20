import assert from "node:assert/strict";
import fs from "node:fs";
import { test } from "node:test";
import { createInstance } from "i18next";
import { initReactI18next } from "react-i18next";
import type { MonitorPlacement } from "../src/api";
import { formatPlacement } from "../src/pages/monitor/placement";

async function translator(lng: "en" | "zh") {
  const i18n = createInstance();
  await i18n.use(initReactI18next).init({
    lng,
    fallbackLng: "en",
    resources: {
      en: { monitoring: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/monitoring.json", import.meta.url), "utf8")) },
      zh: { monitoring: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/zh/monitoring.json", import.meta.url), "utf8")) },
    },
    defaultNS: "monitoring",
    interpolation: { escapeValue: false },
  });
  return i18n.getFixedT(lng, "monitoring");
}

const spark: MonitorPlacement = { nodes: ["gb10-1", "gb10-2", "gb10-3", "gb10-4"], nnodes: 4, gpus_per_node: 1 };

test("a multi-node replica reads as nodes x cards, in the server's rank order, with no TP the server did not send", async () => {
  assert.equal(formatPlacement(await translator("en"), spark), "4 nodes × 1 GPU · gb10-1, gb10-2, gb10-3, gb10-4");
  assert.equal(formatPlacement(await translator("zh"), spark), "4 节点 × 1 卡 · gb10-1, gb10-2, gb10-3, gb10-4");
});

test("the stated tensor-parallel degree leads, beside the node and card counts", async () => {
  const stated = { ...spark, tensor_parallel: 4 };
  assert.equal(formatPlacement(await translator("en"), stated), "TP4 · 4 nodes × 1 GPU · gb10-1, gb10-2, gb10-3, gb10-4");
  assert.equal(formatPlacement(await translator("zh"), stated), "TP4 · 4 节点 × 1 卡 · gb10-1, gb10-2, gb10-3, gb10-4");
});

test("single-node placement keeps the familiar TP4 · node form", async () => {
  const single: MonitorPlacement = { nodes: ["gpu3"], nnodes: 1, gpus_per_node: 4, tensor_parallel: 4 };
  assert.equal(formatPlacement(await translator("en"), single), "TP4 · gpu3");
  assert.equal(formatPlacement(await translator("zh"), single), "TP4 · gpu3");
});

test("single-node placement without a stated TP shows the card count, pluralised, never a TP", async () => {
  const en = await translator("en");
  assert.equal(formatPlacement(en, { nodes: ["gpu3"], nnodes: 1, gpus_per_node: 4 }), "4 GPUs · gpu3");
  assert.equal(formatPlacement(en, { nodes: ["gpu3"], nnodes: 1, gpus_per_node: 1 }), "1 GPU · gpu3");
  assert.equal(formatPlacement(await translator("zh"), { nodes: ["gpu3"], nnodes: 1, gpus_per_node: 4 }), "4 卡 · gpu3");
});

test("a deployment with no instances yet says so instead of printing an empty string", async () => {
  const none: MonitorPlacement = { nodes: [], nnodes: 0, gpus_per_node: 0 };
  assert.equal(formatPlacement(await translator("en"), none), "Not placed yet");
  assert.equal(formatPlacement(await translator("zh"), none), "尚未放置");
});
