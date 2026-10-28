import assert from "node:assert/strict";
import test from "node:test";

import fs from "node:fs";
import { createInstance } from "i18next";

import { instanceEventStage, instanceEventText, lifecycleEventCodes } from "../src/pages/instanceEvents.ts";

const tDeployments = ((key: string, options: Record<string, unknown>) => {
  const values: Record<string, string> = {
    "instance.event.INSTANCE_CREATED": `created (${options.state})`,
    "instance.event.INSTANCE_STATE_CHANGED": `changed to ${options.state}`,
    "instance.event.INSTANCE_STATUS_UPDATED": `reported as ${options.state}`,
    "instance.event.INSTANCE_SLEEP_RESULT": `sleep ${options.result} (L${options.level})`,
    "instance.phase.image_pull": "Downloading Image",
  };
  return values[key] ?? String(options.defaultValue ?? key);
}) as never;

const tCommon = ((key: string, options: Record<string, unknown>) => (
  key === "states.running" ? "Running" : String(options.defaultValue ?? key)
)) as never;

test("structured instance events translate state and phase but preserve raw detail", () => {
  assert.equal(instanceEventText(tDeployments, tCommon, {
    kind: "state",
    code: "INSTANCE_STATUS_UPDATED",
    params: { state: "running", phase: "image_pull", detail: "layer 2/4" },
    created_at: "2026-08-25T00:00:00Z",
  }), "reported as Running [Downloading Image]: layer 2/4");
});

test("sleep result events show the result and effective level", () => {
  assert.equal(instanceEventText(tDeployments, tCommon, {
    kind: "sleep",
    code: "INSTANCE_SLEEP_RESULT",
    params: { result: "failed_rollback", target: 1, actual: 0, detail: "engine timeout" },
    created_at: "2026-08-25T00:00:00Z",
  }), "sleep failed_rollback (L0): engine timeout");
});

test("historical instance event messages remain unchanged", () => {
  assert.equal(instanceEventText(tDeployments, tCommon, {
    kind: "state",
    message: "state → running",
    created_at: "2026-08-25T00:00:00Z",
  }), "state → running");
  assert.equal(instanceEventText(tDeployments, tCommon, {
    kind: "state",
    code: "FUTURE_EVENT",
    message: "future fallback",
    created_at: "2026-08-25T00:00:00Z",
  }), "future fallback");
});

test("startup stages read new params and historical data", () => {
  assert.deepEqual(instanceEventStage({
    kind: "state",
    code: "INSTANCE_STATUS_UPDATED",
    params: { phase: "image_pull", detail: "layer 2/4" },
    data: JSON.stringify({ phase: "image_pull" }),
    created_at: "2026-08-25T00:00:00Z",
  }), { phase: "image_pull", message: "layer 2/4" });
  assert.deepEqual(instanceEventStage({
    kind: "state",
    message: "legacy stage",
    data: JSON.stringify({ phase: "container_start" }),
    created_at: "2026-08-25T00:00:00Z",
  }), { phase: "container_start", message: "legacy stage" });
});

test("every lifecycle event has text in both locales, and a stalled rolling restart names its replica", async () => {
  const bundle = (lang: string, ns: string) => JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/${lang}/${ns}.json`, import.meta.url), "utf8"));
  for (const lang of ["en", "zh"]) {
    const events = bundle(lang, "deployments").instance.event;
    for (const code of lifecycleEventCodes) assert.ok(events[code], `${lang} has text for ${code}`);
  }
  const i18n = createInstance();
  await i18n.init({ lng: "en", resources: { en: { deployments: bundle("en", "deployments"), common: bundle("en", "common") } }, interpolation: { escapeValue: false } });
  assert.equal(instanceEventText(i18n.getFixedT("en", "deployments") as never, i18n.getFixedT("en", "common") as never, {
    kind: "schedule",
    code: "DEPLOYMENT_ROLLING_STALLED",
    params: { role: "", replica: 2, attempts: 3 },
    created_at: "2026-09-30T00:00:00Z",
  }), "Rolling restart waits: replica 2 gave up after 3 failed attempts; retry it to continue");
  // On a PD deployment prefill and decode each count from 1, so the role says
  // which replica to retry.
  assert.equal(instanceEventText(i18n.getFixedT("en", "deployments") as never, i18n.getFixedT("en", "common") as never, {
    kind: "schedule",
    code: "DEPLOYMENT_ROLLING_STALLED",
    params: { role: "prefill", replica: 2, attempts: 3 },
    created_at: "2026-09-30T00:00:00Z",
  }), "Rolling restart waits: replica Prefill 2 gave up after 3 failed attempts; retry it to continue");
  // A rebuilt replica says which attempt it is, where it starts and why the
  // last one failed; a failure without a message drops the clause.
  const t = i18n.getFixedT("en", "deployments") as never;
  const tc = i18n.getFixedT("en", "common") as never;
  assert.equal(instanceEventText(t, tc, {
    kind: "schedule", code: "REPLICA_REBUILT", params: { role: "decode", replica: 1, attempt: 2, node: "gpu-3", reason: "CUDA out of memory" }, created_at: "2026-09-30T00:00:00Z",
  }), "Replica Decode 1 failed and was rebuilt automatically (attempt 2, on gpu-3); it failed with: CUDA out of memory");
  assert.equal(instanceEventText(t, tc, {
    kind: "schedule", code: "REPLICA_REBUILT", params: { replica: 3, attempt: 3, node: "gpu-3", reason: "" }, created_at: "2026-09-30T00:00:00Z",
  }), "Replica 3 failed and was rebuilt automatically (attempt 3, on gpu-3)");
});

test("a storage outage on the timeline names the storage, the node and the localized state", async () => {
  const bundle = (lang: string, ns: string) => JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/${lang}/${ns}.json`, import.meta.url), "utf8"));
  const i18n = createInstance();
  await i18n.init({ lng: "zh", resources: { zh: { deployments: bundle("zh", "deployments"), common: bundle("zh", "common") } }, interpolation: { escapeValue: false } });
  const t = i18n.getFixedT("zh", "deployments") as never;
  const tc = i18n.getFixedT("zh", "common") as never;
  assert.equal(instanceEventText(t, tc, {
    kind: "schedule",
    code: "STORAGE_UNAVAILABLE",
    params: { storage: "nfs-test", node: "gpu3", state: "unresponsive", reason: "statfs: filesystem did not respond" },
    created_at: "2026-10-02T17:30:00Z",
  }), "存储 nfs-test 在节点 gpu3 上不可用（无响应）：运行中的实例继续用显存里的权重服务，正在加载的实例会等它恢复");
  assert.equal(instanceEventText(t, tc, {
    kind: "schedule",
    code: "STORAGE_RECOVERED",
    params: { storage: "nfs-test", node: "gpu3" },
    created_at: "2026-10-02T17:34:00Z",
  }), "存储 nfs-test 在节点 gpu3 上已恢复");
});
