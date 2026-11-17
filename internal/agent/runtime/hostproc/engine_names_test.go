package hostproc

import (
	"encoding/json"
	"os"
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	oci "gpuworks.ai/gpuworks/internal/agent/runtime/udocker/native"
	"gpuworks.ai/gpuworks/internal/proto"
)

// engineNameCases is the contract shared with the gateway: the argv the host
// agent runs for a stored launch, and the model name that engine answers
// under. The proxy package reads the same file.
type engineNameCases struct {
	Cases []struct {
		Name       string                     `json:"name"`
		Desired    proto.InstanceDesiredState `json:"desired"`
		HostArgs   []string                   `json:"host_args"`
		EngineName string                     `json:"engine_name"`
	} `json:"cases"`
}

// TestHostLaunchRunsTheEngineUnderTheGatewaysName pushes each stored launch
// through planLaunch the way the engine manager builds the workload (the model
// mounted from ModelPath at ModelMount) and checks the engine is handed the
// name the gateway asks for.
func TestHostLaunchRunsTheEngineUnderTheGatewaysName(t *testing.T) {
	raw, err := os.ReadFile("../../../proto/testdata/hostproc_engine_names.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden engineNameCases
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	if len(golden.Cases) == 0 {
		t.Fatal("no cases")
	}
	exists := fakeFS("/env/ROOT/bin", "/env/ROOT/bin/vllm", "/env/ROOT/bin/python3")
	for _, c := range golden.Cases {
		t.Run(c.Name, func(t *testing.T) {
			d := c.Desired
			spec := runtime.WorkloadSpec{Entrypoint: d.Entrypoint, Cmd: d.Argv}
			for _, m := range d.Mounts {
				spec.Mounts = append(spec.Mounts, runtime.Mount{Source: m.Source, Target: m.Target, ReadOnly: m.ReadOnly})
			}
			spec.Mounts = append(spec.Mounts, runtime.Mount{Source: d.ModelPath, Target: d.ModelMount, ReadOnly: true})
			got := plan(t, oci.ImageConfig{Env: []string{"PATH=/bin"}}, spec, exists)
			if !slices.Equal(got.Args, c.HostArgs) {
				t.Fatalf("Args = %q, want %q", got.Args, c.HostArgs)
			}
			if !argsName(got.Args, c.EngineName) {
				t.Fatalf("engine name %q is not a model argument of %q", c.EngineName, got.Args)
			}
		})
	}
}

// argsName reports whether name is what args hand the engine as its model or
// served name: the token after one of those flags, the positional model after
// "serve", or the same word inside a shell script.
func argsName(args []string, name string) bool {
	for i, tok := range args {
		switch tok {
		case "--served-model-name":
			return i+1 < len(args) && args[i+1] == name
		}
	}
	for i, tok := range args {
		switch tok {
		case "--model", "--model-path", "serve":
			if i+1 < len(args) && args[i+1] == name {
				return true
			}
		}
		if i > 0 && args[i-1] == "-c" {
			return argsName(splitWords(tok), name)
		}
	}
	return false
}

func splitWords(s string) []string {
	var out []string
	word := []rune{}
	for _, r := range s {
		if r == ' ' || r == '\n' || r == '\t' {
			if len(word) > 0 {
				out = append(out, string(word))
				word = word[:0]
			}
			continue
		}
		word = append(word, r)
	}
	if len(word) > 0 {
		out = append(out, string(word))
	}
	return out
}
