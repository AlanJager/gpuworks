package controller

import (
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A PD unit an operator pinned to a card another deployment holds waits for
// it, and its refusal says which card and who holds it, not only how many
// free cards there are; the unit that pinned nothing takes the free card.
func TestAPinnedPDCardTakenNamesItsHolder(t *testing.T) {
	f := newPDFixture(t, "pinned-holder", twoPrefillOneDecode)
	n1 := f.nodes["n1"]
	other, err := createTestDeployment(t, f.st, &store.Deployment{Name: "chat-bot", ModelID: f.dep.ModelID, NodeID: n1.ID, ServedName: "chat-bot", Engine: "vllm", State: "active", Replicas: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.CreateInstance(&store.Instance{UUID: "chat-bot-1", DeploymentID: other.ID, NodeID: n1.ID, State: proto.InstRunning, GPUIndexes: "[0]"}); err != nil {
		t.Fatal(err)
	}
	insts := f.reconcile(t)
	for _, inst := range insts {
		if inst.Role == "prefill" && strings.Contains(inst.GPUIndexes, "0") {
			t.Fatalf("the pinned prefill started on the taken card: %+v", inst)
		}
	}
	events := eventCodes(t, f.st, f.dep.ID, store.EventDeploymentUnschedulable)
	var found bool
	for _, event := range events {
		reason, _ := event["scheduler"].(map[string]any)
		if reason["code"] != scheduler.ReasonPinnedGPUsUnavailable {
			continue
		}
		params, _ := reason["params"].(map[string]any)
		if params["gpus"] != "0" || params["holders"] != "chat-bot" || params["node"] != n1.DisplayName() {
			t.Fatalf("params = %v", params)
		}
		found = true
	}
	if !found {
		t.Fatalf("events = %v, want pinned_gpus_unavailable", events)
	}
}
