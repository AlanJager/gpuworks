package controller

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The image catalog row is an acquisition record only (ADR 0031): a custom
// image registered in the catalog contributes nothing to the launch, which the
// engine, the template snapshot and the deployment's overrides fully decide.
func TestDeploymentLaunchIsDecidedWithoutTheImageCatalogRow(t *testing.T) {
	st, err := store.Open("file:controller_explicit_launch?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("explicit-launch-node", store.HashToken("token"), "explicit-launch-node")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Create(&store.GPU{NodeID: node.ID, Index: 0, UUID: "explicit-launch-gpu", Name: "NVIDIA H100", Vendor: "nvidia", ArchID: "sm_90", ComputeCapability: "9.0", MemoryTotal: 80 << 30}).Error; err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "example/explicit", Name: "explicit", WeightBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/models/explicit"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertImage(&store.Image{ImageRef: "private.example/sglang:custom", Source: "custom", Engine: "sglang", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	dep, err := createTestDeployment(t, st, &store.Deployment{
		Name: "explicit-launch", ModelID: model.ID, NodeID: node.ID, ServedName: "explicit", Engine: "sglang", Image: "private.example/sglang:custom", State: "active",
		QuickParams: `{"log_level":"error"}`, CatalogSelectionSnapshot: `{"pre_start_script":"echo template-preparation","env":{"TEMPLATE_ENV":"yes"}}`,
		ContainerOverrides: `{"env":{"USER_ENV":"yes"},"extra_mounts":[{"container_path":"/work","host_path":"/work"}],"shm_bytes":34359738368}`,
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
	var actual proto.InstanceDesiredState
	if err := json.Unmarshal([]byte(instances[0].DesiredStateJSON), &actual); err != nil {
		t.Fatal(err)
	}
	if actual.CommandMode != "engine" || actual.Env["TEMPLATE_ENV"] != "yes" || actual.Env["USER_ENV"] != "yes" || actual.Env["SGLANG_LOGGING_CONFIG_PATH"] == "" || len(actual.Mounts) != 1 || len(actual.DockerArgs) != 0 || actual.ShmBytes != 32<<30 {
		t.Fatalf("instance launch = %+v", actual.ResolvedLaunch)
	}
	if !strings.Contains(actual.PreStartScript, "echo template-preparation") || !strings.Contains(actual.PreStartScript, `"level": "ERROR"`) {
		t.Fatalf("engine and template preparation did not compose: %s", actual.PreStartScript)
	}
	// The same ResolveLaunch contract predicts the command, environment,
	// preparation and mounts stored in the immutable desired state.
	engine, _ := deploy.SelectEngine("sglang")
	expected, err := deploy.ResolveLaunch(deploy.ResolveLaunchInput{
		Engine: engine, Params: deploy.Params{ModelPath: "/model", ServedName: "explicit", APIPort: actual.APIPort, AssignedGPUs: 1, TensorParallel: 1, Quick: deploy.LaunchConfig{LogLevel: "error"}, TemplateEnv: map[string]string{"TEMPLATE_ENV": "yes"}, Arch: "Hopper", ArchID: "sm_90", ComputeCapability: "9.0", Vendor: "nvidia", NNodes: 1},
		ContainerOverrides: deploy.ContainerOverrides{Env: map[string]string{"USER_ENV": "yes"}, ExtraMounts: []deploy.ContainerMount{{ContainerPath: "/work", HostPath: "/work"}}, ShmBytes: 32 << 30},
		PreStartScript:     "echo template-preparation",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actual.Entrypoint, expected.Entrypoint) || !reflect.DeepEqual(actual.Argv, expected.Argv) ||
		actual.PreStartScript != expected.PreStartScript || !reflect.DeepEqual(actual.Env, expected.Env) ||
		!reflect.DeepEqual(actual.Mounts, expected.Mounts) || !reflect.DeepEqual(actual.DockerArgs, expected.DockerArgs) || actual.ShmBytes != expected.ShmBytes {
		t.Fatalf("resolved launch diverged: actual=%+v expected=%+v", actual.ResolvedLaunch, expected)
	}
}

// Every deployment is interpreted the same way at scheduling: engine mode
// unless the deployment chose otherwise. A launch_contract key left in a
// snapshot written before ADR 0031 changes nothing.
func TestLaunchModesAtScheduling(t *testing.T) {
	for _, tc := range []struct {
		name, snapshot, overrides, wantMode, wantEntry string
		wantShm                                        int64
	}{
		{name: "engine by default", wantMode: "engine", wantEntry: "vllm", wantShm: 16 << 30},
		{name: "retired marker", snapshot: `{"launch_contract":"explicit"}`, wantMode: "engine", wantEntry: "vllm", wantShm: 16 << 30},
		{name: "image entry with the image entrypoint", overrides: `{"command_mode":"image-entry"}`, wantMode: "image-entry", wantEntry: ""},
		{name: "image entry with a stated entrypoint", overrides: `{"command_mode":"image-entry","entrypoint_display":"[\"/custom-entry\"]"}`, wantMode: "image-entry", wantEntry: "/custom-entry"},
		{name: "manual", overrides: `{"command_mode":"manual","cmd_display":"vllm serve /model --port 30000","shm_bytes":8589934592}`, wantMode: "manual", wantEntry: "/bin/sh", wantShm: 8 << 30},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := store.Open("file:launch_modes_"+strings.ReplaceAll(tc.name, " ", "_")+"?mode=memory&cache=shared", bus.New())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			node, err := st.RegisterNode("launch-mode-node", store.HashToken("token"), "launch-mode-node")
			if err != nil {
				t.Fatal(err)
			}
			if err := st.DB().Create(&store.GPU{NodeID: node.ID, Index: 0, UUID: "launch-mode-gpu", Name: "NVIDIA H100", Vendor: "nvidia", MemoryTotal: 80 << 30}).Error; err != nil {
				t.Fatal(err)
			}
			model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "example/mode", Name: "mode", WeightBytes: 1 << 30})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/models/mode"}); err != nil {
				t.Fatal(err)
			}
			if _, err := st.UpsertImage(&store.Image{ImageRef: "example/mode:v1", Source: "custom", Engine: "vllm", Enabled: true}); err != nil {
				t.Fatal(err)
			}
			dep, err := createTestDeployment(t, st, &store.Deployment{ModelID: model.ID, NodeID: node.ID, ServedName: "mode", Engine: "vllm", Image: "example/mode:v1", State: "active", CatalogSelectionSnapshot: tc.snapshot, ContainerOverrides: tc.overrides})
			if err != nil {
				t.Fatal(err)
			}
			hub := &distributedTestHub{online: map[string]bool{node.UUID: true}, updates: map[string][]proto.StateUpdate{}}
			if err := New(st, hub, nil).reconcileDeployment(context.Background(), dep.ID); err != nil {
				t.Fatal(err)
			}
			instances, err := st.InstancesForDeployment(dep.ID)
			if err != nil || len(instances) != 1 {
				t.Fatalf("instances=%+v err=%v", instances, err)
			}
			var launch proto.InstanceDesiredState
			if err := json.Unmarshal([]byte(instances[0].DesiredStateJSON), &launch); err != nil {
				t.Fatal(err)
			}
			entry := ""
			if len(launch.Entrypoint) > 0 {
				entry = launch.Entrypoint[0]
			}
			if launch.CommandMode != tc.wantMode || entry != tc.wantEntry {
				t.Fatalf("launch mode=%q entrypoint=%v, want %q %q", launch.CommandMode, launch.Entrypoint, tc.wantMode, tc.wantEntry)
			}
			if tc.wantShm != 0 && launch.ShmBytes != tc.wantShm {
				t.Fatalf("shm = %d, want %d", launch.ShmBytes, tc.wantShm)
			}
		})
	}
}
