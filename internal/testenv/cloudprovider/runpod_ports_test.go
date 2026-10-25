package cloudprovider

import (
	"testing"

	serverprovider "gpuworks.ai/gpuworks/internal/server/provider"
)

// A server-dialed agent needs a port next to SSH from the moment the pod is
// created: RunPod opens one later only by editing the pod, which restarts it
// and the install refuses without consent (INSTANCE_RESTART_REQUIRED).
func TestRunpodRentExposesTheAgentPortOnlyWhenServerDialed(t *testing.T) {
	for _, tc := range []struct {
		name       string
		spec       GPUSpec
		wantPorts  string
		wantPinned bool
	}{
		{name: "ssh only by default", spec: GPUSpec{GPUModel: "RTX4090", GPUCount: 1}, wantPorts: "22/tcp", wantPinned: true},
		{name: "driver default ports for a server-dialed agent", spec: GPUSpec{GPUModel: "RTX4090", GPUCount: 1, ServerDialPort: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			driver := &fakeProviderDriver{result: serverprovider.Instance{
				ExternalID: "pod-1", Kind: "container", PublicIP: "203.0.113.4", SSHPort: 2244, SSHUser: "root",
			}}
			p, err := newRunpod(driver, serverprovider.Credentials{APIKey: "key"}, "run-ports")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := p.Rent(t.Context(), tc.spec); err != nil {
				t.Fatal(err)
			}
			got, pinned := driver.created.Extra["ports"]
			if pinned != tc.wantPinned || got != tc.wantPorts {
				t.Fatalf("create ports = %q (set=%v), want %q (set=%v); extra=%#v", got, pinned, tc.wantPorts, tc.wantPinned, driver.created.Extra)
			}
		})
	}
}
