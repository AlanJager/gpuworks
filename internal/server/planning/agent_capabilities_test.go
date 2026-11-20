package planning

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A launch that uses an agent-side facility goes only to an agent that
// announces it; an agent known to lack it refuses the node with a coded
// reason naming the capability, and an agent whose capabilities are unknown
// (not connected) refuses nothing.
func TestALaunchUsingAnAgentFacilityNeedsTheAgentsCapability(t *testing.T) {
	node := &store.Node{Name: "gb10-2", UUID: "node-2"}
	withDirs := deploy.ResolvedLaunch{Mounts: []proto.MountSpec{
		{Target: "/model", Source: "/models/x", ReadOnly: true},
		{Target: "/pico", Purpose: proto.MountPurposeManagedDir, ManagedDir: &proto.ManagedDirRef{Group: "7-1", Name: "pico-state"}},
	}}
	plain := deploy.ResolvedLaunch{Mounts: []proto.MountSpec{{Target: "/model", Source: "/models/x", ReadOnly: true}}}
	older := []string{proto.CapNetworkUpdate}

	refusal := agentCapabilityRefusal(older, node, withDirs)
	if refusal == nil || refusal.Code != scheduler.ReasonAgentCapabilityMissing {
		t.Fatalf("refusal = %+v, want %s", refusal, scheduler.ReasonAgentCapabilityMissing)
	}
	if refusal.Params["capability"] != proto.CapManagedDirs || refusal.Params["node"] != "gb10-2" || refusal.Params["node_uuid"] != "node-2" {
		t.Fatalf("params = %+v", refusal.Params)
	}
	for name, test := range map[string]struct {
		caps   []string
		launch deploy.ResolvedLaunch
	}{
		"current agent":           {append(older, proto.CapManagedDirs), withDirs},
		"agent not connected":     {nil, withDirs},
		"launch uses no facility": {older, plain},
	} {
		if refusal := agentCapabilityRefusal(test.caps, node, test.launch); refusal != nil {
			t.Fatalf("%s: refusal = %+v", name, refusal)
		}
	}
}
