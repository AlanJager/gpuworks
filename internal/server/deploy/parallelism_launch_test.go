package deploy

import (
	"strings"
	"testing"
)

// TP and PP are the deployment's degrees: each engine is handed them as is,
// and the topology check measures TP×PP against every node's GPUs.
func TestLaunchCarriesGlobalParallelism(t *testing.T) {
	for _, tc := range []struct {
		name, engine string
		params       Params
		want         string
	}{
		{"vLLM TP spans two one-GPU nodes", EngineVLLM,
			Params{AssignedGPUs: 1, TensorParallel: 2, NNodes: 2, NodeRank: 0, MasterAddr: "10.0.0.1", MasterPort: 29500},
			"--tensor-parallel-size 2 --distributed-executor-backend mp --nnodes 2 --node-rank 0"},
		{"vLLM TP and PP over two nodes", EngineVLLM,
			Params{AssignedGPUs: 4, TensorParallel: 4, PipelineParallel: 2, NNodes: 2, NodeRank: 1, Headless: true, MasterAddr: "10.0.0.1", MasterPort: 29500},
			"--tensor-parallel-size 4 --pipeline-parallel-size 2 --distributed-executor-backend mp --nnodes 2 --node-rank 1 --headless"},
		{"vLLM single-node PP", EngineVLLM,
			Params{AssignedGPUs: 4, TensorParallel: 2, PipelineParallel: 2},
			"--tensor-parallel-size 2 --pipeline-parallel-size 2"},
		{"SGLang TP spans two one-GPU nodes", EngineSGLang,
			Params{AssignedGPUs: 1, TensorParallel: 2, NNodes: 2, NodeRank: 0, MasterAddr: "10.0.0.1", MasterPort: 29500},
			"--tp-size 2 --host"},
		{"SGLang TP and PP over two nodes", EngineSGLang,
			Params{AssignedGPUs: 4, TensorParallel: 4, PipelineParallel: 2, NNodes: 2, NodeRank: 1, Headless: true, MasterAddr: "10.0.0.1", MasterPort: 29500},
			"--tp-size 4 --pp-size 2 --host"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eng, err := SelectEngine(tc.engine)
			if err != nil {
				t.Fatal(err)
			}
			params := tc.params
			params.ModelPath = "/model"
			launch, err := ResolveLaunch(ResolveLaunchInput{Engine: eng, Params: params})
			if err != nil {
				t.Fatalf("ResolveLaunch: %v", err)
			}
			command := strings.Join(append(append([]string{}, launch.Entrypoint...), launch.Argv...), " ")
			if !strings.Contains(command, tc.want) {
				t.Fatalf("command = %q, want it to contain %q", command, tc.want)
			}
		})
	}
}

func TestLaunchRejectsParallelismTheNodesCannotHold(t *testing.T) {
	for _, engineName := range []string{EngineVLLM, EngineSGLang} {
		eng, _ := SelectEngine(engineName)
		// TP 4 over two nodes is two GPUs each, not one.
		_, err := ResolveLaunch(ResolveLaunchInput{Engine: eng, Params: Params{ModelPath: "/model", AssignedGPUs: 1, TensorParallel: 4, NNodes: 2}})
		if err == nil || !strings.Contains(err.Error(), "requires 4 GPUs, but placement assigned 2") {
			t.Fatalf("%s: err = %v", engineName, err)
		}
	}
}

// A pipeline degree the user states under any SGLang alias wins, and the
// platform does not add its own beside it.
func TestSGLangUserPipelineAliasIsNotDuplicated(t *testing.T) {
	eng, _ := SelectEngine(EngineSGLang)
	launch := eng.Build(Params{ModelPath: "/model", AssignedGPUs: 4, TensorParallel: 2, PipelineParallel: 2, ExtraArgs: []string{"--pp", "2"}})
	command := strings.Join(launch.Entrypoint, " ")
	if strings.Contains(command, "--pp-size") || strings.Count(command, "--pp ") != 1 {
		t.Fatalf("command = %q", command)
	}
}
