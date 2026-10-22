package deploy

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// TestDGPUQuotaDeploymentCarriesInjectionThroughTheRealAgentWire verifies the
// complete quota path rather than only the dGPU injection builder: the API/controller creates
// an InstanceDesiredState with a memory limit, the real in-process agent applies the
// injection, and the runtime hook observes host/container mounts and env.
func TestDGPUQuotaDeploymentCarriesInjectionThroughTheRealAgentWire(t *testing.T) {
	env := testenv.New(t)
	node := env.AddNode("dgpu-deploy-node", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriverName("docker"))
	env.WaitNodeOnline(t, "dgpu-deploy-node")
	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes=%+v err=%v", nodes, err)
	}
	model, err := env.Store().UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "dgpu-model", Revision: "main", Name: "dgpu-model",
		Arch: "Qwen3ForCausalLM", WeightBytes: 1 << 30,
		Meta: `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: nodes[0].ID, State: proto.MFPresent, Path: testenv.ModelDir(t, "dgpu")}); err != nil {
		t.Fatal(err)
	}
	runSeen := make(chan runtime.WorkloadSpec, 1)
	node.Driver.AfterRun(func(spec runtime.WorkloadSpec) {
		select {
		case runSeen <- spec:
		default:
		}
	})
	api := env.API(t)
	body, _ := json.Marshal(map[string]any{
		"model_id": model.ID, "node_uuid": nodes[0].UUID, "served_name": "dgpu-model",
		"vram_limit_gib": 8, "quick_params": map[string]any{"max_model_len": 256},
	})
	created, err := api.PostJSON("/api/v1/deployments", body)
	if err != nil || created.StatusCode != http.StatusCreated {
		data := []byte(nil)
		if created != nil {
			data, _ = io.ReadAll(created.Body)
			created.Body.Close()
		}
		t.Fatalf("create quota deployment status=%v body=%s err=%v", dgpuResponseStatus(created), data, err)
	}
	created.Body.Close()
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		rows, listErr := env.Store().ListInstances()
		if listErr == nil && len(rows) == 1 && rows[0].State == proto.InstRunning {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	rows, _ := env.Store().ListInstances()
	if len(rows) != 1 || rows[0].State != proto.InstRunning {
		depRow, _ := env.Store().GetDeployment(1)
		t.Fatalf("quota instance did not become running: deployment=%+v rows=%+v", depRow, rows)
	}
	var spec runtime.WorkloadSpec
	select {
	case spec = <-runSeen:
	case <-time.After(5 * time.Second):
		t.Fatal("AfterRun did not observe quota workload")
	}
	if spec.Env["LD_PRELOAD"] != "/gpuworks/libvgpu.so" {
		t.Fatalf("quota LD_PRELOAD=%q", spec.Env["LD_PRELOAD"])
	}
	if spec.Env["CUDA_DEVICE_MEMORY_LIMIT_0"] != "8589934592" {
		t.Fatalf("quota memory limit=%q", spec.Env["CUDA_DEVICE_MEMORY_LIMIT_0"])
	}
	if !strings.HasSuffix(spec.Env["CUDA_DEVICE_MEMORY_SHARED_CACHE"], ".cache") {
		t.Fatalf("quota cache env=%q", spec.Env["CUDA_DEVICE_MEMORY_SHARED_CACHE"])
	}
	var libMount, cacheMount bool
	for _, mount := range spec.Mounts {
		if mount.Target == "/gpuworks/libvgpu.so" && strings.HasSuffix(mount.Source, "/dgpu/libvgpu.so") {
			libMount = true
		}
		if mount.Target == "/gpuworks/cache" && strings.HasSuffix(mount.Source, "/dgpu-cache") {
			cacheMount = true
		}
	}
	if !libMount || !cacheMount {
		t.Fatalf("quota mounts=%+v", spec.Mounts)
	}

	// A new quota from the dGPU page is in effect at once: the running slice
	// is not restarted, its desired state carries the new limit, and the
	// agent rewrites the limit in the slice's HAMi-core shared cache - here
	// laid down as libvgpu's first process would have left it.
	oldUUID := rows[0].UUID
	cache := filepath.Join(node.StateDir(), "dgpu-cache", oldUUID+".cache")
	writeHAMiCache(t, cache, 8<<30)
	req, _ := http.NewRequest(http.MethodPut, "/api/v1/deployments/1/dgpu", strings.NewReader(`{"vram_limit_gib":12}`))
	req.Header.Set("Content-Type", "application/json")
	changed, err := api.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(changed.Body)
	changed.Body.Close()
	if changed.StatusCode != http.StatusOK || !strings.Contains(string(data), `"live":["`+oldUUID+`"]`) {
		t.Fatalf("quota change status=%d body=%s", changed.StatusCode, data)
	}
	deadline = time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && hamiLimit(t, cache) != 12<<30 {
		time.Sleep(25 * time.Millisecond)
	}
	if got := hamiLimit(t, cache); got != 12<<30 {
		t.Fatalf("limit in the slice's shared cache = %d, want 12 GiB", got)
	}
	select {
	case again := <-runSeen:
		t.Fatalf("the slice was started again: %+v", again.Env)
	default:
	}
	rows, _ = env.Store().ListInstances()
	if len(rows) != 1 || rows[0].UUID != oldUUID || rows[0].State != proto.InstRunning {
		t.Fatalf("after the quota change instances=%+v, want the same running slice", rows)
	}
	var held proto.InstanceDesiredState
	if err := json.Unmarshal([]byte(rows[0].DesiredStateJSON), &held); err != nil || held.VRAMLimitBytes != 12<<30 {
		t.Fatalf("slice spec=%+v err=%v", held, err)
	}
}

// writeHAMiCache lays down a HAMi-core v1.2 shared cache (dgpucache's
// layout) with limit on device 0 and no process holding memory.
func writeHAMiCache(t *testing.T, path string, limit uint64) {
	t.Helper()
	data := make([]byte, 1856+1024*1960+4)
	binary.LittleEndian.PutUint32(data[0:], 19920718)
	binary.LittleEndian.PutUint32(data[4:], 1)
	binary.LittleEndian.PutUint32(data[8:], 2)
	binary.LittleEndian.PutUint64(data[1600:], limit)
	if err := os.WriteFile(path, data, 0o666); err != nil {
		t.Fatal(err)
	}
}

func hamiLimit(t *testing.T, path string) uint64 {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return binary.LittleEndian.Uint64(data[1600:])
}

func dgpuResponseStatus(resp *http.Response) any {
	if resp == nil {
		return "<nil>"
	}
	return resp.StatusCode
}
