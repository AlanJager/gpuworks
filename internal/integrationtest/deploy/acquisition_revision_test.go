package deploy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

func TestEmptyRevisionEvaluationReusesDeployedPinnedArtifact(t *testing.T) {
	env := testenv.New(t)
	node := env.AddNode("revision", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 40960}))
	env.WaitNodeOnline(t, node.Name)
	n, err := env.Store().NodeByUUID(node.UUID)
	if err != nil {
		t.Fatal(err)
	}
	model, err := env.Store().UpsertModel(&store.Model{Source: "huggingface", SourceID: "Acme/Pinned", Revision: "commit", Name: "pinned", Arch: "Qwen3ForCausalLM", WeightBytes: 1 << 20, Meta: `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":64,"num_attention_heads":2,"num_key_value_heads":1,"max_position_embeddings":4096}`})
	if err != nil {
		t.Fatal(err)
	}
	pinnedDir := testenv.ModelDir(t, "pinned")
	if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: n.ID, State: proto.MFPresent, Path: pinnedDir}); err != nil {
		t.Fatal(err)
	}
	catalog := store.InferenceModel{ModelID: "acme/pinned", DisplayTitle: "Pinned", Source: "upstream", Architecture: "Qwen3ForCausalLM", Specs: []store.InferenceSpecRow{{Name: "pinned", Args: "[]", Env: "{}", Engine: "vllm", Image: "gpuworks/fake:v1", WeightBytes: 1 << 20, Quantization: "bf16", GPUs: `{"vendor":"nvidia"}`, WeightSources: `[{"provider":"huggingface","repo_id":"Acme/Pinned","revision":"commit"}]`}}}
	if err := env.Store().ReconcileInferenceCatalog([]store.InferenceModel{catalog}); err != nil {
		t.Fatal(err)
	}
	template, err := env.Store().InferenceModelByModelID(catalog.ModelID)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(fmt.Sprintf(`{"template_id":%d,"template_spec_source":"upstream","template_spec_name":"pinned","node_uuid":%q,"served_name":"pinned","quick_params":{},"placement":{}}`, template.ID, node.UUID))
	api := env.API(t)
	resp, err := api.PostJSON("/api/v1/deployments", body)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusAccepted {
		t.Fatalf("deploy=%d %s", resp.StatusCode, raw)
	}
	env.Eventually(t, 20*time.Second, func() bool {
		instances, _ := env.Store().ListInstances()
		return len(instances) > 0 && instances[0].State == "running"
	})
	catalog.Specs[0].WeightSources = `[{"provider":"huggingface","repo_id":"Acme/Pinned"}]`
	if err := env.Store().ReconcileInferenceCatalog([]store.InferenceModel{catalog}); err != nil {
		t.Fatal(err)
	}
	resp, err = api.PostJSON("/api/v1/evaluate", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var wire struct {
		Acquisition struct {
			Nodes []struct {
				WeightsPresent bool   `json:"weights_present"`
				WeightsPlan    string `json:"weights_plan"`
			} `json:"nodes"`
		} `json:"acquisition"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&wire); err != nil {
		t.Fatal(err)
	}
	// The pinned copy already on the node is what the evaluation finds: the
	// node is forecast to use it rather than fetch the unpinned revision.
	if resp.StatusCode != http.StatusOK || len(wire.Acquisition.Nodes) != 1 || !wire.Acquisition.Nodes[0].WeightsPresent || wire.Acquisition.Nodes[0].WeightsPlan != "present" {
		t.Fatalf("evaluation=%+v status=%d", wire, resp.StatusCode)
	}
	models, err := env.Store().ListModels()
	if err != nil || len(models) != 1 || models[0].ID != model.ID {
		t.Fatalf("models=%+v err=%v", models, err)
	}
}
