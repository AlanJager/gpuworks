//go:build docker

package l3

import (
	"encoding/json"
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

type v4DockerFixture struct {
	env *testenv.Env
	api *testenv.APIClient
	// agent is the in-process node runtime behind node; its state directory is
	// where the docker driver keeps the managed mounts it creates.
	agent    *testenv.Node
	model    *store.Model
	node     *store.Node
	image    *store.Image
	imageRef string
}

func newV4DockerFixture(t *testing.T, suffix string) v4DockerFixture {
	t.Helper()
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
	imageRef := "gpuworks/fake-engine:v4-" + suffix
	tagFakeEngineImage(t, "gpuworks/fake-engine:test", imageRef)
	env := testenv.New(t)
	nodeHandle := env.AddNode("v4-docker-"+suffix, testenv.WithGPUs(1, testenv.GPUSpec{Model: "NVIDIA A100 80GB", VRAMMB: 81920, ComputeCapability: "8.0"}), testenv.WithDriver(dockerdrv.New()))
	env.WaitNodeOnline(t, nodeHandle.Name)
	// Spec-matched creates read the node's GPUs; the inventory lands in the
	// first status.node report, after online.
	env.WaitNodeGPUs(t, nodeHandle.Name, 1)
	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes=%+v err=%v", nodes, err)
	}
	node, err := env.Store().NodeWithGPUs(nodes[0].UUID)
	if err != nil {
		t.Fatal(err)
	}
	modelDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(modelDir, "config.json"), []byte(`{"model_type":"qwen3"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	model, err := env.Store().UpsertModel(&store.Model{
		// The template deliberately leaves ModelScope revision empty (the
		// default branch); keep the explicit model binding identical so BUG-059
		// validation does not reject this otherwise valid deployment.
		Source: "modelscope", SourceID: "gpuworks/v4-" + suffix, Revision: "", Name: "v4-" + suffix,
		Arch: "qwen3", WeightBytes: 1 << 30,
		Meta: `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: modelDir}); err != nil {
		t.Fatal(err)
	}
	image, err := env.Store().UpsertImage(&store.Image{
		Name: "v4 " + suffix, ImageRef: imageRef, Source: "custom", Engine: "vllm", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return v4DockerFixture{env: env, api: env.API(t), agent: nodeHandle, model: model, node: node, image: image, imageRef: imageRef}
}

func (f v4DockerFixture) createDeployment(t *testing.T, body map[string]any) (uint, store.Instance) {
	t.Helper()
	body["model_id"] = f.model.ID
	body["node_uuid"] = f.node.UUID
	body["engine"] = "vllm"
	encoded, _ := json.Marshal(body)
	resp, err := f.api.PostJSON("/api/v1/deployments", encoded)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated {
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("create deployment: %d %s", resp.StatusCode, data)
	}
	var created struct {
		ID uint `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	var instance store.Instance
	f.env.Eventually(t, 45*time.Second, func() bool {
		instances, _ := f.env.Store().InstancesForDeployment(created.ID)
		if len(instances) != 1 {
			return false
		}
		instance = instances[0]
		if instance.State == proto.InstFailed {
			t.Fatalf("deployment failed: %+v", instance)
		}
		return instance.State == proto.InstRunning && instance.ContainerID != ""
	})
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", instance.ContainerID).Run() })
	return created.ID, instance
}

func (f v4DockerFixture) deleteDeployment(t *testing.T, id uint) {
	t.Helper()
	resp, err := f.api.Delete("/api/v1/deployments/" + strconv.FormatUint(uint64(id), 10))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestDockerInferenceTemplateDeployPropagatesProfileArgsAndEnv(t *testing.T) {
	fixture := newV4DockerFixture(t, "inference-template")
	const specName = "bf16-throughput"
	// The weight source's revision must match fixture.model.Revision exactly:
	// template-owned deploys resolve the weight model by (provider, repo_id,
	// revision), and a mismatch silently resolves to a different, file-less
	// model row instead of the fixture's — the deployment then waits forever
	// for a model file that will never appear, with no error anywhere.
	catalogModel := store.InferenceModel{ModelID: fixture.model.SourceID, DisplayTitle: "L3 inference template", Source: "upstream", Specs: []store.InferenceSpecRow{{
		Name: specName, Scenario: "throughput", Engine: "vllm", Image: fixture.imageRef, Quantization: "bf16",
		WeightSources: `[{"provider":"modelscope","repo_id":"` + fixture.model.SourceID + `","revision":"` + fixture.model.Revision + `"}]`, GPUs: `{"vendor":"nvidia","cc":">=7.5"}`,
		Args: `["--trust-remote-code"]`, Env: `{"INFERENCE_TEMPLATE_PROFILE":"applied"}`, PreStartArgv: `[]`,
	}}}
	if err := fixture.env.Store().ReconcileInferenceCatalog([]store.InferenceModel{catalogModel}); err != nil {
		t.Fatal(err)
	}
	graph, err := fixture.env.Store().InferenceModelByModelID(catalogModel.ModelID)
	if err != nil {
		t.Fatal(err)
	}
	depID, instance := fixture.createDeployment(t, map[string]any{
		"template_id": graph.ID, "template_spec_name": specName, "scenario": "throughput", "served_name": "l3-inference-template",
	})
	argv := containerArgv(t, instance.ContainerID)
	if !containsAll(argv, "serve", "/model", "--trust-remote-code") {
		t.Fatalf("inference template argv=%v", argv)
	}
	if !containsArg(containerEnv(t, instance.ContainerID), "INFERENCE_TEMPLATE_PROFILE=applied") {
		t.Fatalf("inference template env=%v", containerEnv(t, instance.ContainerID))
	}
	dep, err := fixture.env.Store().GetDeployment(depID)
	if err != nil || dep.TemplateID != graph.ID || dep.TemplateSpecName != specName || dep.TemplateDeviated {
		t.Fatalf("deployment provenance=%+v err=%v", dep, err)
	}
	key, err := fixture.env.Store().CreateAPIKey("l3-inference-template", 0)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, fixture.env.URL()+"/v1/chat/completions", strings.NewReader(`{"model":"l3-inference-template","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(data), "choices") {
		t.Fatalf("gateway status=%d body=%s", resp.StatusCode, data)
	}
	fixture.deleteDeployment(t, depID)
}

func TestDockerKVOffloadReachesContainerArgvAndEnv(t *testing.T) {
	fixture := newV4DockerFixture(t, "kv-offload")
	depID, instance := fixture.createDeployment(t, map[string]any{
		"image": fixture.imageRef, "served_name": "l3-kv",
		"quick_params":        map[string]any{"kv_offloading_size": 8, "long_prefill_token_threshold": 1024},
		"container_overrides": map[string]any{"env": map[string]string{"KV_OFFLOAD_TEST": "applied"}},
	})
	argv := containerArgv(t, instance.ContainerID)
	if !containsAll(argv, "--kv-offloading-size", "8", "--kv-offloading-backend", "native", "--long-prefill-token-threshold", "1024") {
		t.Fatalf("KV offload argv=%v", argv)
	}
	env := containerEnv(t, instance.ContainerID)
	if !containsArg(env, "KV_OFFLOAD_TEST=applied") {
		t.Fatalf("KV offload env=%v", env)
	}
	fixture.deleteDeployment(t, depID)
}

func TestDockerCustomImageDeleteSynchronizesNodeCatalog(t *testing.T) {
	fixture := newV4DockerFixture(t, "image-delete")
	fixture.env.Eventually(t, 20*time.Second, func() bool {
		desired, err := fixture.env.Store().DesiredStateForNode(fixture.node.ID)
		return err == nil && containsArg(desired.ImageRefs, fixture.imageRef)
	})
	// The real docker-backed agent reports the tagged fixture through its normal
	// heartbeat inventory before deletion.
	fixture.env.Eventually(t, 20*time.Second, func() bool {
		row, err := fixture.env.Store().ImageNode(fixture.image.ID, fixture.node.ID)
		return err == nil && row.State == store.ImageStatePresent
	})
	resp, err := fixture.api.Delete("/api/v1/images/" + strconv.FormatUint(uint64(fixture.image.ID), 10))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete image status=%d body=%s", resp.StatusCode, data)
	}
	fixture.env.Eventually(t, 10*time.Second, func() bool {
		if _, err := fixture.env.Store().GetImage(fixture.image.ID); err != store.ErrNotFound {
			return false
		}
		if _, err := fixture.env.Store().ImageNode(fixture.image.ID, fixture.node.ID); err != store.ErrNotFound {
			return false
		}
		desired, err := fixture.env.Store().DesiredStateForNode(fixture.node.ID)
		return err == nil && !containsArg(desired.ImageRefs, fixture.imageRef)
	})
}

func containerEnv(t *testing.T, containerID string) []string {
	t.Helper()
	out, err := exec.Command("docker", "inspect", containerID, "--format", "{{json .Config.Env}}").Output()
	if err != nil {
		t.Fatalf("inspect container env: %v", err)
	}
	var env []string
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &env); err != nil {
		t.Fatalf("decode container env: %v: %s", err, out)
	}
	return env
}
