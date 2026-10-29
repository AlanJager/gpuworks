import type {
  Deployment,
  YieldPolicy,
  YieldPolicyName,
  YieldPolicyRequest,
  YieldPresetName,
  YieldPreset,
  YieldSleepLevel,
  YieldUnfinished,
} from "../../api";

// The pure side of the yield policy editor: the presets table, which
// parameters apply to which policy, the preset a set of parameters amounts to,
// the ranges the server holds them to, and the request that saves them. The
// table and the applicability rules mirror the server's (internal/server/
// servicelevel); a preset is only a name for a set of parameters, so changing
// one makes the deployment custom and changing it back makes it the preset
// again.

// YieldParams is a policy with the parameters that apply to it, exactly as the
// server reports one (minus the derived preset): a parameter that does not
// apply is absent.
export type YieldParams = Omit<YieldPolicy, "preset">;

// YieldDraft is the editor's working copy: every parameter has a value (null
// while the field is cleared), whether it applies or not, so switching the
// policy and back does not lose what was typed.
export interface YieldDraft {
  policy: YieldPolicyName;
  auto_sleep: boolean;
  idle_sec: number | null;
  sleep_level: YieldSleepLevel;
  min_serve_sec: number | null;
  unfinished: YieldUnfinished;
  unfinished_wait_sec: number | null;
  hold_max_sec: number | null;
}

export type NumericField = "idle_sec" | "min_serve_sec" | "unfinished_wait_sec" | "hold_max_sec";

// The ranges, in seconds, the server accepts.
export const numericRanges: Record<NumericField, { minimum: number; maximum: number }> = {
  idle_sec: { minimum: 30, maximum: 604800 },
  min_serve_sec: { minimum: 0, maximum: 3600 },
  unfinished_wait_sec: { minimum: 1, maximum: 600 },
  hold_max_sec: { minimum: 10, maximum: 1800 },
};

export const yieldPresets: YieldPreset[] = ["resident", "online", "shared", "offline"];
export const yieldPolicyNames: YieldPolicyName[] = ["never", "idle", "timeslice", "anytime"];
export const sleepLevels: YieldSleepLevel[] = ["auto", "l1", "l2"];
export const unfinishedModes: YieldUnfinished[] = ["abort", "wait", "wait_unbounded"];

// The presets, as the server normalizes them.
export const presetParams: Record<YieldPreset, YieldParams> = {
  resident: { policy: "never", auto_sleep: false },
  online: { policy: "idle", auto_sleep: false, idle_sec: 300, sleep_level: "auto", hold_max_sec: 120 },
  shared: {
    policy: "timeslice",
    auto_sleep: false,
    idle_sec: 60,
    sleep_level: "auto",
    min_serve_sec: 120,
    unfinished: "wait",
    unfinished_wait_sec: 30,
    hold_max_sec: 600,
  },
  offline: { policy: "anytime", auto_sleep: false, sleep_level: "auto", unfinished: "abort", hold_max_sec: 1800 },
};

// What a parameter that a preset leaves out starts from when the operator
// turns it on.
const baseDraft: YieldDraft = {
  policy: "never",
  auto_sleep: false,
  idle_sec: 300,
  sleep_level: "auto",
  min_serve_sec: 120,
  unfinished: "wait",
  unfinished_wait_sec: 30,
  hold_max_sec: 120,
};

// The preset each policy's defaults are those of.
const policyDefaultsPreset: Record<YieldPolicyName, YieldPreset> = {
  never: "resident",
  idle: "online",
  timeslice: "shared",
  anytime: "offline",
};

// Which parameters apply, given the policy and whether the deployment sleeps
// on its own when idle.
export interface Applicable {
  idleSec: boolean;
  sleepLevel: boolean;
  minServeSec: boolean;
  unfinished: boolean;
  unfinishedWaitSec: boolean;
  holdMaxSec: boolean;
}

export function applicable(draft: Pick<YieldDraft, "policy" | "auto_sleep" | "unfinished">): Applicable {
  const { policy, auto_sleep: autoSleep } = draft;
  const yields = policy !== "never";
  const unfinished = policy === "timeslice" || policy === "anytime";
  return {
    idleSec: policy === "idle" || policy === "timeslice" || autoSleep,
    sleepLevel: yields || autoSleep,
    minServeSec: policy === "timeslice",
    unfinished,
    unfinishedWaitSec: unfinished && draft.unfinished === "wait",
    holdMaxSec: yields || autoSleep,
  };
}

// draftOfParams is the full working copy of a policy the server reported.
export function draftOfParams(params: YieldParams): YieldDraft {
  return {
    policy: params.policy,
    auto_sleep: params.auto_sleep,
    idle_sec: params.idle_sec ?? baseDraft.idle_sec,
    sleep_level: params.sleep_level ?? baseDraft.sleep_level,
    min_serve_sec: params.min_serve_sec ?? baseDraft.min_serve_sec,
    unfinished: params.unfinished ?? baseDraft.unfinished,
    unfinished_wait_sec: params.unfinished_wait_sec ?? baseDraft.unfinished_wait_sec,
    hold_max_sec: params.hold_max_sec ?? baseDraft.hold_max_sec,
  };
}

// draftOfPreset is what choosing a preset fills in: its full parameter set.
export function draftOfPreset(preset: YieldPreset): YieldDraft {
  return draftOfParams(presetParams[preset]);
}

// draftOfPolicy is what choosing a policy fills in: that policy's default
// parameters, keeping whether the deployment also sleeps when idle.
export function draftOfPolicy(policy: YieldPolicyName, autoSleep: boolean): YieldDraft {
  return { ...draftOfPreset(policyDefaultsPreset[policy]), auto_sleep: autoSleep };
}

// paramsOfDraft is the draft as the server would normalize it: only the
// parameters that apply, none of them null. A cleared field stays out, which
// no preset equals.
export function paramsOfDraft(draft: YieldDraft): YieldParams {
  const use = applicable(draft);
  const params: YieldParams = { policy: draft.policy, auto_sleep: draft.auto_sleep };
  const put = <K extends keyof YieldParams>(key: K, value: YieldParams[K] | null) => {
    if (value !== null && value !== undefined) params[key] = value;
  };
  if (use.idleSec) put("idle_sec", draft.idle_sec);
  if (use.sleepLevel) put("sleep_level", draft.sleep_level);
  if (use.minServeSec) put("min_serve_sec", draft.min_serve_sec);
  if (use.unfinished) put("unfinished", draft.unfinished);
  if (use.unfinishedWaitSec) put("unfinished_wait_sec", draft.unfinished_wait_sec);
  if (use.holdMaxSec) put("hold_max_sec", draft.hold_max_sec);
  return params;
}

function sameParams(left: YieldParams, right: YieldParams): boolean {
  const keys = new Set([...Object.keys(left), ...Object.keys(right)]) as Set<keyof YieldParams>;
  for (const key of keys) if (left[key] !== right[key]) return false;
  return true;
}

// presetOf names a set of parameters: the preset whose table row it equals,
// custom when it equals none. A cleared field that applies never equals a row.
export function presetOf(params: YieldParams): YieldPresetName {
  for (const [name, row] of Object.entries(presetParams) as Array<[YieldPreset, YieldParams]>) {
    if (sameParams(params, row)) return name;
  }
  return "custom";
}

export const presetOfDraft = (draft: YieldDraft): YieldPresetName => presetOf(paramsOfDraft(draft));

// draftEqual is whether two drafts amount to the same policy: a parameter that
// does not apply is no difference.
export const draftEqual = (left: YieldDraft, right: YieldDraft): boolean => sameParams(paramsOfDraft(left), paramsOfDraft(right));

// A field the server would refuse: out of its range, or cleared.
export interface YieldFieldError {
  field: NumericField;
  reason: "out_of_range" | "required";
  minimum: number;
  maximum: number;
}

// draftErrors lists the applicable numeric fields that would be refused.
export function draftErrors(draft: YieldDraft): YieldFieldError[] {
  const use = applicable(draft);
  const applies: Record<NumericField, boolean> = {
    idle_sec: use.idleSec,
    min_serve_sec: use.minServeSec,
    unfinished_wait_sec: use.unfinishedWaitSec,
    hold_max_sec: use.holdMaxSec,
  };
  const errors: YieldFieldError[] = [];
  for (const field of Object.keys(numericRanges) as NumericField[]) {
    if (!applies[field]) continue;
    const value = draft[field];
    const { minimum, maximum } = numericRanges[field];
    if (value === null || !Number.isFinite(value)) errors.push({ field, reason: "required", minimum, maximum });
    else if (!Number.isInteger(value) || value < minimum || value > maximum) errors.push({ field, reason: "out_of_range", minimum, maximum });
  }
  return errors;
}

// requestOfDraft is the body that saves a draft: the policy with the
// parameters that apply to it.
export function requestOfDraft(draft: YieldDraft): YieldPolicyRequest {
  return paramsOfDraft(draft);
}

// savedPolicy is the policy a deployment holds; one without any is resident.
export function savedPolicy(dep: Deployment): YieldParams {
  const policy = dep.yield_policy;
  if (!policy) return presetParams.resident;
  const params: Partial<YieldPolicy> = { ...policy };
  delete params.preset;
  return params as YieldParams;
}

// canYield is whether the deployment may use a policy other than never: it has
// to be able to sleep, and to be single-node and not PD (the server refuses the
// rest).
export function canYield(dep: Deployment): boolean {
  return dep.sleepable && !dep.pd && (dep.nnodes ?? 1) <= 1;
}

// canAutoSleep is whether the deployment may sleep by itself when idle: any
// deployment that can sleep, whatever its shape.
export function canAutoSleep(dep: Deployment): boolean {
  return dep.sleepable;
}
