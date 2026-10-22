//go:build docker

package l3

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	dockerdrv "gpuworks.ai/gpuworks/internal/agent/runtime/docker"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

func TestDockerInferenceTemplateCloneEditPreStartDeploySnapshot(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		skipDocker(t, err)
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		skipDocker(t, err)
	}
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	if err := ensureFakeImage(t, root); err != nil {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatal(err)
		}
		t.Skip(err)
	}
	if err := ensureFakePreStartImage(root); err != nil {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatal(err)
		}
		t.Skip(err)
	}
	registerFakePreStartEngine(t)

	env := testenv.New(t)
	node := env.AddNode("docker-template-matched-by",
		testenv.WithGPUs(1, testenv.GPUSpec{Model: "NVIDIA A100 80GB", VRAMMB: 81920, ComputeCapability: "8.0"}),
		testenv.WithDriver(dockerdrv.New()),
	)
	env.WaitNodeOnline(t, node.Name)
	// The template's spec is matched against the node's GPUs at create time;
	// the inventory lands in the first status.node report, after online.
	env.WaitNodeGPUs(t, node.Name, 1)
	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes=%+v err=%v", nodes, err)
	}

	modelDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(modelDir, "config.json"), []byte(`{"model_type":"qwen3"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	model, err := env.Store().UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "gpuworks/l3-template", Name: "gpuworks/l3-template",
		Arch: "qwen3", WeightBytes: 1 << 30,
		Meta: `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: nodes[0].ID, State: proto.MFPresent, Path: modelDir}); err != nil {
		t.Fatal(err)
	}
	modelCatalog := store.InferenceModel{ModelID: "gpuworks/l3-template", DisplayTitle: "L3 matched-by template", Source: "upstream", InputModalities: `["text"]`, ContextLength: 2048, Specs: []store.InferenceSpecRow{{
		Name: "bf16-balanced", Scenario: "balanced", Engine: "fake-engine", Image: "gpuworks/fake-engine:prestart", Quantization: "bf16",
		WeightSources: `[{"provider":"huggingface","repo_id":"gpuworks/l3-template"}]`, GPUs: `{"vendor":"nvidia","cc":">=7.5"}`, Args: `[]`, Env: `{}`, PreStartArgv: `[]`,
	}}}
	if err := env.Store().ReconcileInferenceCatalog([]store.InferenceModel{modelCatalog}); err != nil {
		t.Fatal(err)
	}
	graph, err := env.Store().InferenceModelByModelID(modelCatalog.ModelID)
	if err != nil {
		t.Fatal(err)
	}

	api := env.API(t)
	cloneBody, _ := json.Marshal(map[string]any{"source_model_id": graph.ModelID, "model_id": "gpuworks/l3-template-custom", "display_title": "L3 custom pre-start"})
	cloned, err := api.PostJSON("/api/v1/inference-templates/clone", cloneBody)
	if err != nil {
		t.Fatal(err)
	}
	if cloned.StatusCode != http.StatusCreated {
		data, _ := io.ReadAll(cloned.Body)
		cloned.Body.Close()
		t.Fatalf("clone inference template: %d %s", cloned.StatusCode, data)
	}
	cloned.Body.Close()
	editedYAML := `models:
- model_id: gpuworks/l3-template-custom
  source: user
  display_title: L3 custom pre-start
  input_modalities: [text]
  specs:
  - name: bf16-balanced
    scenario: balanced
    engine: fake-engine
    image: gpuworks/fake-engine:prestart
    weights:
      quantization: bf16
      sources:
      - huggingface: gpuworks/l3-template
    weight_bytes: 1073741824
    gpus: {vendor: nvidia, cc: ">=7.5"}
    pre_start_argv: [sh, -c, "printf ready > /tmp/catalog-prestart"]
`
	importRequest, err := http.NewRequest(http.MethodPost, "/api/v1/inference-templates/import?overwrite=true", strings.NewReader(editedYAML))
	if err != nil {
		t.Fatal(err)
	}
	importRequest.Header.Set("Content-Type", "application/yaml")
	imported, err := api.Do(importRequest)
	if err != nil {
		t.Fatal(err)
	}
	if imported.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(imported.Body)
		imported.Body.Close()
		t.Fatalf("edit cloned inference template: %d %s", imported.StatusCode, data)
	}
	imported.Body.Close()
	custom, err := env.Store().InferenceModelByModelID("gpuworks/l3-template-custom")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"template_id": custom.ID, "template_spec_name": "bf16-balanced", "node_uuid": nodes[0].UUID, "served_name": "l3-template-matched-by"})
	created, err := api.PostJSON("/api/v1/deployments", body)
	if err != nil {
		t.Fatal(err)
	}
	if created.StatusCode != http.StatusCreated {
		data, _ := io.ReadAll(created.Body)
		created.Body.Close()
		t.Fatalf("create template deployment: %d %s", created.StatusCode, data)
	}
	var response struct {
		ID        uint   `json:"id"`
		MatchedBy string `json:"matched_by"`
	}
	if err := json.NewDecoder(created.Body).Decode(&response); err != nil {
		created.Body.Close()
		t.Fatal(err)
	}
	created.Body.Close()
	if response.ID == 0 || !strings.Contains(response.MatchedBy, "spec:bf16-balanced") {
		t.Fatalf("deployment response=%+v", response)
	}

	var containerID string
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		instances, _ := env.Store().ListInstances()
		if len(instances) == 1 {
			if instances[0].State == proto.InstRunning && instances[0].ContainerID != "" {
				containerID = instances[0].ContainerID
				break
			}
			if instances[0].State == proto.InstFailed {
				t.Fatalf("deployment failed early: %+v", instances[0])
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if containerID == "" {
		instances, _ := env.Store().ListInstances()
		t.Fatalf("deployment did not become running: %+v", instances)
	}
	if output, err := exec.Command("docker", "exec", containerID, "sh", "-c", "test \"$(cat /tmp/catalog-prestart)\" = ready").CombinedOutput(); err != nil {
		t.Fatalf("pre-start marker missing: %v: %s", err, output)
	}
	t.Cleanup(func() {
		if containerID != "" {
			_ = exec.Command("docker", "rm", "-f", containerID).Run()
		}
	})
	persisted, err := env.Store().GetDeployment(response.ID)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot map[string]any
	if err := json.Unmarshal([]byte(persisted.CatalogSelectionSnapshot), &snapshot); err != nil {
		t.Fatal(err)
	}
	if matchedBy, _ := snapshot["matched_by"].(string); !strings.Contains(matchedBy, "spec:bf16-balanced") {
		t.Fatalf("template snapshot matched_by=%q snapshot=%s", matchedBy, persisted.CatalogSelectionSnapshot)
	}
	preStart, _ := snapshot["pre_start_argv"].([]any)
	if len(preStart) != 3 || preStart[0] != "sh" || !strings.Contains(preStart[2].(string), "catalog-prestart") {
		t.Fatalf("template snapshot pre_start_argv=%#v snapshot=%s", preStart, persisted.CatalogSelectionSnapshot)
	}
	deleted, err := api.Delete("/api/v1/deployments/" + strconv.FormatUint(uint64(response.ID), 10))
	if err != nil {
		t.Fatal(err)
	}
	deleted.Body.Close()
	env.Eventually(t, 30*time.Second, func() bool {
		instances, _ := env.Store().ListInstances()
		return len(instances) == 0
	})
}

func ensureFakePreStartImage(root string) error {
	const image = "gpuworks/fake-engine:prestart"
	if err := exec.Command("docker", "image", "inspect", image).Run(); err == nil {
		return nil
	}
	dockerfile := filepath.Join(root, "test", "fake-engine", "Dockerfile.prestart")
	args := []string{"build", "--pull=false", "-f", dockerfile, "-t", image}
	if base := os.Getenv("GPUWORKS_L3_SHELL_BASE_IMAGE"); base != "" {
		args = append(args, "--build-arg", "GPUWORKS_L3_SHELL_BASE_IMAGE="+base)
	}
	args = append(args, root)
	if output, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("build shell-capable fake-engine image: %w: %s", err, output)
	}
	return nil
}
