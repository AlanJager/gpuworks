// Client for a deployment's yield policy (PUT /api/v1/deployments/{id}/yield-policy)
// and the policy dictionary (GET /api/v1/scheduling/yield-policies),
// docs/explanation/service-levels.md. The wire types mirror
// internal/server/api/dto/yield_policy.go.
import { getJSON, sendJSON } from "./api";

// YieldPolicyName is how a deployment shares its GPUs, in rank order (highest
// first): never gives its GPUs to another deployment, idle gives them up when
// another waits and it has been idle, timeslice also takes turns with other
// timeslice deployments and gives way at once to a higher-ranked waiter,
// anytime gives them to any higher-ranked waiter at once.
export type YieldPolicyName = "never" | "idle" | "timeslice" | "anytime";
export type YieldSleepLevel = "auto" | "l1" | "l2";
export type YieldUnfinished = "abort" | "wait";

// YieldPolicy is a policy with the parameters that apply to it, as the server
// holds it and as PUT /deployments/{id}/yield-policy and a create request's
// yield_policy take it: a parameter that does not apply is absent.
export interface YieldPolicy {
  policy: YieldPolicyName;
  idle_sec?: number;
  sleep_level?: YieldSleepLevel;
  max_serve_sec?: number;
  unfinished?: YieldUnfinished;
  unfinished_wait_sec?: number;
  hold_max_sec?: number;
}

export type YieldPolicyRequest = YieldPolicy;

// YieldPolicyAnswer is what the PUT answers: the policy as stored, and the
// other idle-policy deployments on the same cards (the server allows this, the
// operator should know). Only the PUT answer carries them.
export interface YieldPolicyAnswer extends YieldPolicy {
  idle_neighbours?: string[];
}

export type YieldParamName = "idle_sec" | "sleep_level" | "max_serve_sec" | "unfinished" | "unfinished_wait_sec" | "hold_max_sec";

// One policy of the dictionary (GET /scheduling/yield-policies): its default
// parameters and which of them apply to it.
export interface YieldPolicyEntry {
  policy: YieldPolicyName;
  defaults: YieldPolicy;
  applies: YieldParamName[];
}

// The range of a numeric parameter in seconds. at_most names the parameter it
// may not exceed when both apply.
export interface YieldBound {
  minimum: number;
  maximum: number;
  at_most?: YieldParamName;
}

// The yield policy dictionary: the policies in rank order with their defaults,
// the conditions under which a parameter applies, the bounds of the numeric
// parameters and the values of the choices. applies_when narrows a policy's
// applies: a parameter it names applies only while each parameter it lists
// holds one of the given values (unfinished_wait_sec while unfinished is
// "wait").
export interface YieldPolicyDictionary {
  policies: YieldPolicyEntry[];
  applies_when: Partial<Record<YieldParamName, Partial<Record<YieldParamName, string[]>>>>;
  bounds: Partial<Record<YieldParamName, YieldBound>>;
  sleep_levels: YieldSleepLevel[];
  unfinished: YieldUnfinished[];
}

export const yieldPolicyApi = {
  set: (id: number, body: YieldPolicyRequest) =>
    sendJSON<YieldPolicyAnswer>("PUT", `/api/v1/deployments/${id}/yield-policy`, body),
  dictionary: () => getJSON<YieldPolicyDictionary>("/api/v1/scheduling/yield-policies"),
};
