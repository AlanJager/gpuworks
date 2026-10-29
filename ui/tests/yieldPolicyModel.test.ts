import assert from "node:assert/strict";
import fs from "node:fs";
import { test } from "node:test";
import type { YieldPolicyDictionary } from "../src/api.ts";
import {
  applicable,
  boundOf,
  canYield,
  chooseUnfinished,
  draftEqual,
  draftErrors,
  draftOfParams,
  draftOfPolicy,
  modifiedFields,
  paramsOfDraft,
  paramsOfValue,
  requestOfDraft,
  savedPolicy,
  type YieldDraft,
} from "../src/pages/yieldPolicy/model.ts";

// The pure rules behind the yield policy editor. Defaults, applicability and
// bounds all come from the server's dictionary (the literal answer of GET
// /scheduling/yield-policies); the model holds none of its own.

const dictionary = JSON.parse(fs.readFileSync(new URL("./fixtures/yield-policies.json", import.meta.url), "utf8")) as YieldPolicyDictionary;
const policies = ["never", "idle", "timeslice", "anytime"] as const;

test("choosing a policy fills in that policy's defaults from the dictionary", () => {
  assert.deepEqual(draftOfPolicy(dictionary, "never"), { policy: "never" });
  assert.deepEqual(draftOfPolicy(dictionary, "idle"), { policy: "idle", idle_sec: 60, sleep_level: "auto", hold_max_sec: 300 });
  assert.deepEqual(draftOfPolicy(dictionary, "timeslice"), {
    policy: "timeslice", idle_sec: 30, sleep_level: "auto", max_serve_sec: 120, unfinished: "wait", unfinished_wait_sec: 30, hold_max_sec: 600,
  });
  assert.deepEqual(draftOfPolicy(dictionary, "anytime"), {
    policy: "anytime", idle_sec: 30, sleep_level: "auto", unfinished: "abort", unfinished_wait_sec: 30, hold_max_sec: 3600,
  });
  // Another dictionary, other defaults: nothing is remembered by the model.
  const other = structuredClone(dictionary);
  other.policies[1].defaults.idle_sec = 99;
  assert.equal(draftOfPolicy(other, "idle").idle_sec, 99);
});

test("which parameters apply comes from the dictionary, and the wait time only while unfinished is wait", () => {
  const names = (draft: YieldDraft) => [...applicable(dictionary, draft)].sort();
  assert.deepEqual(names({ policy: "never" }), []);
  assert.deepEqual(names(draftOfPolicy(dictionary, "idle")), ["hold_max_sec", "idle_sec", "sleep_level"]);
  assert.deepEqual(names(draftOfPolicy(dictionary, "timeslice")), ["hold_max_sec", "idle_sec", "max_serve_sec", "sleep_level", "unfinished", "unfinished_wait_sec"]);
  // anytime lists the wait time but holds abort: it does not apply.
  assert.deepEqual(names(draftOfPolicy(dictionary, "anytime")), ["hold_max_sec", "idle_sec", "sleep_level", "unfinished"]);
  assert.deepEqual(names({ ...draftOfPolicy(dictionary, "anytime"), unfinished: "wait" }), ["hold_max_sec", "idle_sec", "sleep_level", "unfinished", "unfinished_wait_sec"]);
  assert.deepEqual(names({ ...draftOfPolicy(dictionary, "timeslice"), unfinished: "abort" }), ["hold_max_sec", "idle_sec", "max_serve_sec", "sleep_level", "unfinished"]);
});

test("the request is the policy with the parameters that apply, none of them null, in wire order", () => {
  assert.equal(JSON.stringify(requestOfDraft(dictionary, draftOfPolicy(dictionary, "never"))), '{"policy":"never"}');
  assert.equal(
    JSON.stringify(requestOfDraft(dictionary, draftOfPolicy(dictionary, "timeslice"))),
    '{"policy":"timeslice","idle_sec":30,"sleep_level":"auto","max_serve_sec":120,"unfinished":"wait","unfinished_wait_sec":30,"hold_max_sec":600}',
  );
  assert.equal(
    JSON.stringify(requestOfDraft(dictionary, draftOfPolicy(dictionary, "anytime"))),
    '{"policy":"anytime","idle_sec":30,"sleep_level":"auto","unfinished":"abort","hold_max_sec":3600}',
    "abort carries no wait time",
  );
  // A parameter that does not apply to the policy is left out, a cleared one too.
  const stale: YieldDraft = { ...draftOfPolicy(dictionary, "idle"), max_serve_sec: 500, unfinished: "wait", hold_max_sec: null };
  assert.deepEqual(requestOfDraft(dictionary, stale), { policy: "idle", idle_sec: 60, sleep_level: "auto" });
  assert.deepEqual(paramsOfDraft(dictionary, { policy: "never", idle_sec: 60 }), { policy: "never" });
});

test("a draft equals a saved policy when they amount to the same parameters", () => {
  const saved = { policy: "idle", idle_sec: 60, sleep_level: "auto", hold_max_sec: 300 } as const;
  assert.equal(draftEqual(dictionary, draftOfParams(saved), saved), true);
  assert.equal(draftEqual(dictionary, { ...draftOfParams(saved), idle_sec: 61 }, saved), false);
  // A parameter that does not apply is no difference, on either side.
  assert.equal(draftEqual(dictionary, { ...draftOfParams(saved), max_serve_sec: 5 }, saved), true);
  assert.equal(draftEqual(dictionary, draftOfParams(saved), { ...saved, max_serve_sec: 5 } as never), true);
  assert.equal(draftEqual(dictionary, { policy: "never" }, saved), false);
  // A cleared field differs from what is saved.
  assert.equal(draftEqual(dictionary, { ...draftOfParams(saved), idle_sec: null }, saved), false);
});

test("modifiedFields lists the applicable parameters that differ from the chosen policy's defaults", () => {
  for (const policy of policies) assert.equal(modifiedFields(dictionary, draftOfPolicy(dictionary, policy)).size, 0, policy);
  const timeslice = draftOfPolicy(dictionary, "timeslice");
  assert.deepEqual([...modifiedFields(dictionary, { ...timeslice, idle_sec: 31, hold_max_sec: 601 })].sort(), ["hold_max_sec", "idle_sec"]);
  assert.deepEqual([...modifiedFields(dictionary, { ...timeslice, sleep_level: "l2", max_serve_sec: 121 })].sort(), ["max_serve_sec", "sleep_level"]);
  assert.deepEqual([...modifiedFields(dictionary, { ...timeslice, idle_sec: null })], ["idle_sec"], "a cleared field differs");
  // The wait time counts only while it applies.
  assert.deepEqual([...modifiedFields(dictionary, { ...timeslice, unfinished: "abort", unfinished_wait_sec: 99 })], ["unfinished"]);
  assert.deepEqual([...modifiedFields(dictionary, { ...timeslice, unfinished_wait_sec: 99 })], ["unfinished_wait_sec"]);
  // The same value is a default for one policy and a change for another.
  assert.deepEqual([...modifiedFields(dictionary, { ...draftOfPolicy(dictionary, "idle"), idle_sec: 30 })], ["idle_sec"]);
  assert.equal(modifiedFields(dictionary, { ...draftOfPolicy(dictionary, "anytime"), idle_sec: 30 }).size, 0);
  assert.equal(modifiedFields({ ...dictionary, policies: [] }, timeslice).size, 0, "no entry, nothing to compare");
});

test("turning unfinished to wait brings the policy's default wait time when the draft holds none", () => {
  const abort = { policy: "timeslice", unfinished: "abort", unfinished_wait_sec: null } as YieldDraft;
  assert.deepEqual(chooseUnfinished(dictionary, abort, "wait"), { policy: "timeslice", unfinished: "wait", unfinished_wait_sec: 30 });
  assert.equal(chooseUnfinished(dictionary, { ...abort, unfinished_wait_sec: 77 }, "wait").unfinished_wait_sec, 77, "a typed value stays");
  assert.equal(chooseUnfinished(dictionary, { policy: "anytime", unfinished: "abort" }, "wait").unfinished_wait_sec, 30);
  assert.equal(chooseUnfinished(dictionary, draftOfPolicy(dictionary, "timeslice"), "abort").unfinished, "abort");
});

test("numbers are held to the dictionary's bounds, and a cleared applicable field is required", () => {
  const timeslice = draftOfPolicy(dictionary, "timeslice");
  assert.deepEqual(draftErrors(dictionary, timeslice), []);
  const cases: Array<[Partial<YieldDraft>, string, "out_of_range" | "required", number, number]> = [
    [{ idle_sec: 14 }, "idle_sec", "out_of_range", 15, 120],
    [{ max_serve_sec: 29, idle_sec: 15 }, "max_serve_sec", "out_of_range", 30, 86400],
    [{ max_serve_sec: 86401 }, "max_serve_sec", "out_of_range", 30, 86400],
    [{ unfinished_wait_sec: 0 }, "unfinished_wait_sec", "out_of_range", 1, 600],
    [{ unfinished_wait_sec: 601 }, "unfinished_wait_sec", "out_of_range", 1, 600],
    [{ hold_max_sec: 9 }, "hold_max_sec", "out_of_range", 10, 3600],
    [{ hold_max_sec: 3601 }, "hold_max_sec", "out_of_range", 10, 3600],
    [{ hold_max_sec: 10.5 }, "hold_max_sec", "out_of_range", 10, 3600],
    [{ idle_sec: null }, "idle_sec", "required", 15, 120],
    [{ hold_max_sec: null }, "hold_max_sec", "required", 10, 3600],
  ];
  for (const [patch, field, reason, minimum, maximum] of cases) {
    assert.deepEqual(draftErrors(dictionary, { ...timeslice, ...patch }), [{ field, reason, minimum, maximum }], JSON.stringify(patch));
  }
  // The boundaries are accepted.
  assert.deepEqual(draftErrors(dictionary, { ...timeslice, idle_sec: 15, max_serve_sec: 30, unfinished_wait_sec: 600, hold_max_sec: 10 }), []);
  assert.deepEqual(draftErrors(dictionary, { ...timeslice, idle_sec: 120, max_serve_sec: 86400, hold_max_sec: 3600 }), []);
  // A field that does not apply is not checked.
  assert.deepEqual(draftErrors(dictionary, { ...timeslice, unfinished: "abort", unfinished_wait_sec: 0 }), []);
  assert.deepEqual(draftErrors(dictionary, { ...draftOfPolicy(dictionary, "idle"), max_serve_sec: 1 }), []);
  assert.deepEqual(draftErrors(dictionary, { policy: "never", idle_sec: 1 }), []);
});

test("idle_sec may not exceed max_serve_sec when both apply, and the bounds come from the dictionary", () => {
  const timeslice = draftOfPolicy(dictionary, "timeslice");
  assert.deepEqual(boundOf(dictionary, timeslice, "idle_sec"), { minimum: 15, maximum: 120 });
  assert.deepEqual(boundOf(dictionary, timeslice, "hold_max_sec"), { minimum: 10, maximum: 3600 });
  assert.deepEqual(draftErrors(dictionary, { ...timeslice, idle_sec: 121 }), [{ field: "idle_sec", reason: "out_of_range", minimum: 15, maximum: 120 }]);
  assert.deepEqual(draftErrors(dictionary, { ...timeslice, idle_sec: 120 }), []);
  assert.deepEqual(draftErrors(dictionary, { ...timeslice, idle_sec: 604800, max_serve_sec: 86400 }), [{ field: "idle_sec", reason: "out_of_range", minimum: 15, maximum: 86400 }]);
  // Without max_serve_sec in play (idle policy, a cleared field) the cap is the plain maximum.
  assert.deepEqual(boundOf(dictionary, draftOfPolicy(dictionary, "idle"), "idle_sec"), { minimum: 15, maximum: 604800 });
  assert.deepEqual(draftErrors(dictionary, { ...draftOfPolicy(dictionary, "idle"), idle_sec: 604800 }), []);
  assert.deepEqual(boundOf(dictionary, { ...timeslice, max_serve_sec: null }, "idle_sec"), { minimum: 15, maximum: 604800 });
  // Other bounds, other errors.
  const other = structuredClone(dictionary);
  other.bounds.hold_max_sec = { minimum: 20, maximum: 40 };
  assert.deepEqual(draftErrors(other, { ...draftOfPolicy(other, "idle"), hold_max_sec: 41 }), [{ field: "hold_max_sec", reason: "out_of_range", minimum: 20, maximum: 40 }]);
  assert.equal(boundOf({ ...dictionary, bounds: {} }, timeslice, "hold_max_sec"), null);
});

test("a deployment holds a policy or, with none, never; only a sleepable single-node non-PD one can yield", () => {
  assert.deepEqual(savedPolicy({ id: 1 } as never), { policy: "never" });
  const held = { policy: "anytime", idle_sec: 30, sleep_level: "auto", unfinished: "abort", hold_max_sec: 3600 } as const;
  assert.deepEqual(savedPolicy({ id: 1, yield_policy: held } as never), held);
  assert.equal(canYield({ sleepable: true, nnodes: 1 } as never), true);
  assert.equal(canYield({ sleepable: true } as never), true);
  assert.equal(canYield({ sleepable: false, nnodes: 1 } as never), false);
  assert.equal(canYield({ sleepable: true, nnodes: 2 } as never), false);
  assert.equal(canYield({ sleepable: true, nnodes: 1, pd: { mode: "ep", roles: [] } } as never), false);
});

test("paramsOfValue reads a policy out of a form value or a stored draft and drops what it does not know", () => {
  assert.deepEqual(paramsOfValue({ policy: "idle", idle_sec: 60, sleep_level: "l1", hold_max_sec: 300 }), { policy: "idle", idle_sec: 60, sleep_level: "l1", hold_max_sec: 300 });
  assert.deepEqual(paramsOfValue({ policy: "timeslice", unfinished: "wait", unfinished_wait_sec: 5, max_serve_sec: 90, extra: 1 }), {
    policy: "timeslice", max_serve_sec: 90, unfinished: "wait", unfinished_wait_sec: 5,
  });
  // The old forms are not policies.
  assert.equal(paramsOfValue({ preset: "online" }), undefined);
  assert.equal(paramsOfValue({ policy: "resident" }), undefined);
  assert.equal(paramsOfValue(null), undefined);
  assert.equal(paramsOfValue("idle"), undefined);
  assert.deepEqual(paramsOfValue({ policy: "idle", sleep_level: "l9", unfinished: "wait_unbounded", idle_sec: "60" }), { policy: "idle" });
});
