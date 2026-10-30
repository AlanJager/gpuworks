import type {
  Deployment,
  YieldBound,
  YieldParamName,
  YieldPolicy,
  YieldPolicyDictionary,
  YieldPolicyEntry,
  YieldPolicyName,
  YieldPolicyRequest,
  YieldSleepLevel,
  YieldUnfinished,
} from "../../api";

// The pure side of the yield policy editor: the working copy of a policy, which
// parameters apply to the chosen one, what differs from its defaults, the
// bounds the server holds the numbers to, and the request that saves them.
// Defaults, applicability (with the conditions under which a parameter
// applies), the parameter names and the bounds are the server's own, from its
// dictionary (GET /scheduling/yield-policies); no rule is kept here.

export type YieldParams = YieldPolicy;

// YieldDraft is the editor's working copy: the parameters of the chosen policy,
// with null for a number field that was cleared.
export interface YieldDraft {
  policy: YieldPolicyName;
  idle_sec?: number | null;
  sleep_level?: YieldSleepLevel;
  max_serve_sec?: number | null;
  unfinished?: YieldUnfinished;
  unfinished_wait_sec?: number | null;
  hold_max_sec?: number | null;
}

export type NumericField = "idle_sec" | "max_serve_sec" | "unfinished_wait_sec" | "hold_max_sec";

// isNumericField is whether the dictionary bounds a parameter: the numeric
// ones are those with a range.
const isNumericField = (dictionary: YieldPolicyDictionary, name: YieldParamName): name is NumericField => dictionary.bounds[name] !== undefined;

export const entryOf = (dictionary: YieldPolicyDictionary, policy: YieldPolicyName): YieldPolicyEntry | undefined =>
  dictionary.policies.find((entry) => entry.policy === policy);

// draftOfParams is the working copy of a policy as the server reports it.
export function draftOfParams(params: YieldParams): YieldDraft {
  return { ...params };
}

// draftOfPolicy is what choosing a policy fills in: its default parameters.
export function draftOfPolicy(dictionary: YieldPolicyDictionary, policy: YieldPolicyName): YieldDraft {
  return draftOfParams(entryOf(dictionary, policy)?.defaults ?? { policy });
}

// Which parameters apply to the draft's policy, in the dictionary's order: the
// policy's applies, less a parameter whose applies_when condition the draft
// does not meet (unfinished_wait_sec while unfinished is not "wait").
export function applicable(dictionary: YieldPolicyDictionary, draft: YieldDraft): Set<YieldParamName> {
  const applies = new Set(entryOf(dictionary, draft.policy)?.applies ?? []);
  const held = draft as unknown as Record<string, unknown>;
  for (const name of [...applies]) {
    for (const [selector, values] of Object.entries(dictionary.applies_when?.[name] ?? {})) {
      if (!(values ?? []).includes(held[selector] as string)) applies.delete(name);
    }
  }
  return applies;
}

// paramsOfDraft is the draft as the server would hold it: only the parameters
// that apply, none of them null. A cleared field stays out.
export function paramsOfDraft(dictionary: YieldPolicyDictionary, draft: YieldDraft): YieldParams {
  const applies = applicable(dictionary, draft);
  const params: Record<string, unknown> = { policy: draft.policy };
  for (const name of applies) {
    const value = draft[name];
    if (value !== null && value !== undefined) params[name] = value;
  }
  return params as unknown as YieldParams;
}

// normalized strips from a saved policy what does not apply, so it can be
// compared with a draft.
export function normalized(dictionary: YieldPolicyDictionary, params: YieldParams): YieldParams {
  return paramsOfDraft(dictionary, draftOfParams(params));
}

function sameParams(left: YieldParams, right: YieldParams): boolean {
  const keys = new Set([...Object.keys(left), ...Object.keys(right)]) as Set<keyof YieldParams>;
  for (const key of keys) if (left[key] !== right[key]) return false;
  return true;
}

// draftEqual is whether a draft amounts to a saved policy: a parameter that
// does not apply is no difference.
export const draftEqual = (dictionary: YieldPolicyDictionary, draft: YieldDraft, saved: YieldParams): boolean =>
  sameParams(paramsOfDraft(dictionary, draft), normalized(dictionary, saved));

// modifiedFields lists the applicable parameters whose value differs from the
// chosen policy's default.
export function modifiedFields(dictionary: YieldPolicyDictionary, draft: YieldDraft): Set<YieldParamName> {
  const defaults = entryOf(dictionary, draft.policy)?.defaults;
  const modified = new Set<YieldParamName>();
  if (!defaults) return modified;
  for (const name of applicable(dictionary, draft)) if ((draft[name] ?? null) !== (defaults[name] ?? null)) modified.add(name);
  return modified;
}

// chooseUnfinished sets how unfinished requests are handled; a parameter the
// choice makes apply (the wait time, on turning to "wait") takes the policy's
// default when the draft holds none.
export function chooseUnfinished(dictionary: YieldPolicyDictionary, draft: YieldDraft, unfinished: YieldUnfinished): YieldDraft {
  const before = applicable(dictionary, draft);
  const next = { ...draft, unfinished };
  const defaults = (entryOf(dictionary, draft.policy)?.defaults ?? {}) as Record<string, unknown>;
  const filled = next as unknown as Record<string, unknown>;
  for (const name of applicable(dictionary, next)) {
    if (!before.has(name) && (filled[name] ?? null) === null) filled[name] = defaults[name] ?? null;
  }
  return next;
}

// A field the server would refuse: out of its range (or above the parameter it
// may not exceed), or cleared. maximum is the largest value it takes now.
export interface YieldFieldError {
  field: NumericField;
  reason: "out_of_range" | "required";
  minimum: number;
  maximum: number;
}

// boundOf is the range of a field and, when the field is capped by another
// parameter that applies and has a value, the tighter maximum.
export function boundOf(dictionary: YieldPolicyDictionary, draft: YieldDraft, field: NumericField): { minimum: number; maximum: number } | null {
  const bound: YieldBound | undefined = dictionary.bounds[field];
  if (!bound) return null;
  let maximum = bound.maximum;
  const cap = bound.at_most;
  if (cap && applicable(dictionary, draft).has(cap)) {
    const limit = draft[cap];
    if (typeof limit === "number" && Number.isFinite(limit)) maximum = Math.min(maximum, limit);
  }
  return { minimum: bound.minimum, maximum };
}

// draftErrors lists the applicable numeric fields that would be refused.
export function draftErrors(dictionary: YieldPolicyDictionary, draft: YieldDraft): YieldFieldError[] {
  const applies = applicable(dictionary, draft);
  const errors: YieldFieldError[] = [];
  for (const field of applies) {
    if (!isNumericField(dictionary, field)) continue;
    const bound = boundOf(dictionary, draft, field) ?? { minimum: 0, maximum: Number.MAX_SAFE_INTEGER };
    const value = draft[field];
    if (value === null || value === undefined || !Number.isFinite(value)) errors.push({ field, reason: "required", ...bound });
    else if (!Number.isInteger(value) || value < bound.minimum || value > bound.maximum) errors.push({ field, reason: "out_of_range", ...bound });
  }
  return errors;
}

// requestOfDraft is the body that saves a draft: the policy with the
// parameters that apply to it.
export function requestOfDraft(dictionary: YieldPolicyDictionary, draft: YieldDraft): YieldPolicyRequest {
  return paramsOfDraft(dictionary, draft);
}

// savedPolicy is the policy a deployment holds; one without any is never.
export function savedPolicy(dep: Deployment): YieldParams {
  return dep.yield_policy ?? { policy: "never" };
}

// The names a policy value can hold, for paramsOfValue: it reads values (a
// stored deploy draft, a form field) where no dictionary is at hand, so these
// are the wire contract's enumerations (api.ts); the editor itself takes every
// list from the dictionary.
const policyNames: YieldPolicyName[] = ["never", "idle", "timeslice", "anytime"];
const levels: YieldSleepLevel[] = ["auto", "l1", "l2"];
const unfinishedModes: YieldUnfinished[] = ["abort", "wait"];

// paramsOfValue reads a policy out of a value the form or a stored draft holds,
// keeping the parameters of known names and kinds; undefined for anything that
// is not a policy.
export function paramsOfValue(value: unknown): YieldParams | undefined {
  if (typeof value !== "object" || value === null) return undefined;
  const raw = value as Record<string, unknown>;
  if (!policyNames.includes(raw.policy as YieldPolicyName)) return undefined;
  const params: Record<string, unknown> = { policy: raw.policy };
  for (const name of ["idle_sec", "max_serve_sec", "unfinished_wait_sec", "hold_max_sec"] as const) {
    if (typeof raw[name] === "number") params[name] = raw[name];
  }
  if (levels.includes(raw.sleep_level as YieldSleepLevel)) params.sleep_level = raw.sleep_level;
  if (unfinishedModes.includes(raw.unfinished as YieldUnfinished)) params.unfinished = raw.unfinished;
  return params as unknown as YieldParams;
}
