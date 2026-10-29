package deploy

import (
	"errors"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"testing"
)

func TestKunlunSingleNodePrefixRejectsDistributedLaunch(t *testing.T) {
	engine, err := SelectEngine("sglang")
	if err != nil {
		t.Fatal(err)
	}
	for _, nodes := range []int{2, 3} {
		_, err := ResolveLaunch(ResolveLaunchInput{Engine: engine, Params: Params{Vendor: "kunlunxin", ModelPath: "/model", NNodes: nodes, AssignedGPUs: 1, TensorParallel: nodes, MasterAddr: "192.0.2.1", MasterPort: 29500}})
		var product *messages.ProductError
		if !errors.As(err, &product) || product.Code() != messages.ErrorAcceleratorFeatureUnsupported {
			t.Fatalf("distributed launch accepted: %v", err)
		}
	}
}
