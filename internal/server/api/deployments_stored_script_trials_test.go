package api

import (
	"net/http"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// A deployment stored with two cards and a script that states one (GWKS-415,
// written before scripts were held to the assigned GPUs) is trialled as it
// launches: naming a node for a new replica answers about the node, not with
// an internal error about the script.
func TestScaleReplicasTrialsAStoredMismatchedScriptAsItLaunches(t *testing.T) {
	h, st, modelID := twoCardFixture(t)
	clone := oneCardManualClone(t, h, modelID, "sglang", sglangOneCardScript)
	read, err := st.GetDeployment(clone.ID)
	if err != nil {
		t.Fatal(err)
	}
	reqs, err := st.DeploymentRequirements(clone.ID)
	if err != nil {
		t.Fatal(err)
	}
	stale := *read
	stale.Placement = `{"gpus":[0,1],"tp":2}`
	if err := st.EditDeployment(read, reqs, store.DeploymentEdit{Next: &stale, Apply: store.EditApplyNone}); err != nil {
		t.Fatal(err)
	}
	rec := pdOpsRequest(h.scaleReplicas, http.MethodPut, clone.ID, `{"replicas":2,"place_on":["two-card"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("scale = %d %s", rec.Code, rec.Body)
	}
	if row, _ := st.GetDeployment(clone.ID); row.Replicas != 2 {
		t.Fatalf("replicas = %d, want 2", row.Replicas)
	}
}

// The same holds for the trial a unit suggestion plans on each rank of a
// multi-node deployment.
func TestSuggestUnitTrialsAStoredMismatchedScriptAsItLaunches(t *testing.T) {
	h, st, dep := suggestFixture(t)
	if _, err := st.SetNodeScheduling("s-4", store.NodeSchedulable); err != nil {
		t.Fatal(err)
	}
	overrides := `{"command_mode":"manual","cmd_display":"exec vllm serve /model --tensor-parallel-size 1 --port 8000"}`
	if err := st.DB().Model(&store.Deployment{}).Where("id = ?", dep.ID).Update("container_overrides", overrides).Error; err != nil {
		t.Fatal(err)
	}
	status, body, got := postSuggest(t, h, dep.ID, listedReplica)
	if status != http.StatusOK || got.Unit == nil {
		t.Fatalf("suggest = %d %s", status, body)
	}
	if want := `"unit":{"node_uuid":"s-3","followers":[{"node_uuid":"s-4"}]}`; !strings.Contains(body, want) {
		t.Fatalf("suggestion lacks %s\n%s", want, body)
	}
}
