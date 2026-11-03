//go:build linux && amd64

package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
	"gpuworks.ai/gpuworks/internal/testutil/catalogfixture"
)

// TestAMD395TemplateChatAndStop exercises the control plane with a reported
// gfx1151 card. The GPU prober and runtime are fakes: this does not exercise
// host sysfs/KFD, ROCm containers, or inference on AMD hardware.
// The assembled agent reports the real host OS/architecture, so this scenario
// runs on the Linux/amd64 host contract supported by the AMD implementation.
func TestAMD395TemplateChatAndStop(t *testing.T) {
	env := testenv.New(t)
	healthy := true
	// The fake runtime reports Docker protocol identity for AMD admission.
	// It runs an in-process HTTP listener and never contacts a Docker daemon.
	driver := testenv.NewFakeDriver("docker")
	t.Cleanup(driver.CloseAll)
	runs := make(chan runtime.WorkloadSpec, 1)
	driver.AfterRun(func(spec runtime.WorkloadSpec) {
		if !strings.HasPrefix(spec.Name, "gpuworks-partition-probe-") {
			select {
			case runs <- spec:
			default:
			}
		}
	})
	good := env.AddNode("amd395-gfx1151", testenv.WithGPUList(testenv.GPUSpec{
		Model: "AMD GPU 0x1586 (gfx1151)", Vendor: "amd", ArchID: "gfx1151", VRAMMB: 65536, Healthy: &healthy,
	}), testenv.WithAccelerator("amd", "rocm", "7.2.3"), testenv.WithDriver(driver), testenv.WithoutAcceleratorRuntimeInjection())
	bad := env.AddNode("amd395-gfx1150", testenv.WithGPUList(testenv.GPUSpec{
		Model: "AMD GPU 0x1586 (gfx1151)", Vendor: "amd", ArchID: "gfx1150", VRAMMB: 65536, Healthy: &healthy,
	}), testenv.WithAccelerator("amd", "rocm", "7.2.3"), testenv.WithDriverName("docker"), testenv.WithoutAcceleratorRuntimeInjection())
	env.WaitNodeOnline(t, good.Name)
	env.WaitNodeOnline(t, bad.Name)

	fixture := t.TempDir()
	catalogfixture.WriteLaunchOverlay(t, "../../server/inferencecatalog/seed/catalog", "../../../build/inference-templates/amd395/catalog", fixture)
	pkg, err := inferencecatalog.LoadPackage(os.DirFS(fixture))
	if err != nil {
		t.Fatal(err)
	}
	models, err := inferencecatalog.ToFlatStoreModels(pkg.Catalog)
	if err != nil {
		t.Fatal(err)
	}
	const modelID = "qwen/qwen3-0.6b"
	const specName = "bf16-vllm-amd395-gfx1151-local"
	const fakeImage = "example.invalid/amd395-vllm:control-plane-test"
	var selected *store.InferenceModel
	for i := range models {
		if models[i].ModelID == modelID {
			copyModel := models[i]
			selected = &copyModel
			break
		}
	}
	if selected == nil {
		t.Fatalf("AMD package lacks %s", modelID)
	}
	selected.Specs = nil
	for _, model := range models {
		if model.ModelID != modelID {
			continue
		}
		for _, spec := range model.Specs {
			if spec.Name == specName {
				spec.Image = fakeImage // Explicit test-only mapping of the shipped placeholder.
				selected.Specs = append(selected.Specs, spec)
			}
		}
	}
	if len(selected.Specs) != 1 {
		t.Fatalf("AMD vLLM spec count=%d", len(selected.Specs))
	}
	if err := env.Store().ImportUserInferenceCatalog([]store.InferenceModel{*selected}, false); err != nil {
		t.Fatal(err)
	}
	template, err := env.Store().InferenceModelByModelID(modelID)
	if err != nil {
		t.Fatal(err)
	}
	model, err := env.Store().UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "Qwen/Qwen3-0.6B", Revision: "c1899de289a04d12100db370d81485cdf75e47ca",
		Name: "Qwen3-0.6B", Arch: "Qwen3ForCausalLM", WeightBytes: 1503300328,
		Meta: `{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"],"num_hidden_layers":28,"hidden_size":1024,"num_attention_heads":16,"num_key_value_heads":8,"max_position_embeddings":40960}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := env.Store().ListNodes()
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range nodes {
		wantArch := "gfx1151"
		if node.UUID == bad.UUID {
			wantArch = "gfx1150"
		}
		if node.GPUVendor != "amd" || len(node.GPUs) != 1 || node.GPUs[0].Vendor != "amd" || node.GPUs[0].ArchID != wantArch {
			t.Fatalf("registered AMD node facts: %+v", node)
		}
		if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: testenv.ModelDir(t, "amd395")}); err != nil {
			t.Fatal(err)
		}
	}
	api := env.API(t)
	create := func(uuid string) (*http.Response, error) {
		body, _ := json.Marshal(map[string]any{"template_id": template.ID, "node_uuid": uuid, "served_name": "amd395-sim", "quick_params": map[string]any{}, "placement": map[string]any{}})
		return api.PostJSON("/api/v1/deployments", body)
	}
	denied, err := create(bad.UUID)
	if err != nil {
		t.Fatal(err)
	}
	deniedBody, _ := io.ReadAll(denied.Body)
	denied.Body.Close()
	if denied.StatusCode != http.StatusBadRequest || !bytes.Contains(deniedBody, []byte("no matching spec")) {
		t.Fatalf("gfx1150 chip mismatch status=%d body=%s", denied.StatusCode, deniedBody)
	}
	created, err := create(good.UUID)
	if err != nil {
		t.Fatal(err)
	}
	defer created.Body.Close()
	if created.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(created.Body)
		t.Fatalf("gfx1151 status=%d body=%s", created.StatusCode, body)
	}
	var deployment struct {
		ID               uint   `json:"id"`
		TemplateSpecName string `json:"template_spec_name"`
	}
	if err := json.NewDecoder(created.Body).Decode(&deployment); err != nil {
		t.Fatal(err)
	}
	if deployment.ID == 0 || deployment.TemplateSpecName != specName {
		t.Fatalf("chosen template=%+v", deployment)
	}
	var workload runtime.WorkloadSpec
	select {
	case workload = <-runs:
	case <-time.After(20 * time.Second):
		t.Fatal("AMD agent did not receive workload")
	}
	var instance store.Instance
	env.Eventually(t, 30*time.Second, func() bool {
		rows, _ := env.Store().InstancesForDeployment(deployment.ID)
		if len(rows) != 1 || rows[0].State != proto.InstRunning || rows[0].APIPort == 0 {
			return false
		}
		instance = rows[0]
		return true
	})
	var desired proto.InstanceDesiredState
	if err := json.Unmarshal([]byte(instance.DesiredStateJSON), &desired); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(desired.GPUIndexes, []int{0}) || !reflect.DeepEqual(workload.GPUIndexes, []int{0}) || workload.Image != fakeImage || workload.Env["VLLM_ROCM_USE_AITER"] != "0" {
		t.Fatalf("AMD desired/workload mismatch: desired=%+v workload=%+v", desired, workload)
	}
	key, err := env.Store().CreateAPIKey("amd395-simulation", 0)
	if err != nil {
		t.Fatal(err)
	}
	chatReq, _ := http.NewRequest(http.MethodPost, env.URL()+"/v1/chat/completions", bytes.NewReader([]byte(`{"model":"amd395-sim","messages":[{"role":"user","content":"hello"}]}`)))
	chatReq.Header.Set("Authorization", "Bearer "+key)
	chatReq.Header.Set("Content-Type", "application/json")
	chat, err := (&http.Client{Timeout: 10 * time.Second}).Do(chatReq)
	if err != nil {
		t.Fatal(err)
	}
	chatBody, _ := io.ReadAll(chat.Body)
	chat.Body.Close()
	if chat.StatusCode != http.StatusOK || !bytes.Contains(chatBody, []byte(`"choices"`)) {
		t.Fatalf("chat status=%d body=%s", chat.StatusCode, chatBody)
	}
	postAction(t, api, fmt.Sprintf("/api/v1/deployments/%d/stop", deployment.ID), ``, http.StatusOK)
	env.Eventually(t, 30*time.Second, func() bool { rows, _ := env.Store().InstancesForDeployment(deployment.ID); return len(rows) == 0 })
	env.Eventually(t, 10*time.Second, func() bool {
		status, err := driver.Status(context.Background(), instance.ContainerID)
		return err == nil && !status.Running
	})
}
