package messages

// Product error codes of yield policies: how a deployment shares GPUs with
// others (internal/server/yieldpolicy).
const (
	// ErrorYieldPolicyInvalid: a field of the yield policy is none of the
	// allowed values, out of its range, or missing ({field}, {reason}:
	// "invalid_value", "out_of_range" with {minimum} and {maximum}, or
	// "required").
	ErrorYieldPolicyInvalid = "YIELD_POLICY_INVALID"
	// ErrorYieldPolicyShape: a policy other than never is for a single-node
	// deployment; a multi-node or PD one stays never.
	ErrorYieldPolicyShape = "YIELD_POLICY_SHAPE"
	// ErrorYieldPolicyNeedsSleep: an edit turns sleep off while the
	// deployment's yield policy {policy} gives way; giving way is sleeping,
	// so the policy goes back to never first.
	ErrorYieldPolicyNeedsSleep = "YIELD_POLICY_NEEDS_SLEEP"
)
