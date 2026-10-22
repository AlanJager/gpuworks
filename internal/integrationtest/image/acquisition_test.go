package image

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

func TestEvaluateImageCacheThroughAgent(t *testing.T) {
	env := testenv.New(t)
	node := env.AddNode("cache", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 40960}))
	env.WaitNodeOnline(t, "cache")
	model, err := env.Store().UpsertModel(&store.Model{Source: "huggingface", SourceID: "Acme/Cache", Name: "cache", WeightBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	api := env.API(t)
	body := []byte(fmt.Sprintf(`{"model_id":%d,"node_uuid":%q,"engine":"vllm","image":"uncatalogued/image:v1","quick_params":{},"placement":{}}`, model.ID, node.UUID))
	for _, state := range []string{"absent", "present", "failed"} {
		t.Run(state, func(t *testing.T) {
			if state == "present" {
				if err := node.Driver.PullImage(context.Background(), "uncatalogued/image:v1", nil); err != nil {
					t.Fatal(err)
				}
			}
			if state == "failed" {
				node.Driver.FailNext("ImageExists", errors.New("runtime unavailable"))
			}
			resp, err := api.PostJSON("/api/v1/evaluate", body)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			var wire struct {
				Acquisition struct {
					Image map[string]any `json:"image"`
				} `json:"acquisition"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&wire); err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d", resp.StatusCode)
			}
			image := wire.Acquisition.Image
			cached, ok := image["cached"]
			if state == "failed" {
				if ok || image["cache_reason"] != "query_failed" {
					t.Fatalf("image=%v", image)
				}
			} else if !ok || cached != (state == "present") {
				t.Fatalf("image=%v", image)
			}
		})
	}
}
