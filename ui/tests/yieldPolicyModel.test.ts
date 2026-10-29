import assert from "node:assert/strict";
import { test } from "node:test";
import type { YieldPolicyName } from "../src/api.ts";
import {
  applicable,
  canAutoSleep,
  canYield,
  draftEqual,
  draftErrors,
  draftOfParams,
  draftOfPolicy,
  draftOfPreset,
  paramsOfDraft,
  presetOf,
  presetOfDraft,
  presetParams,
  requestOfDraft,
  savedPolicy,
  type YieldDraft,
} from "../src/pages/yieldPolicy/model.ts";

// The pure rules behind the yield policy editor: the presets table (equal to
// the server's), which parameters apply to which policy, the preset a set of
// parameters amounts to, the ranges, and the request a draft saves.

const presets = ["resident", "online", "shared", "offline"] as const;

test("the presets table equals the server's normalized presets, key by key", () => {
  assert.deepEqual(presetParams.resident, { policy: "never", auto_sleep: false });
  assert.deepEqual(presetParams.online, { policy: "idle", auto_sleep: false, idle_sec: 300, sleep_level: "auto", hold_max_sec: 120 });
  assert.deepEqual(presetParams.shared, {
    policy: "timeslice", auto_sleep: false, idle_sec: 60, sleep_level: "auto", min_serve_sec: 120,
    unfinished: "wait", unfinished_wait_sec: 30, hold_max_sec: 600,
  });
  assert.deepEqual(presetParams.offline, { policy: "anytime", auto_sleep: false, sleep_level: "auto", unfinished: "abort", hold_max_sec: 1800 });
});

test("a preset's full draft reads back as that preset and normalizes to its table row", () => {
  for (const name of presets) {
    const draft = draftOfPreset(name);
    assert.equal(presetOfDraft(draft), name, name);
    assert.deepEqual(paramsOfDraft(draft), presetParams[name], name);
    assert.equal(presetOf(presetParams[name]), name, name);
  }
});

test("every parameter of a preset, changed, makes it custom, and changed back makes it the preset again", () => {
  const edits: Array<[typeof presets[number], Partial<YieldDraft>]> = [
    ["online", { idle_sec: 301 }],
    ["online", { sleep_level: "l1" }],
    ["online", { hold_max_sec: 121 }],
    ["online", { auto_sleep: true }],
    ["shared", { idle_sec: 61 }],
    ["shared", { min_serve_sec: 0 }],
    ["shared", { unfinished: "abort" }],
    ["shared", { unfinished: "wait_unbounded" }],
    ["shared", { unfinished_wait_sec: 31 }],
    ["shared", { hold_max_sec: 601 }],
    ["offline", { unfinished: "wait" }],
    ["offline", { sleep_level: "l2" }],
    ["offline", { hold_max_sec: 1799 }],
    ["offline", { auto_sleep: true }],
    ["resident", { auto_sleep: true }],
  ];
  for (const [name, patch] of edits) {
    const base = draftOfPreset(name);
    const changed = { ...base, ...patch };
    assert.equal(presetOfDraft(changed), "custom", `${name} ${JSON.stringify(patch)}`);
    const back = { ...changed, ...Object.fromEntries(Object.keys(patch).map((key) => [key, base[key as keyof YieldDraft]])) } as YieldDraft;
    assert.equal(presetOfDraft(back), name, `${name} back`);
  }
});

test("a parameter that does not apply is no difference: it neither makes a preset custom nor a draft dirty", () => {
  // Resident ignores everything but auto_sleep, whatever the draft remembers.
  assert.equal(presetOfDraft({ ...draftOfPreset("resident"), idle_sec: 999, hold_max_sec: 15, unfinished: "abort" }), "resident");
  // Online has no minimum serving time or unfinished-request handling.
  assert.equal(presetOfDraft({ ...draftOfPreset("online"), min_serve_sec: 5, unfinished: "abort", unfinished_wait_sec: 2 }), "online");
  // Offline waits for nobody: the wait seconds only apply to the wait mode.
  assert.equal(presetOfDraft({ ...draftOfPreset("offline"), unfinished_wait_sec: 5, idle_sec: 7 }), "offline");
  assert.equal(draftEqual({ ...draftOfPreset("online"), min_serve_sec: 5 }, draftOfPreset("online")), true);
  assert.equal(draftEqual({ ...draftOfPreset("online"), idle_sec: 301 }, draftOfPreset("online")), false);
  // The wait seconds apply to wait only.
  assert.equal(draftEqual({ ...draftOfPreset("shared"), unfinished: "wait_unbounded", unfinished_wait_sec: 99 }, { ...draftOfPreset("shared"), unfinished: "wait_unbounded" }), true);
});

test("the same parameters with automatic sleep are custom, whichever policy they are on", () => {
  const custom = { policy: "never", auto_sleep: true, idle_sec: 600, sleep_level: "l1", hold_max_sec: 120 } as const;
  assert.equal(presetOf(custom), "custom");
  assert.deepEqual(paramsOfDraft(draftOfParams(custom)), custom);
});

const policies: YieldPolicyName[] = ["never", "idle", "timeslice", "anytime"];

test("which parameters apply, for every policy with automatic sleep off and on", () => {
  const expected = (policy: YieldPolicyName, autoSleep: boolean) => ({
    idleSec: policy === "idle" || policy === "timeslice" || autoSleep,
    sleepLevel: policy !== "never" || autoSleep,
    minServeSec: policy === "timeslice",
    unfinished: policy === "timeslice" || policy === "anytime",
    holdMaxSec: policy !== "never" || autoSleep,
  });
  for (const policy of policies) {
    for (const autoSleep of [false, true]) {
      const use = applicable({ policy, auto_sleep: autoSleep, unfinished: "abort" });
      const want = expected(policy, autoSleep);
      assert.equal(use.idleSec, want.idleSec, `${policy}/${autoSleep} idle`);
      assert.equal(use.sleepLevel, want.sleepLevel, `${policy}/${autoSleep} level`);
      assert.equal(use.minServeSec, want.minServeSec, `${policy}/${autoSleep} min serve`);
      assert.equal(use.unfinished, want.unfinished, `${policy}/${autoSleep} unfinished`);
      assert.equal(use.holdMaxSec, want.holdMaxSec, `${policy}/${autoSleep} hold`);
      assert.equal(use.unfinishedWaitSec, false, "abort needs no wait seconds");
    }
  }
  assert.equal(applicable({ policy: "timeslice", auto_sleep: false, unfinished: "wait" }).unfinishedWaitSec, true);
  assert.equal(applicable({ policy: "anytime", auto_sleep: false, unfinished: "wait" }).unfinishedWaitSec, true);
  assert.equal(applicable({ policy: "timeslice", auto_sleep: false, unfinished: "wait_unbounded" }).unfinishedWaitSec, false);
  assert.equal(applicable({ policy: "idle", auto_sleep: false, unfinished: "wait" }).unfinishedWaitSec, false, "idle never has unfinished-request handling");
});

test("choosing a policy fills that policy's defaults and keeps automatic sleep", () => {
  assert.deepEqual(paramsOfDraft(draftOfPolicy("idle", false)), presetParams.online);
  assert.deepEqual(paramsOfDraft(draftOfPolicy("timeslice", false)), presetParams.shared);
  assert.deepEqual(paramsOfDraft(draftOfPolicy("anytime", false)), presetParams.offline);
  assert.deepEqual(paramsOfDraft(draftOfPolicy("never", false)), presetParams.resident);
  const never = draftOfPolicy("never", true);
  assert.equal(never.auto_sleep, true);
  assert.deepEqual(paramsOfDraft(never), { policy: "never", auto_sleep: true, idle_sec: 300, sleep_level: "auto", hold_max_sec: 120 });
});

test("ranges: out of range and cleared fields are refused, only where the field applies", () => {
  const shared = draftOfPreset("shared");
  assert.deepEqual(draftErrors(shared), []);
  const bad = draftErrors({ ...shared, idle_sec: 29, min_serve_sec: 3601, unfinished_wait_sec: 0, hold_max_sec: 1801 });
  assert.deepEqual(bad.map((item) => [item.field, item.reason, item.minimum, item.maximum]), [
    ["idle_sec", "out_of_range", 30, 604800],
    ["min_serve_sec", "out_of_range", 0, 3600],
    ["unfinished_wait_sec", "out_of_range", 1, 600],
    ["hold_max_sec", "out_of_range", 10, 1800],
  ]);
  assert.deepEqual(draftErrors({ ...shared, idle_sec: 30, min_serve_sec: 0, unfinished_wait_sec: 600, hold_max_sec: 10 }), []);
  assert.deepEqual(draftErrors({ ...shared, idle_sec: 604800 }), []);
  assert.equal(draftErrors({ ...shared, idle_sec: 604801 })[0].reason, "out_of_range");
  assert.equal(draftErrors({ ...shared, idle_sec: 30.5 })[0].reason, "out_of_range");
  assert.equal(draftErrors({ ...shared, idle_sec: null })[0].reason, "required");
  // A cleared or bad value that does not apply is nobody's business.
  assert.deepEqual(draftErrors({ ...draftOfPreset("resident"), idle_sec: null, hold_max_sec: 1 }), []);
  assert.deepEqual(draftErrors({ ...draftOfPreset("online"), min_serve_sec: null, unfinished_wait_sec: null }), []);
  assert.deepEqual(draftErrors({ ...shared, unfinished: "abort", unfinished_wait_sec: null }), []);
  assert.equal(draftErrors({ ...draftOfPreset("resident"), auto_sleep: true, idle_sec: 5 })[0].field, "idle_sec");
});

test("the request is the policy with the parameters that apply, as literal JSON", () => {
  assert.equal(JSON.stringify(requestOfDraft(draftOfPreset("online"))), '{"policy":"idle","auto_sleep":false,"idle_sec":300,"sleep_level":"auto","hold_max_sec":120}');
  assert.equal(JSON.stringify(requestOfDraft(draftOfPreset("resident"))), '{"policy":"never","auto_sleep":false}');
  assert.equal(
    JSON.stringify(requestOfDraft({ ...draftOfPreset("shared"), unfinished: "wait_unbounded" })),
    '{"policy":"timeslice","auto_sleep":false,"idle_sec":60,"sleep_level":"auto","min_serve_sec":120,"unfinished":"wait_unbounded","hold_max_sec":600}',
  );
});

test("a deployment's saved policy: absent is resident, the derived preset key is dropped, and eligibility", () => {
  const dep = (extra: Record<string, unknown> = {}) => ({ id: 1, sleepable: true, nnodes: 1, ...extra }) as never;
  assert.deepEqual(savedPolicy(dep()), presetParams.resident);
  assert.deepEqual(savedPolicy(dep({ yield_policy: { preset: "online", ...presetParams.online } })), presetParams.online);
  assert.equal(presetOf(savedPolicy(dep({ yield_policy: { preset: "custom", policy: "never", auto_sleep: true, idle_sec: 600, sleep_level: "l1", hold_max_sec: 120 } }))), "custom");
  assert.equal(canYield(dep()), true);
  assert.equal(canYield(dep({ sleepable: false })), false);
  assert.equal(canYield(dep({ nnodes: 2 })), false);
  assert.equal(canYield(dep({ pd: { mode: "ep", roles: [] } })), false);
  assert.equal(canAutoSleep(dep({ nnodes: 2 })), true, "automatic sleep needs only a deployment that can sleep");
  assert.equal(canAutoSleep(dep({ sleepable: false })), false);
});
