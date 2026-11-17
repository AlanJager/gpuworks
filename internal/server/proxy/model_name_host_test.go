package proxy

import (
	"encoding/json"
	"os"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// A host-process engine launched without --served-model-name has its model
// path translated by the agent to the mount's host source, and registers that
// path as its model name. The gateway asks for that name. Desired states are
// written as a server before the served-name change stored them.
func TestFlaglessHostProcessLaunchIsAskedForByItsHostModelPath(t *testing.T) {
	const hostModel = "/Users/a/models/q"
	for _, test := range []struct {
		name, engine, desired string
		host                  bool
		want                  string
	}{
		{"vllm model flag", "vllm", `{"launch":{"entrypoint":["vllm","serve"],"argv":["--model","/model"],"model_mount":"/model"},"model_path":"/Users/a/models/q"}`, true, hostModel},
		{"vllm positional", "vllm", `{"launch":{"entrypoint":["vllm","serve","/model"],"argv":["--port","8000"],"model_mount":"/model"},"model_path":"/Users/a/models/q"}`, true, hostModel},
		{"sglang model path", "sglang", `{"launch":{"entrypoint":["python3","-m","sglang.launch_server"],"argv":["--model-path","/model"],"model_mount":"/model"},"model_path":"/Users/a/models/q"}`, true, hostModel},
		{"below the mount", "vllm", `{"launch":{"entrypoint":["vllm","serve","/model/sub"],"model_mount":"/model"},"model_path":"/Users/a/models/q"}`, true, hostModel + "/sub"},
		{"trailing slash", "vllm", `{"launch":{"entrypoint":["vllm","serve","/model/"],"model_mount":"/model"},"model_path":"/Users/a/models/q"}`, true, hostModel + "/"},
		{"manual script", "vllm", `{"launch":{"entrypoint":["/bin/sh"],"argv":["-c","set -a\nexec vllm serve --model /model --port 8000"],"model_mount":"/model"},"model_path":"/Users/a/models/q"}`, true, hostModel},
		{"mount fallback", "sglang", `{"launch":{"entrypoint":["sglang-custom"],"model_mount":"/model"},"model_path":"/Users/a/models/q"}`, true, hostModel},
		{"not under the mount", "vllm", `{"launch":{"entrypoint":["vllm","serve","/modelx"],"model_mount":"/model"},"model_path":"/Users/a/models/q"}`, true, "/modelx"},
		{"hub id", "vllm", `{"launch":{"entrypoint":["vllm","serve","Qwen/Qwen3-0.6B"],"model_mount":"/model"},"model_path":"/Users/a/models/q"}`, true, "Qwen/Qwen3-0.6B"},
		{"docker model flag", "vllm", `{"launch":{"entrypoint":["vllm","serve"],"argv":["--model","/model"],"model_mount":"/model"},"model_path":"/Users/a/models/q"}`, false, "/model"},
		{"docker positional", "vllm", `{"launch":{"entrypoint":["vllm","serve","/model"],"model_mount":"/model"},"model_path":"/Users/a/models/q"}`, false, "/model"},
		{"docker sglang", "sglang", `{"launch":{"entrypoint":["python3","-m","sglang.launch_server"],"argv":["--model-path","/model"],"model_mount":"/model"},"model_path":"/Users/a/models/q"}`, false, "/model"},
		{"docker manual script", "vllm", `{"launch":{"entrypoint":["/bin/sh"],"argv":["-c","set -a\nexec vllm serve --model /model --port 8000"],"model_mount":"/model"},"model_path":"/Users/a/models/q"}`, false, "/model"},
		{"host served name", "vllm", `{"launch":{"entrypoint":["vllm","serve","/model"],"argv":["--served-model-name","/model"],"model_mount":"/model"},"model_path":"/Users/a/models/q"}`, true, "/model"},
		{"host alias", "sglang", `{"launch":{"entrypoint":["python3","-m","sglang.launch_server"],"argv":["--model-path","/model","--served-model-name","qwen"],"model_mount":"/model"},"model_path":"/Users/a/models/q"}`, true, "qwen"},
		{"host shell variable", "vllm", `{"launch":{"entrypoint":["vllm","serve"],"argv":["--model","$MODEL"],"model_mount":"/model"},"model_path":"/Users/a/models/q"}`, true, ""},
		{"host without model path", "vllm", `{"launch":{"entrypoint":["vllm","serve","/model"],"model_mount":"/model"}}`, true, "/model"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := instanceModelName(&store.Deployment{Engine: test.engine, ServedName: "public"}, &store.Instance{DesiredStateJSON: test.desired}, test.host)
			if got != test.want {
				t.Fatalf("engine model=%q, want %q", got, test.want)
			}
		})
	}
}

// TestGatewayAsksHostEnginesForTheNameTheAgentGaveThem is the gateway side of
// the contract in internal/proto/testdata/hostproc_engine_names.json, whose
// agent side is tested in internal/agent/runtime/hostproc.
func TestGatewayAsksHostEnginesForTheNameTheAgentGaveThem(t *testing.T) {
	raw, err := os.ReadFile("../../proto/testdata/hostproc_engine_names.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		Cases []struct {
			Name       string          `json:"name"`
			Engine     string          `json:"engine"`
			Desired    json.RawMessage `json:"desired"`
			EngineName string          `json:"engine_name"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	if len(golden.Cases) == 0 {
		t.Fatal("no cases")
	}
	for _, c := range golden.Cases {
		t.Run(c.Name, func(t *testing.T) {
			got := instanceModelName(&store.Deployment{Engine: c.Engine, ServedName: "public"}, &store.Instance{DesiredStateJSON: string(c.Desired)}, true)
			if got != c.EngineName {
				t.Fatalf("gateway asks for %q, engine answers under %q", got, c.EngineName)
			}
		})
	}
}
