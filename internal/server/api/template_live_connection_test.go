package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestTemplateHTTPDetectedCompatibilityUsesLiveConnections(t *testing.T) {
	live := false
	rig := newTokenRig(t, func(d *Deps) { d.Online = func(string) bool { return live } })
	t.Cleanup(func() { _ = rig.st.Close() })
	if err := rig.st.ReconcileInferenceCatalog([]store.InferenceModel{templateSupportFixture()}); err != nil {
		t.Fatal(err)
	}
	node, err := rig.st.RegisterNode("detected", store.HashToken("token"), "detected")
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.st.UpdateNodeStatus(node.UUID, proto.NodeStatus{GPUs: []proto.GPUInfo{{Index: 0, UUID: "amd-card", Vendor: "amd", Name: "Radeon 8060S", ArchID: "gfx1151", Healthy: true}}}); err != nil {
		t.Fatal(err)
	}
	for _, connected := range []bool{false, true} {
		live = connected
		stored := store.NodeStateOnline
		if connected {
			stored = store.NodeStateOffline
		}
		if err := rig.st.SetNodeState(node.UUID, stored); err != nil {
			t.Fatal(err)
		}
		for _, target := range []string{"", "&node_uuid=" + node.UUID} {
			response := rig.do(rig.session, http.MethodGet, "/api/v1/inference-templates/index?compatibility=detected&vendor=amd"+target, "")
			if response.Code != http.StatusOK {
				t.Fatalf("index=%d %s", response.Code, response.Body)
			}
			var body struct {
				Items []struct {
					Profiles []struct {
						Compatible *bool `json:"detected_compatible"`
					} `json:"profiles"`
				} `json:"items"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			matched := false
			for _, item := range body.Items {
				for _, profile := range item.Profiles {
					matched = matched || profile.Compatible != nil && *profile.Compatible
				}
			}
			if matched != connected {
				t.Fatalf("node filter=%q live=%v detected compatible=%v", target, connected, matched)
			}
		}
	}
}
