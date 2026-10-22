package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestFlatSpecTensorParallelUsesAllocatedGPUsUnlessArgsPinIt(t *testing.T) {
	for _, tt := range []struct {
		name     string
		snapshot map[string]any
		wantTP   string
	}{
		{
			name:     "no topology argument derives allocated GPU count",
			snapshot: map[string]any{"extra_args": []string{"--max-model-len", "512"}},
			wantTP:   "2",
		},
		{
			name: "explicit tensor parallel argument is retained once",
			snapshot: map[string]any{"extra_args": []string{
				"--tensor-parallel-size", "1", "--pipeline-parallel-size", "2",
			}},
			wantTP: "1",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			spec := scheduleFlatSpecFixture(t, tt.snapshot)
			if len(spec.GPUIndexes) != 2 {
				t.Fatalf("allocated GPUs=%v, want two", spec.GPUIndexes)
			}
			if got := instanceFlagValues(spec.Argv, "--tensor-parallel-size"); !reflect.DeepEqual(got, []string{tt.wantTP}) {
				t.Fatalf("tensor parallel flags=%v argv=%v, want exactly %q", got, spec.Argv, tt.wantTP)
			}
		})
	}
}

func TestFlatSpecPreStartScriptReachesInstanceDesiredState(t *testing.T) {
	spec := scheduleFlatSpecFixture(t, map[string]any{
		"pre_start_script": "echo preparing weights\napply-local-patches\n",
	})
	if spec.PreStartScript != "echo preparing weights\napply-local-patches\n" || len(spec.PreStartArgv) != 0 {
		t.Fatalf("pre-start script was not retained in instance spec: %+v", spec)
	}
}

func TestMultiComponentDeploymentWaitsThenMountsEveryArtifact(t *testing.T) {
	fixtureName := strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
	st, err := store.Open("file:controller_components_"+fixtureName+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("component-node", store.HashToken("token"), "component-node")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Create(&store.GPU{NodeID: node.ID, Index: 0, UUID: "component-gpu", Name: "NVIDIA H100", Vendor: "nvidia", ArchID: "sm_90", ComputeCapability: "9.0", MemoryTotal: 80 << 30}).Error; err != nil {
		t.Fatal(err)
	}
	models := map[string]*store.Model{}
	for _, id := range []string{"target", "draft", "verifier"} {
		models[id], err = st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "acme/" + id, Name: id, WeightBytes: 1 << 20, Meta: `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":64,"num_attention_heads":2,"num_key_value_heads":1,"max_position_embeddings":512}`})
		if err != nil {
			t.Fatal(err)
		}
	}
	for id, path := range map[string]string{"target": "/host/target", "draft": "/host/draft"} {
		if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: models[id].ID, NodeID: node.ID, State: proto.MFPresent, Path: path}); err != nil {
			t.Fatal(err)
		}
	}
	image, err := st.UpsertImage(&store.Image{Name: "components", Engine: "vllm", Framework: "cuda", ImageRef: "catalog/components:v1", Source: "official", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, _ := json.Marshal(map[string]any{
		"extra_args": []string{"--model", "/models/target", "--speculative-draft-model", "/models/draft", "--speculative-verifier", "/models/verifier"},
		"components": []map[string]any{
			{"name": "target", "role": "primary", "store_model_id": models["target"].ID, "mount_path": "/models/target"},
			{"name": "draft", "role": "speculative_draft", "store_model_id": models["draft"].ID, "mount_path": "/models/draft"},
			{"name": "verifier", "role": "verifier", "store_model_id": models["verifier"].ID, "mount_path": "/models/verifier"},
		},
	})
	dep, err := createTestDeployment(t, st, &store.Deployment{Name: "components", ModelID: models["target"].ID, NodeID: node.ID, ServedName: "components", Engine: "vllm", Image: image.ImageRef, QuickParams: `{"max_model_len":512}`, CatalogSelectionSnapshot: string(snapshot), State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	hub := &distributedTestHub{online: map[string]bool{node.UUID: true}, updates: map[string][]proto.StateUpdate{}}
	ctrl := New(st, hub, nil)
	if err := ctrl.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	instances, _ := st.InstancesForDeployment(dep.ID)
	if len(instances) != 0 {
		t.Fatalf("instance scheduled before verifier was present: %+v", instances)
	}
	verifierFile, err := st.ModelFileByModelNode(models["verifier"].ID, node.ID)
	if err != nil || verifierFile.State == proto.MFPresent || verifierFile.State == proto.MFRegistered {
		t.Fatalf("verifier download=%+v err=%v", verifierFile, err)
	}
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: models["verifier"].ID, NodeID: node.ID, State: proto.MFPresent, Path: "/host/verifier"}); err != nil {
		t.Fatal(err)
	}
	if err := ctrl.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	instances, _ = st.InstancesForDeployment(dep.ID)
	if len(instances) != 1 {
		t.Fatalf("instances=%+v", instances)
	}
	var desired proto.InstanceDesiredState
	if err := json.Unmarshal([]byte(instances[0].DesiredStateJSON), &desired); err != nil {
		t.Fatal(err)
	}
	if desired.ModelPath != "/host/target" || desired.ModelMount != "/models/target" {
		t.Fatalf("primary mount host=%q container=%q", desired.ModelPath, desired.ModelMount)
	}
	want := map[string]string{"/models/draft": "/host/draft", "/models/verifier": "/host/verifier"}
	for _, mount := range desired.Mounts {
		if source, ok := want[mount.Target]; ok {
			if mount.Source != source || !mount.ReadOnly || !strings.HasPrefix(mount.Purpose, "model_component:") {
				t.Fatalf("component mount=%+v", mount)
			}
			delete(want, mount.Target)
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing component mounts=%v, desired=%+v", want, desired.Mounts)
	}
	joined := strings.Join(append(append([]string{}, desired.Entrypoint...), desired.Argv...), " ")
	for _, path := range []string{"/models/target", "/models/draft", "/models/verifier"} {
		if !strings.Contains(joined, path) {
			t.Fatalf("argv %q does not reference %s", joined, path)
		}
	}
}

func TestPlacementResolverUsesFourForAutomaticAndAllEightExplicitGPUs(t *testing.T) {
	for _, test := range []struct {
		name      string
		placement string
		want      int
	}{
		{name: "automatic minimum", want: 4},
		{name: "explicit eight", placement: `{"gpus":[0,1,2,3,4,5,6,7]}`, want: 8},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec := schedulePlacementResolverFixture(t, test.placement)
			if len(spec.GPUIndexes) != test.want {
				t.Fatalf("GPU indexes=%v, want %d", spec.GPUIndexes, test.want)
			}
			if got := instanceFlagValues(spec.Argv, "--tp-size"); !reflect.DeepEqual(got, []string{fmt.Sprint(test.want)}) {
				t.Fatalf("tp-size=%v argv=%v", got, spec.Argv)
			}
			if got := instanceFlagValues(spec.Argv, "--sp-degree"); !reflect.DeepEqual(got, []string{"1"}) {
				t.Fatalf("sp-degree=%v argv=%v", got, spec.Argv)
			}
			if !reflect.DeepEqual(spec.Entrypoint, []string{"python3", "-m", "fixture.diffusion"}) {
				t.Fatalf("entrypoint=%v", spec.Entrypoint)
			}
		})
	}
}

func schedulePlacementResolverFixture(t *testing.T, selected string) proto.InstanceDesiredState {
	t.Helper()
	fixtureName := strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
	st, err := store.Open("file:controller_placement_"+fixtureName+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("placement-node-"+fixtureName, store.HashToken("token"), "placement-node")
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 8; index++ {
		if err := st.DB().Create(&store.GPU{NodeID: node.ID, Index: index, UUID: fmt.Sprintf("placement-gpu-%d", index), Name: "NVIDIA H100", Vendor: "nvidia", ArchID: "sm_90", ComputeCapability: "9.0", MemoryTotal: 32 << 30}).Error; err != nil {
			t.Fatal(err)
		}
	}
	model, err := st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "fixture/placement-" + fixtureName, Name: "placement", WeightBytes: 1 << 30, Meta: `{"model_type":"fixture","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":32,"num_key_value_heads":8,"max_position_embeddings":512}`})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/models/placement"}); err != nil {
		t.Fatal(err)
	}
	image, err := st.UpsertImage(&store.Image{Name: "placement", Engine: "sglang", Framework: "cuda", ImageRef: "lmsysorg/sglang:latest", Source: "official", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := json.Marshal(map[string]any{
		"entrypoint": []string{"python3", "-m", "fixture.diffusion"},
		"extra_args": []string{"--tp-size", "${TP_SIZE}", "--sp-degree", "${SP_DEGREE}"},
		"placement":  map[string]any{"language": "starlark", "resolver": "def resolve(gpu):\n    if gpu.count < 4:\n        return reject(\"need four\")\n    if gpu.count % 4 != 0:\n        return reject(\"multiple of four\")\n    if gpu.min_memory_gib < 32:\n        return reject(\"need 32 GiB\")\n    return {\"TP_SIZE\": gpu.count, \"SP_DEGREE\": 1}\n"},
	})
	if err != nil {
		t.Fatal(err)
	}
	dep, err := createTestDeployment(t, st, &store.Deployment{Name: "placement", ModelID: model.ID, NodeID: node.ID, ServedName: "placement", Engine: "sglang", Image: image.ImageRef, Placement: selected, QuickParams: `{"max_model_len":512}`, CatalogSelectionSnapshot: string(snapshot)})
	if err != nil {
		t.Fatal(err)
	}
	hub := &distributedTestHub{online: map[string]bool{node.UUID: true}, updates: map[string][]proto.StateUpdate{}}
	controller := New(st, hub, nil)
	if err := controller.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	instances, err := st.InstancesForDeployment(dep.ID)
	if err != nil || len(instances) != 1 {
		t.Fatalf("instances=%+v err=%v", instances, err)
	}
	var spec proto.InstanceDesiredState
	if err := json.Unmarshal([]byte(instances[0].DesiredStateJSON), &spec); err != nil {
		t.Fatal(err)
	}
	return spec
}

// scheduleFlatSpecFixture uses a model that cannot fit on one 80 GiB GPU but
// fits when the scheduler allocates two. The snapshot deliberately has no tp
// field: flat specs never persist placement topology.
func scheduleFlatSpecFixture(t *testing.T, snapshot map[string]any) proto.InstanceDesiredState {
	t.Helper()
	fixtureName := strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
	st, err := store.Open("file:controller_flat_spec_"+fixtureName+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("flat-spec-node-"+fixtureName, store.HashToken("token"), "flat-spec-node")
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 2; index++ {
		if err := st.DB().Create(&store.GPU{
			NodeID: node.ID, Index: index, UUID: "flat-spec-gpu-" + string(rune('a'+index)),
			Name: "NVIDIA H100", Vendor: "nvidia", ArchID: "sm_90", ComputeCapability: "9.0",
			MemoryTotal: 80 << 30,
		}).Error; err != nil {
			t.Fatal(err)
		}
	}
	model, err := st.UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "fixture/flat-spec-" + fixtureName, Revision: "main", Name: "flat-spec",
		WeightBytes: 100 << 30,
		Meta:        `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":2,"num_key_value_heads":2,"max_position_embeddings":512}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/models/flat-spec"}); err != nil {
		t.Fatal(err)
	}
	image, err := st.UpsertImage(&store.Image{Name: "flat spec", Engine: "vllm", Framework: "cuda", ImageRef: "catalog/flat-spec:v1", Source: "official", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	rawSnapshot, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	dep, err := createTestDeployment(t, st, &store.Deployment{
		Name: "flat-spec", ModelID: model.ID, NodeID: node.ID, ServedName: "flat-spec", Engine: "vllm", Image: image.ImageRef,
		QuickParams: `{"max_model_len":512}`, CatalogSelectionSnapshot: string(rawSnapshot),
	})
	if err != nil {
		t.Fatal(err)
	}
	hub := &distributedTestHub{online: map[string]bool{node.UUID: true}, updates: map[string][]proto.StateUpdate{}}
	ctrl := New(st, hub, nil)
	if err := ctrl.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	instances, err := st.InstancesForDeployment(dep.ID)
	if err != nil || len(instances) != 1 {
		t.Fatalf("instances=%+v err=%v", instances, err)
	}
	var spec proto.InstanceDesiredState
	if err := json.Unmarshal([]byte(instances[0].DesiredStateJSON), &spec); err != nil {
		t.Fatal(err)
	}
	return spec
}
