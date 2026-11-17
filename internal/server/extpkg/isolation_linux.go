package extpkg

import (
	"syscall"

	"gpuworks.ai/gpuworks/pkg/extapi"
)

// landlockCreateRuleset is the landlock_create_ruleset system call, the same
// number on every Linux architecture; with the version flag it returns the
// Landlock ABI the kernel supports.
const (
	landlockCreateRuleset  = 444
	landlockRulesetVersion = 1
)

// HostIsolation is the strongest isolation this host can apply to a plugin
// process: Landlock when the kernel offers it (ABI 1 or later, Linux 5.13),
// otherwise none.
func HostIsolation() string {
	abi, _, errno := syscall.Syscall(landlockCreateRuleset, 0, 0, landlockRulesetVersion)
	if errno != 0 || int(abi) < 1 {
		return extapi.IsolationNone
	}
	return extapi.IsolationLandlock
}
