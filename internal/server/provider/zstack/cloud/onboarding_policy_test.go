package cloud

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/inventory"
)

func TestEvaluateHostOnboardingSeparatesHostEligibilityFromGPUAllocation(t *testing.T) {
	tests := []struct {
		name     string
		resource inventory.CloudResource
		want     hostOnboardingDecision
	}{
		{
			name:     "no GPU still admits host",
			resource: inventory.CloudResource{Type: "host", Hypervisor: "KVM", ManagementAddress: "192.0.2.10", Status: "Connected"},
			want:     hostOnboardingCandidate,
		},
		{
			name: "used gpu still admits host",
			resource: inventory.CloudResource{
				Type: "host", Hypervisor: "KVM", ManagementAddress: "192.0.2.10", Status: "Connected",
				GPUs: []inventory.CloudGPU{{ExternalID: "gpu-used", State: "Enabled", Allocation: "Allocated"}},
			},
			want: hostOnboardingCandidate,
		},
		{
			name: "invalid host is rejected",
			resource: inventory.CloudResource{
				Type: "host", Hypervisor: "KVM", ManagementAddress: "", Status: "Connected",
				GPUs: []inventory.CloudGPU{{ExternalID: "gpu-free", State: "Enabled", Allocation: "Unallocated"}},
			},
			want: hostOnboardingReject,
		},
		{
			name:     "disabled and allocated GPU",
			resource: inventory.CloudResource{Type: "host", Hypervisor: "KVM", ManagementAddress: "192.0.2.10", Status: "Connected", GPUs: []inventory.CloudGPU{{State: "Disabled", Allocation: "Allocated"}}},
			want:     hostOnboardingCandidate,
		},
		{
			name:     "no GPU and no address",
			resource: inventory.CloudResource{Type: "host", Hypervisor: "KVM", Status: "Connected"},
			want:     hostOnboardingReject,
		},
		{
			name:     "no GPU and disconnected",
			resource: inventory.CloudResource{Type: "host", Hypervisor: "KVM", ManagementAddress: "192.0.2.10", Status: "Disconnected"},
			want:     hostOnboardingReject,
		},
		{
			name:     "no GPU and non KVM",
			resource: inventory.CloudResource{Type: "host", Hypervisor: "ESX", ManagementAddress: "192.0.2.10", Status: "Connected"},
			want:     hostOnboardingReject,
		},
		{
			name:     "not a host",
			resource: inventory.CloudResource{Type: "vm", Hypervisor: "KVM", ManagementAddress: "192.0.2.10", Status: "Connected"},
			want:     hostOnboardingReject,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := evaluateHostOnboarding(tt.resource); got != tt.want {
				t.Fatalf("decision = %q, want %q", got, tt.want)
			}
		})
	}
}
