package messages

const (
	// ErrorSleepNotEnabled: a sleep order to a deployment that was not
	// created sleepable.
	ErrorSleepNotEnabled = "SLEEP_NOT_ENABLED"
	// ErrorSleepHostMemoryInsufficient: the node's host memory cannot hold the
	// weights an L1 sleep moves there.
	ErrorSleepHostMemoryInsufficient = "SLEEP_HOST_MEMORY_INSUFFICIENT"
	// ErrorSleepHostBackingInsufficient: a sleepable deployment on {vendor}
	// keeps host memory behind the GPU memory its engine takes, and {node}
	// has {available_gib} GiB available where {need_gib} GiB is needed.
	ErrorSleepHostBackingInsufficient = "SLEEP_HOST_BACKING_INSUFFICIENT"
)

// SleepRefusalFallback is the English wire text for a refused sleep order or
// sleep declaration; the UI renders the code.
func SleepRefusalFallback(code string) string {
	switch code {
	case ErrorSleepNotEnabled:
		return "sleep mode was not enabled for this deployment"
	case ErrorSleepEngineUnsupported:
		return "the deployment's engine does not support sleep mode"
	case ErrorInstanceNotServing:
		return "the instance fills no replica: it failed, is draining or is being removed"
	case ErrorSleepRoleCannotEmpty:
		return "the role cannot be left without an awake instance; sleep the whole deployment instead"
	case ErrorSleepHostMemoryInsufficient:
		return "the node's host memory cannot hold the weights an L1 sleep moves there; sleep at L2 instead"
	case ErrorSleepHostBackingInsufficient:
		return "the node's host memory cannot back the GPU memory a sleepable engine takes on this accelerator; lower gpu_memory_utilization or turn sleep off"
	case ErrorPDNotPD:
		return "deployment is not PD disaggregated"
	case ErrorPDRoleUnknown:
		return "the role is not a role of this deployment"
	case ErrorAcceleratorFeatureUnsupported:
		return "the selected accelerator does not support sleep mode"
	}
	return "sleep order refused"
}
