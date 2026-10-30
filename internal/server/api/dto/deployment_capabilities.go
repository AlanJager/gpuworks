package dto

// Eligibility says whether a deployment may be offered something - a yield
// policy other than never, a sleep order - and, when not, why: Reason is a
// stable product code the client words in its own locale (SLEEP_NOT_ENABLED,
// YIELD_POLICY_SHAPE, ...). The server decides; a client renders.
type Eligibility struct {
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
}

// EligibilityOf is the eligibility a refusal code gives: allowed when the
// code is "".
func EligibilityOf(refusal string) Eligibility {
	return Eligibility{OK: refusal == "", Reason: refusal}
}

// ScheduleCapabilities is what a deployment's schedule may order
// (planning.ScheduleCapabilitiesOf): a sleep, a wake, and a replica count
// within bounds.
type ScheduleCapabilities struct {
	Sleep Eligibility `json:"sleep"`
	Wake  Eligibility `json:"wake"`
	// Replicas has one entry with no role for any deployment but PD, and one
	// per role of its mode, in the mode's order, for a PD deployment.
	Replicas []ReplicaCapability `json:"replicas"`
}

// ReplicaCapability is the replica count a schedule may set for the
// deployment, or for one PD role of it: Minimum to Maximum, when OK.
type ReplicaCapability struct {
	Role    string `json:"role,omitempty"`
	Minimum int    `json:"minimum"`
	Maximum int    `json:"maximum"`
	OK      bool   `json:"ok"`
	Reason  string `json:"reason,omitempty"`
}
