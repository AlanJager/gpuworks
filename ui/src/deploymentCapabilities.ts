// What the server lets a client offer a deployment (a deployment's
// yield_policy_eligible and schedule_capabilities, and the evaluate answer's
// yield_policy_eligible): the pages render these decisions and do not
// re-derive them.

// Eligibility is whether the server lets a client offer something; when not,
// reason is the stable product code (an apiError.* key) that says why.
export interface Eligibility {
  ok: boolean;
  reason?: string;
}

// ScheduleCapabilities is what a deployment's schedule may order: sleep, wake,
// and a replica count - one entry with no role, or one per PD role in the
// mode's order - between minimum and maximum.
export interface ScheduleCapabilities {
  sleep: Eligibility;
  wake: Eligibility;
  replicas: ReplicaCapability[];
}

export interface ReplicaCapability extends Eligibility {
  role?: string;
  minimum: number;
  maximum: number;
}
