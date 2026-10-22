package deploy

import (
	"strings"
	"testing"
)

// SGLang's model hooks compare --speculative-algorithm against upper-case names
// before they normalize it (DeepSeek V4 asserts it is EAGLE or DSPARK), so the
// command must carry SGLang's own spelling however the value was written: in a
// catalog template, by a form, or in a configuration stored before the
// dictionary spelled it that way.
func TestSGLangSpeculativeAlgorithmReachesTheEngineInItsOwnSpelling(t *testing.T) {
	engine, _ := SelectEngine(EngineSGLang)
	for _, tc := range []struct{ written, want string }{
		{"DSPARK", "DSPARK"}, {"NEXTN", "NEXTN"}, {"EAGLE", "EAGLE"},
		{"dspark", "DSPARK"}, {"eagle3", "EAGLE3"}, {"Standalone", "STANDALONE"},
	} {
		raw, residual, err := ReverseArgs(EngineSGLang, []string{"--speculative-algorithm", tc.written})
		if err != nil || len(residual) != 0 || raw["speculative_algorithm"] != tc.want {
			t.Fatalf("%s: reverse args=%v residual=%v err=%v", tc.written, raw, residual, err)
		}
		out, err := Materialize(MaterializeInput{Engine: EngineSGLang, Raw: raw})
		if err != nil {
			t.Fatal(err)
		}
		if err := ValidateLaunchConfigForEngine(EngineSGLang, out.Quick); err != nil {
			t.Fatalf("%s: %v", tc.written, err)
		}
		built := engine.Build(Params{ModelPath: "/model", APIPort: 30000, Quick: out.Quick})
		if !argvHas(built.Entrypoint, "--speculative-algorithm", tc.want) {
			t.Fatalf("%s: argv %v lacks --speculative-algorithm %s", tc.written, built.Entrypoint, tc.want)
		}

		// A stored configuration that never went through Materialize.
		stored := engine.Build(Params{ModelPath: "/model", APIPort: 30000, Quick: LaunchConfig{SpeculativeAlgorithm: strings.ToLower(tc.want)}})
		if !argvHas(stored.Entrypoint, "--speculative-algorithm", tc.want) {
			t.Fatalf("stored %s: argv %v", strings.ToLower(tc.want), stored.Entrypoint)
		}
	}

	// vLLM's method names are not SGLang algorithms: SGLang calls MTP NEXTN and
	// a separate draft model STANDALONE, and would refuse either name.
	for _, name := range []string{"mtp", "draft_model"} {
		out, err := Materialize(MaterializeInput{Engine: EngineSGLang, Raw: map[string]any{"speculative_algorithm": name}})
		if err != nil {
			t.Fatal(err)
		}
		if err := ValidateLaunchConfigForEngine(EngineSGLang, out.Quick); err == nil {
			t.Fatalf("SGLang accepted the vLLM method name %q", name)
		}
	}
}

// vLLM keeps its own lower-case method names inside --speculative-config.
func TestVLLMSpeculativeMethodKeepsVLLMSpelling(t *testing.T) {
	out, err := Materialize(MaterializeInput{Engine: EngineVLLM, Raw: map[string]any{"speculative_algorithm": "MTP", "speculative_tokens": 3}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Quick.SpeculativeAlgorithm != "mtp" {
		t.Fatalf("speculative_algorithm = %q, want mtp", out.Quick.SpeculativeAlgorithm)
	}
	engine, _ := SelectEngine(EngineVLLM)
	built := engine.Build(Params{ModelPath: "/model", APIPort: 8000, Quick: out.Quick})
	if !strings.Contains(strings.Join(built.Argv, " "), `"method":"mtp"`) {
		t.Fatalf("argv %v lacks the mtp method", built.Argv)
	}
}
