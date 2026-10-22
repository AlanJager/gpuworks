package controller

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// TestTemplateExtraArgsTextKeepsQuotedJSONValues pins the deploy-form contract
// for speculative decoding: the wizard prefills the extra-args box with the
// template's remaining arguments, single-quoting JSON values, and the server
// must reparse that text into the exact argv tokens. An unquoted
// --speculative-config value once lost its double quotes in the reparse and
// vLLM rejected the launch with "cannot be converted to json.loads".
func TestTemplateExtraArgsTextKeepsQuotedJSONValues(t *testing.T) {
	fixtureName := strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
	st, err := store.Open("file:controller_extra_args_json_"+fixtureName+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("extra-args-json-node", store.HashToken("token"), "extra-args-json-node")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Create(&store.GPU{NodeID: node.ID, Index: 0, UUID: "extra-args-json-gpu", Name: "NVIDIA RTX PRO 6000", Vendor: "nvidia", ArchID: "sm_120", ComputeCapability: "12.0", MemoryTotal: 96 << 30}).Error; err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "RedHatAI/gemma-4-26B-A4B-it-NVFP4", Revision: "main", Name: "gemma-nvfp4",
		WeightBytes: 16 << 30,
		Meta:        `{"model_type":"gemma4","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":2,"num_key_value_heads":2,"max_position_embeddings":512}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	draft, err := st.UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "google/gemma-4-26B-A4B-it-assistant", Revision: "main", Name: "gemma-draft",
		WeightBytes: 1 << 30,
		Meta:        `{"model_type":"gemma4","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":2,"num_key_value_heads":2,"max_position_embeddings":512}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/models/target"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: draft.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/models/draft"}); err != nil {
		t.Fatal(err)
	}
	image, err := st.UpsertImage(&store.Image{Name: "vllm spec", Engine: "vllm", Framework: "cuda", ImageRef: "vllm/vllm-openai:latest", Source: "official", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	// The text the deploy wizard submits for the gemma speculative-draft spec
	// after moving the parsers into structured form values.
	extraArgsText := "--load-format\nfastsafetensors\n--speculative-config\n'" + `{"model":"/models/draft","num_speculative_tokens":4}` + "'\n--enable-auto-tool-choice\n--chat-template\nexamples/tool_chat_template_gemma4.jinja"
	snapshot, err := json.Marshal(map[string]any{
		"extra_args": []string{
			"--load-format", "fastsafetensors",
			"--speculative-config", `{"model":"/models/draft","num_speculative_tokens":4}`,
			"--enable-auto-tool-choice", "--tool-call-parser", "gemma4",
			"--chat-template", "examples/tool_chat_template_gemma4.jinja",
			"--reasoning-parser", "gemma4",
		},
		"components": []map[string]any{
			{"name": "target", "role": "primary", "store_model_id": model.ID, "mount_path": "/models/target"},
			{"name": "draft", "role": "speculative_draft", "store_model_id": draft.ID, "mount_path": "/models/draft"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	dep, err := createTestDeployment(t, st, &store.Deployment{
		Name: "extra-args-json", ModelID: model.ID, NodeID: node.ID, ServedName: "gemma", Engine: "vllm", Image: image.ImageRef,
		QuickParams:              `{"reasoning_parser":"gemma4","tool_call_parser":"gemma4"}`,
		ExtraArgs:                extraArgsText,
		CatalogSelectionSnapshot: string(snapshot),
		TemplateID:               1, TemplateSpecName: "nvfp4-nvidia-nvfp4-vllm-nvidia-rtx-pro-6000-speculative-draft",
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
	want := `{"model":"/models/draft","num_speculative_tokens":4}`
	for index, token := range spec.Argv {
		if token == "--speculative-config" {
			if index+1 >= len(spec.Argv) || spec.Argv[index+1] != want {
				t.Fatalf("argv=%#v: --speculative-config value %#v, want %q", spec.Argv, spec.Argv[index+1:index+2], want)
			}
			return
		}
	}
	t.Fatalf("argv=%#v has no --speculative-config flag", spec.Argv)
	if strings.Contains(strings.Join(spec.Argv, " "), "{model:") {
		t.Fatalf("argv=%#v contains the quote-stripped value", spec.Argv)
	}
}

// TestTemplateExtraArgsEmptyTextLeavesTemplateLayerIntact is the control case:
// an empty extra-args box must not erase the snapshot's template arguments.
func TestTemplateExtraArgsEmptyTextLeavesTemplateLayerIntact(t *testing.T) {
	fixtureName := strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
	st, err := store.Open("file:controller_extra_args_empty_"+fixtureName+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("extra-args-empty-node", store.HashToken("token"), "extra-args-empty-node")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Create(&store.GPU{NodeID: node.ID, Index: 0, UUID: "extra-args-empty-gpu", Name: "NVIDIA H100", Vendor: "nvidia", ArchID: "sm_90", ComputeCapability: "9.0", MemoryTotal: 80 << 30}).Error; err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "fixture/extra-args-empty", Revision: "main", Name: "extra-args-empty",
		WeightBytes: 1 << 30,
		Meta:        `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":64,"num_attention_heads":2,"num_key_value_heads":1,"max_position_embeddings":512}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/models/target"}); err != nil {
		t.Fatal(err)
	}
	image, err := st.UpsertImage(&store.Image{Name: "vllm empty", Engine: "vllm", Framework: "cuda", ImageRef: "vllm/vllm-openai:latest", Source: "official", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := json.Marshal(map[string]any{
		"extra_args": []string{"--speculative-config", `{"model":"/models/draft","num_speculative_tokens":4}`},
	})
	if err != nil {
		t.Fatal(err)
	}
	dep, err := createTestDeployment(t, st, &store.Deployment{
		Name: "extra-args-empty", ModelID: model.ID, NodeID: node.ID, ServedName: "empty", Engine: "vllm", Image: image.ImageRef,
		CatalogSelectionSnapshot: string(snapshot),
		TemplateID:               1, TemplateSpecName: "speculative-draft",
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
	want := []string{"--speculative-config", `{"model":"/models/draft","num_speculative_tokens":4}`}
	if !reflect.DeepEqual(spec.Argv[len(spec.Argv)-len(want):], want) {
		t.Fatalf("argv tail=%#v, want %#v", spec.Argv, want)
	}
}
