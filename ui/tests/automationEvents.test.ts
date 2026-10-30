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

test("a wake a request waited for, and one that took the GPUs back, read in both languages", async () => {
  const en = await texts("en");
  assert.equal(en("AUTO_WAKE", { trigger: "request", instances: [uuid] }), "Woke instance 01234567 because a request arrived");
  assert.equal(en("AUTO_WAKE", { trigger: "resume", instances: [uuid] }), "Woke instance 01234567 once the GPUs it gave up were free again");
  const zh = await texts("zh");
  assert.equal(zh("AUTO_WAKE", { trigger: "request", instances: [uuid] }), "已唤醒实例 01234567，因请求到达");
  assert.equal(zh("AUTO_WAKE", { trigger: "resume", instances: [uuid] }), "已唤醒实例 01234567，让出的 GPU 已空出，恢复运行");
});

test("a sleep that gave GPUs to another deployment says to whom, why, and whether requests were cut off", async () => {
  const zh = await texts("zh");
  assert.equal(zh("AUTO_SLEEP", { trigger: "preempt", reason: "rank", for_deployment: 7, level: 2, abort: false, instances: [uuid] }), "已休眠（L2），部署 #7 的服务等级更高，为其让出 GPU");
  assert.equal(zh("AUTO_SLEEP", { trigger: "preempt", reason: "idle", for_deployment: 7, level: 2, abort: false, instances: [uuid] }), "已休眠（L2），空闲超过让卡时间，为 部署 #7 让出 GPU");
  assert.equal(zh("AUTO_SLEEP", { trigger: "preempt", reason: "turn", for_deployment: 9, level: 1, abort: true, instances: [uuid] }), "已休眠（L1），分时轮到 部署 #9，为其让出 GPU，中断了进行中的请求");
  const en = await texts("en");
  assert.equal(en("AUTO_SLEEP", { trigger: "preempt", reason: "rank", for_deployment: 7, level: 2, abort: true, instances: [uuid] }), "Put to sleep (L2) to give its GPUs to deployment #7, which has a higher service level; requests in flight were cut off");
  assert.equal(en("AUTO_SLEEP", { trigger: "preempt", reason: "turn", for_deployment: 9, level: 1, abort: false, instances: [uuid] }), "Put to sleep (L1) to give its GPUs to deployment #9, whose turn it is in the time share");
  assert.equal(en("AUTO_SLEEP", { trigger: "preempt", reason: "idle", for_deployment: 3, level: 2, abort: false, instances: [uuid] }), "Put to sleep (L2) to give its GPUs to deployment #3 after sitting idle past its hold time");
  // Without a target or a known reason the entry still says it gave GPUs up.
  assert.equal(en("AUTO_SLEEP", { trigger: "preempt", level: 2 }), "Put to sleep (L2) to give its GPUs to another deployment");
});

test("a failed sleep that was ordered to give GPUs up is not worded as a scheduled one", async () => {
  const en = await texts("en");
  assert.equal(en("SCHEDULE_ACTION_FAILED", { trigger: "preempt", action: "sleep", reason: "engine busy" }), "Failed to sleep to give up GPUs: engine busy");
  const zh = await texts("zh");
  assert.equal(zh("SCHEDULE_ACTION_FAILED", { trigger: "preempt", action: "sleep", reason: "引擎忙" }), "让出 GPU 时休眠失败：引擎忙");
});
