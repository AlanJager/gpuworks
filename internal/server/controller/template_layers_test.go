package controller

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestTemplateLayersReachFinalInstanceDesiredStateWithoutTokenLoss(t *testing.T) {
	st, err := store.Open("file:controller_template_layers?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	node, err := st.RegisterNode("template-layer-node", store.HashToken("token"), "template-layer-node")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Create(&store.GPU{
		NodeID: node.ID, Index: 0, UUID: "template-layer-gpu", Name: "NVIDIA H100",
		Vendor: "nvidia", ArchID: "sm_90", ComputeCapability: "9.0",
		MemoryTotal: 80 << 30,
	}).Error; err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "fixture/template-layers", Revision: "", Name: "template-layers",
		WeightBytes: 1 << 30,
		Meta:        `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":32768}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModelFile(&store.ModelFile{
		ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/models/template layers",
	}); err != nil {
		t.Fatal(err)
	}
	image, err := st.UpsertImage(&store.Image{
		Name: "template layers", Engine: "vllm", Framework: "cuda",
		ImageRef: "catalog/template-layers:v1", Source: "official", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	selectionDocument, err := json.Marshal(map[string]any{
		"image":            image.ImageRef,
		"extra_args":       []string{"--max-model-len", "4096", "--template-json", `{"note":"value with spaces"}`},
		"env":              map[string]string{"TEMPLATE_ONLY": "template value", "ORDER": "template"},
		"pre_start_argv":   []string{"vllm-hcu-apply-patches", "--safe"},
		"pre_start_script": "",
		"entrypoint":       []string{"python3", "-m", "fixture.server"},
		"runtime_assets": []proto.RuntimeAsset{{
			File: "assets/runtime.tar.gz", SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			SizeBytes: 7, UnpackTo: "/opt/gpuworks/app",
			PackageDigest: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			ModelID:       "fixture/template-layers", Content: []byte("legacy-bytes-must-not-persist"),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	dep, err := createTestDeployment(t, st, &store.Deployment{
		Name: "template-layers", ModelID: model.ID, NodeID: node.ID, ServedName: "template-layers",
		Engine: "vllm", Image: image.ImageRef, Placement: `{"tp":1,"gpus":[0]}`,
		QuickParams:              `{"max_model_len":8192}`,
		ExtraArgs:                `--max-model-len 16384 --chat-template "/model/chat templates/qwen.jinja"`,
		CatalogSelectionSnapshot: string(selectionDocument),
		ContainerOverrides:       `{"env":{"ORDER":"user","USER_ONLY":"user value"}}`,
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
	if got := instanceFlagValues(spec.Argv, "--max-model-len"); !reflect.DeepEqual(got, []string{"16384"}) {
		t.Fatalf("max-model-len values=%#v argv=%#v", got, spec.Argv)
	}
	if got := instanceFlagValues(spec.Argv, "--template-json"); !reflect.DeepEqual(got, []string{`{"note":"value with spaces"}`}) {
		t.Fatalf("template JSON values=%#v argv=%#v", got, spec.Argv)
	}
	if got := instanceFlagValues(spec.Argv, "--chat-template"); !reflect.DeepEqual(got, []string{"/model/chat templates/qwen.jinja"}) {
		t.Fatalf("user space values=%#v argv=%#v", got, spec.Argv)
	}
	if spec.Env["ORDER"] != "user" || spec.Env["TEMPLATE_ONLY"] != "template value" || spec.Env["USER_ONLY"] != "user value" {
		t.Fatalf("final env=%#v", spec.Env)
	}
	if !reflect.DeepEqual(spec.PreStartArgv, []string{"vllm-hcu-apply-patches", "--safe"}) || spec.PreStartScript != "" {
		t.Fatalf("pre-start fields were not retained in instance spec: %+v", spec)
	}
	if !reflect.DeepEqual(spec.Entrypoint, []string{"python3", "-m", "fixture.server"}) || len(spec.RuntimeAssets) != 1 || len(spec.RuntimeAssets[0].Content) != 0 || spec.RuntimeAssets[0].SizeBytes != 7 || spec.RuntimeAssets[0].PackageDigest == "" {
		t.Fatalf("entrypoint/runtime assets were not retained in instance spec: %+v", spec)
	}
	var schedule struct {
		Image struct {
			Ref string `json:"ref"`
		} `json:"image"`
	}
	if err := json.Unmarshal([]byte(instances[0].ScheduleExpl), &schedule); err != nil {
		t.Fatal(err)
	}
	// Launch no longer re-resolves the catalog; the instance runs the image
	// the deployment stored, which for a template-owned deployment is the
	// spec's pinned ref.
	if schedule.Image.Ref != image.ImageRef {
		t.Fatalf("launched image = %q, want the template's %q", schedule.Image.Ref, image.ImageRef)
	}
	updates := hub.updates[node.UUID]
	if len(updates) != 1 || len(updates[0].Instances) != 1 || !reflect.DeepEqual(updates[0].Instances[0].Argv, spec.Argv) || !reflect.DeepEqual(updates[0].Instances[0].Env, spec.Env) || !reflect.DeepEqual(updates[0].Instances[0].PreStartArgv, spec.PreStartArgv) || !reflect.DeepEqual(updates[0].Instances[0].RuntimeAssets, spec.RuntimeAssets) {
		t.Fatalf("pushed update does not match persisted spec: %+v", updates)
	}
}

func instanceFlagValues(args []string, flag string) []string {
	var values []string
	for index, token := range args {
		if token == flag && index+1 < len(args) {
			values = append(values, args[index+1])
		}
		if len(token) > len(flag)+1 && token[:len(flag)+1] == flag+"=" {
			values = append(values, token[len(flag)+1:])
		}
	}
	return values
}
