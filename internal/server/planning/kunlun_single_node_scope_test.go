package planning

import (
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
	"testing"
)

func TestKunlunSingleNodePrefixReturnsDistributedRefusal(t *testing.T) {
	st, err := store.Open("file:kunlun-single-scope?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	for _, preview := range []bool{false, true} {
		req := kunlunReviewRequest(t)
		req.Preview = preview
		req.Topology.NNodes = 2
		req.Placement.TP = 2 // Two ranks must describe a valid global topology.
		_, reason, err := PlanTemplateLaunch(t.Context(), st, storeNetworks(st), req.Node, req, deploy.CatalogSelection{})
		want := "multi_node_unsupported_on_node"
		if preview {
			want = "accelerator_feature_unsupported"
		}
		if err != nil || reason == nil || reason.Code != want || reason.Params["feature"] != "multi_node" || reason.Params["vendor"] != "Kunlunxin" {
			t.Fatalf("must be a placement refusal: %+v %v", reason, err)
		}
	}
}
