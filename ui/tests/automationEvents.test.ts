import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";

import { createInstance } from "i18next";

import { automationEventCodes } from "../src/pages/automationEvents.ts";
import { instanceEventText } from "../src/pages/instanceEvents.ts";

const bundle = (lang: string, ns: string) => JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/${lang}/${ns}.json`, import.meta.url), "utf8"));

async function texts(lang: "en" | "zh") {
  const i18n = createInstance();
  await i18n.init({ lng: lang, resources: { [lang]: { deployments: bundle(lang, "deployments"), common: bundle(lang, "common") } }, interpolation: { escapeValue: false } });
  const t = i18n.getFixedT(lang, "deployments") as never;
  const tc = i18n.getFixedT(lang, "common") as never;
  return (code: string, params: Record<string, unknown>) =>
    instanceEventText(t, tc, { id: 1, kind: "automation", code, params, created_at: "2026-10-25T00:00:00Z" });
}

const uuid = "0123456789abcdef";

test("every automation event has text in both locales", () => {
  for (const lang of ["en", "zh"]) {
    const events = bundle(lang, "deployments").instance.event;
    for (const code of automationEventCodes) assert.ok(events[code], `${lang} has text for ${code}`);
  }
});

test("a schedule's, an idle sleep's and a waiting wake's entries read in English with their causes", async () => {
  const text = await texts("en");
  assert.equal(text("AUTO_SLEEP", { trigger: "schedule", schedule_id: 3, level: 2, scheduled_at: "2026-10-25T22:00:00Z" }), "Put to sleep (L2) by its schedule");
  assert.equal(text("AUTO_SLEEP", { trigger: "idle", level: 2, idle_sec: 900 }), "Put to sleep (L2) after 15 min without requests");
  assert.equal(text("AUTO_SLEEP", { trigger: "idle", level: 1, idle_sec: 7200 }), "Put to sleep (L1) after 2 h without requests");
  assert.equal(text("AUTO_WAKE", { trigger: "wake_waiting", instances: [uuid, "fedcba9876543210"] }), "Woke instance 01234567, fedcba98 once its GPUs had room");
  assert.equal(text("AUTO_WAKE", { trigger: "schedule", schedule_id: 1, instances: [uuid] }), "Woke instance 01234567 by its schedule");
  assert.equal(text("AUTO_SCALE", { trigger: "schedule", schedule_id: 2, replicas: 4 }), "Replicas set to 4 by its schedule");
});

test("waiting, giving up and yielding entries name the instances, the wait and who gave way", async () => {
  const text = await texts("en");
  assert.equal(text("WAKE_WAITING", { trigger: "operator", instances: [uuid], yielded: ["other", "third"] }), "Wake of instance 01234567 waits for room, after putting other, third to sleep");
  assert.equal(text("WAKE_WAITING", { trigger: "schedule", instances: [uuid] }), "Wake of instance 01234567 waits for room");
  assert.equal(text("WAKE_WAIT_TIMEOUT", { trigger: "wake_waiting", instances: [uuid], waited_sec: 1800 }), "Stopped waiting for room to wake instance 01234567 after 30 min");
  assert.equal(text("YIELDED_FOR_WAKE", { deployment: "qwen", instances: [uuid], level: 1 }), "Put to sleep (L1) to make room for waking qwen");
  assert.equal(text("SCHEDULE_ACTION_FAILED", { trigger: "schedule", action: "sleep", reason: "deployment cannot sleep", reason_code: "SLEEP_NOT_ENABLED", schedule_id: 3 }), "Scheduled sleep failed: deployment cannot sleep");
});

test("the same entries read in Chinese, and a payload without params still renders", async () => {
  const text = await texts("zh");
  assert.equal(text("AUTO_SLEEP", { trigger: "idle", level: 2, idle_sec: 900 }), "已休眠（L2），因 15 分钟内没有请求");
  assert.equal(text("AUTO_SCALE", { trigger: "schedule", schedule_id: 2, replicas: 4 }), "副本数已设为 4，由定时计划触发");
  assert.ok(text("AUTO_WAKE", {}).length > 0);
});
