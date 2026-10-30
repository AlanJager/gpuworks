// What the server says a deployment may be offered, literally as GET
// /deployments/{id} writes it for each shape (internal/server/api's
// deployment_capabilities_test.go asserts the same JSON). Fixtures spread the
// shape they stand for; no test derives these from sleepable, nnodes or pd,
// since the pages must not either.

const sleepOK = { ok: true };
const notSleepable = { ok: false, reason: "SLEEP_NOT_ENABLED" };
const shape = { ok: false, reason: "YIELD_POLICY_SHAPE" };
const manual = { ok: false, reason: "SCHEDULE_REPLICAS_MANUAL_PLACEMENT" };

export const wakeWaitLimit = { wake_wait_limit_sec: 1800 };

// A sleepable single-node deployment: everything is offered.
export const sleepableSingleNode = {
  ...wakeWaitLimit,
  yield_policy_eligible: { ok: true },
  schedule_capabilities: { sleep: sleepOK, wake: sleepOK, replicas: [{ minimum: 1, maximum: 32, ok: true }] },
};

// A single-node deployment that cannot sleep: only a replica count.
export const notSleepableSingleNode = {
  ...wakeWaitLimit,
  yield_policy_eligible: notSleepable,
  schedule_capabilities: { sleep: notSleepable, wake: notSleepable, replicas: [{ minimum: 1, maximum: 32, ok: true }] },
};

// A sleepable multi-node deployment whose replicas the operator placed.
export const sleepableMultiNodeManual = {
  ...wakeWaitLimit,
  yield_policy_eligible: shape,
  schedule_capabilities: { sleep: sleepOK, wake: sleepOK, replicas: [{ minimum: 1, maximum: 32, ok: false, ...manual }] },
};

// A sleepable multi-node deployment the platform places.
export const sleepableMultiNodeAuto = {
  ...wakeWaitLimit,
  yield_policy_eligible: shape,
  schedule_capabilities: { sleep: sleepOK, wake: sleepOK, replicas: [{ minimum: 1, maximum: 32, ok: true }] },
};

// A multi-node deployment placed by hand that cannot sleep: nothing to order.
export const notSleepableMultiNodeManual = {
  ...wakeWaitLimit,
  yield_policy_eligible: notSleepable,
  schedule_capabilities: { sleep: notSleepable, wake: notSleepable, replicas: [{ minimum: 1, maximum: 32, ok: false, ...manual }] },
};

// PD (vllm-nixl: decode, then prefill, which may be empty).
const pdReplicas = (refusal: Record<string, unknown>) => [
  { role: "decode", minimum: 1, maximum: 32, ...refusal },
  { role: "prefill", minimum: 0, maximum: 32, ...refusal },
];

export const sleepablePDAuto = {
  ...wakeWaitLimit,
  yield_policy_eligible: shape,
  schedule_capabilities: { sleep: sleepOK, wake: sleepOK, replicas: pdReplicas({ ok: true }) },
};

export const sleepablePDManual = {
  ...wakeWaitLimit,
  yield_policy_eligible: shape,
  schedule_capabilities: { sleep: sleepOK, wake: sleepOK, replicas: pdReplicas({ ok: false, ...manual }) },
};

export const notSleepablePDAuto = {
  ...wakeWaitLimit,
  yield_policy_eligible: notSleepable,
  schedule_capabilities: { sleep: notSleepable, wake: notSleepable, replicas: pdReplicas({ ok: true }) },
};

export const notSleepablePDManual = {
  ...wakeWaitLimit,
  yield_policy_eligible: notSleepable,
  schedule_capabilities: { sleep: notSleepable, wake: notSleepable, replicas: pdReplicas({ ok: false, ...manual }) },
};
