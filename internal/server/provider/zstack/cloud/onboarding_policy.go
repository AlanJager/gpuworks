package cloud

import (
	"strings"

	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/inventory"
)

type hostOnboardingDecision string

const (
	hostOnboardingCandidate hostOnboardingDecision = "candidate"
	hostOnboardingReject    hostOnboardingDecision = "reject"
)

// evaluateHostOnboarding decides whether a discovered Cloud host should enter
// Agent onboarding. GPU presence and allocation do not determine whether
// the agent can be installed.
func evaluateHostOnboarding(resource inventory.CloudResource) hostOnboardingDecision {
	if resource.Type != "host" ||
		!strings.EqualFold(resource.Hypervisor, "KVM") ||
		strings.TrimSpace(resource.ManagementAddress) == "" ||
		!strings.EqualFold(resource.Status, "Connected") {
		return hostOnboardingReject
	}
	return hostOnboardingCandidate
}
