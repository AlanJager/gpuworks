package api

import (
	"encoding/json"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestRedactBenchmarkEnvBySensitiveKeySuffix(t *testing.T) {
	got := redactBenchmarkEnv(map[string]string{
		"HF_TOKEN":                       "hf-secret",
		"HUGGING_FACE_HUB_TOKEN":         "hub-secret",
		"OPENAI_API_KEY":                 "api-secret",
		"AWS_SECRET_ACCESS_KEY":          "aws-secret",
		"GOOGLE_APPLICATION_CREDENTIALS": "credentials-secret",
		"VLLM_LOGGING_LEVEL":             "INFO",
	})
	for _, key := range []string{"HF_TOKEN", "HUGGING_FACE_HUB_TOKEN", "OPENAI_API_KEY", "AWS_SECRET_ACCESS_KEY", "GOOGLE_APPLICATION_CREDENTIALS"} {
		if got[key] != "<redacted>" {
			t.Errorf("%s = %q, want redacted", key, got[key])
		}
	}
	if got["VLLM_LOGGING_LEVEL"] != "INFO" {
		t.Fatalf("non-sensitive env changed: %+v", got)
	}
}

func TestBenchmarkTensorParallelFromArgv(t *testing.T) {
	if got := benchmarkTensorParallel(deploy.EngineVLLM, []string{"serve", "model", "--tensor-parallel-size", "4"}, 2); got != 4 {
		t.Fatalf("tp = %d, want 4", got)
	}
	if got := benchmarkTensorParallel(deploy.EngineVLLM, []string{"--tensor-parallel-size=8"}, 2); got != 8 {
		t.Fatalf("tp = %d, want 8", got)
	}
	// The other spellings belong to the engine too: SGLang injects --tp-size.
	if got := benchmarkTensorParallel(deploy.EngineSGLang, []string{"--tp-size", "4"}, 2); got != 4 {
		t.Fatalf("sglang tp = %d, want 4", got)
	}
	// An engine with no TP flags of its own (a callable adapter owns its argv)
	// reports the fallback rather than reading a spelling that is not its own.
	if got := benchmarkTensorParallel(deploy.EngineCallable, []string{"--tensor-parallel-size", "4"}, 2); got != 2 {
		t.Fatalf("callable tp = %d, want the fallback 2", got)
	}
}

func TestManagedBenchmarkTargetSnapshotSurvivesDeploymentDeletion(t *testing.T) {
	st, err := store.Open("file:benchmark_snapshot_lifecycle?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("snapshot-node", store.HashToken("token"), "snapshot-host")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Model(node).Update("name", "Snapshot Node").Error; err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Create(&store.GPU{NodeID: node.ID, Index: 2, UUID: "GPU-snapshot", Name: "Quadro RTX 8000", ArchID: "sm75", Vendor: "nvidia"}).Error; err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/snapshot-model", Revision: "main", Name: "snapshot-model"})
	if err != nil {
		t.Fatal(err)
	}
	deployment, err := st.CreateDeployment(&store.Deployment{Name: "snapshot deployment", ModelID: model.ID, ServedName: "served-snapshot", Engine: "vllm", Image: "fallback:image", State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	specRaw, err := json.Marshal(proto.InstanceDesiredState{
		Engine: "sglang", Image: "runtime:image",
		ResolvedLaunch: proto.ResolvedLaunch{
			CommandMode: "manual",
			Entrypoint:  []string{"python", "-m", "sglang.launch_server"},
			Argv:        []string{"--model-path", "/models/snapshot", "--tensor-parallel-size=4"},
			Env: map[string]string{
				"HF_TOKEN": "hf-secret", "HUGGING_FACE_HUB_TOKEN": "hub-secret", "LOG_LEVEL": "debug",
			},
			DockerArgs: []string{"--ipc=host"},
		},
		GPUIndexes: []int{2}, ModelPath: "/models/snapshot", APIPort: 31000,
	})
	if err != nil {
		t.Fatal(err)
	}
	instance, err := st.CreateInstance(&store.Instance{
		UUID: "snapshot-instance", DeploymentID: deployment.ID, NodeID: node.ID,
		State: proto.InstRunning, APIPort: 31000, GPUIndexes: `[2]`, DesiredStateJSON: string(specRaw),
	})
	if err != nil {
		t.Fatal(err)
	}
	handlers := &apiHandlers{d: Deps{Store: st}}
	raw, err := handlers.benchmarkTargetSnapshot("instance", "instance", "http://127.0.0.1:31000", deployment.ServedName, node.UUID, 1, deployment, instance, []store.Instance{*instance})
	if err != nil {
		t.Fatal(err)
	}
	var snapshot benchmarkTargetSnapshot
	if err := json.Unmarshal([]byte(raw), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.DeploymentID != deployment.ID || snapshot.DeploymentName != deployment.Name || snapshot.InstanceUUID != instance.UUID || snapshot.ServedName != deployment.ServedName {
		t.Fatalf("snapshot references = %+v", snapshot)
	}
	if snapshot.ModelSourceID != model.SourceID || snapshot.Engine != "sglang" || snapshot.Image != "runtime:image" || snapshot.CommandMode != "manual" || snapshot.TensorParallel != 4 {
		t.Fatalf("snapshot runtime identity = %+v", snapshot)
	}
	if len(snapshot.Entrypoint) != 3 || len(snapshot.Argv) != 3 || len(snapshot.DockerArgs) != 1 || snapshot.Env["LOG_LEVEL"] != "debug" || snapshot.Env["HF_TOKEN"] != "<redacted>" || snapshot.Env["HUGGING_FACE_HUB_TOKEN"] != "<redacted>" {
		t.Fatalf("snapshot command/env = %+v", snapshot)
	}
	if snapshot.NodeID != node.ID || snapshot.NodeName != "Snapshot Node" || len(snapshot.GPUs) != 1 || snapshot.GPUs[0].Index != 2 || snapshot.GPUs[0].Name != "Quadro RTX 8000" || snapshot.GPUs[0].ArchID != "sm75" || snapshot.GPUs[0].Vendor != "nvidia" || snapshot.GPUs[0].UUID != "GPU-snapshot" {
		t.Fatalf("snapshot hardware = %+v", snapshot)
	}
	run, err := st.CreateBenchmarkRun(&store.BenchmarkRun{
		Name: "snapshot run", TargetKind: "instance", DeploymentID: deployment.ID,
		InstanceUUID: instance.UUID, Model: deployment.ServedName, LoadOrigin: node.UUID,
		TargetSnapshot: raw, State: "done",
	})
	if err != nil {
		t.Fatal(err)
	}
	if dto := handlers.benchmarkDTO(run, false); dto.DeploymentDeleted || dto.InstanceDeleted {
		t.Fatalf("live target marked deleted: %+v", dto)
	}
	if err := st.DeleteDeployment(deployment.ID); err != nil {
		t.Fatal(err)
	}
	persisted, err := st.GetBenchmarkRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.TargetSnapshot != raw {
		t.Fatalf("snapshot changed after target deletion:\n got %s\nwant %s", persisted.TargetSnapshot, raw)
	}
	dto := handlers.benchmarkDTO(persisted, false)
	if !dto.DeploymentDeleted || !dto.InstanceDeleted || string(dto.TargetSnapshot) != raw {
		t.Fatalf("deleted target DTO = %+v", dto)
	}
}
