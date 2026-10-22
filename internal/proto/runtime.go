package proto

// Runtime preferences are persisted by the server and replayed to agents.
// RuntimeDriver in SysInfo remains the observed runtime; these values describe
// the user's selection policy.
const (
	RuntimePreferenceAuto    = "auto"
	RuntimePreferenceDocker  = "docker"
	RuntimePreferenceNerdctl = "nerdctl"
	RuntimePreferenceUdocker = "udocker"
)

func ValidRuntimePreference(value string) bool {
	switch value {
	case RuntimePreferenceAuto, RuntimePreferenceDocker, RuntimePreferenceNerdctl, RuntimePreferenceUdocker:
		return true
	default:
		return false
	}
}

// RuntimeDriverHost is the runtime a Mac node reports: engines run as host
// processes from environment packages, in the node's own network.
const RuntimeDriverHost = "host"
