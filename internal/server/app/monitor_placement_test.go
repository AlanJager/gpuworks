package app

import (
	"encoding/json"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// launchInstance is a store instance whose desired state carries the given
// engine and argv, the way the controller stores a resolved launch.
func launchInstance(t *testing.T, engine string, argv ...string) store.Instance {
	t.Helper()
	spec := proto.InstanceDesiredState{ResolvedLaunch: proto.ResolvedLaunch{Argv: argv}, Engine: engine}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	return store.Instance{DesiredStateJSON: string(raw)}
}

// The tensor-parallel degree is whatever the launch commands state, read with
// the engine's own spelling; the platform never infers it from the card or
// node count, and does not guess when the instances disagree.
func TestDeploymentTensorParallelIsOnlyWhatLaunchCommandsState(t *testing.T) {
	cases := []struct {
		name      string
		engine    string
		instances []store.Instance
		want      int
	}{
		{"vllm states tp", "vllm", []store.Instance{launchInstance(t, "vllm", "--tensor-parallel-size", "4")}, 4},
		{"sglang spells it --tp", "sglang", []store.Instance{launchInstance(t, "sglang", "--tp", "8")}, 8},
		{"callable's flag is not the platform's to read", "callable", []store.Instance{launchInstance(t, "callable", "--tensor-parallel-size", "4")}, 0},
		{"no flag", "vllm", []store.Instance{launchInstance(t, "vllm", "--max-num-seqs", "256")}, 0},
		{"template placeholder is not a number", "vllm", []store.Instance{launchInstance(t, "vllm", "--tensor-parallel-size", "$TP_SIZE")}, 0},
		{"a rank that states none does not veto", "vllm", []store.Instance{
			launchInstance(t, "vllm", "--tensor-parallel-size=4"), launchInstance(t, "vllm", "--headless"),
		}, 4},
		{"instances stating different degrees have no single one", "vllm", []store.Instance{
			launchInstance(t, "vllm", "--tensor-parallel-size", "2"), launchInstance(t, "vllm", "--tensor-parallel-size", "4"),
		}, 0},
		{"unparseable desired state", "vllm", []store.Instance{{DesiredStateJSON: "{"}}, 0},
		{"no instances", "vllm", nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := deploymentTensorParallel(tc.engine, tc.instances); got != tc.want {
				t.Fatalf("tensor parallel = %d, want %d", got, tc.want)
			}
		})
	}
}

// The monitor's instance and deployment refs carry the rank facts and the
// stated degree through from the store rows.
func TestMonitorRefsCarryRankAndTensorParallel(t *testing.T) {
	inst := launchInstance(t, "vllm", "--tensor-parallel-size", "4")
	inst.UUID, inst.DistGroup, inst.NodeRank, inst.NNodes = "u1", "g1", 2, 4
	ref := instanceRef(&inst, nil)
	if ref.DistGroup != "g1" || ref.NodeRank != 2 || ref.NNodes != 4 {
		t.Fatalf("instance ref = %+v", ref)
	}
	dep := deploymentRef(&store.Deployment{Engine: "vllm", Replicas: 1}, []store.Instance{inst})
	if dep.TensorParallel != 4 {
		t.Fatalf("deployment tensor parallel = %d, want 4", dep.TensorParallel)
	}
}
