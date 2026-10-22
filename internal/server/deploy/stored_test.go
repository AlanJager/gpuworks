package deploy

import (
	"encoding/json"
	"testing"
)

// A stored SGLang configuration written while the dictionary still offered
// vLLM's method names is renamed once, at the startup pass, to the algorithm
// SGLang knows; the preview and the launch then read that one stored value.
// The same word typed into the command is the user's and stays as typed, and
// vLLM, where the word is right, is left alone.
func TestNormalizeStoredLaunchConfigRenamesValuesPerEngine(t *testing.T) {
	sglang, _ := SelectEngine(EngineSGLang)
	for _, tc := range []struct{ stored, want string }{
		{"mtp", "NEXTN"}, {"draft_model", "STANDALONE"}, {"eagle3", "EAGLE3"},
	} {
		row := StoredLaunchConfig{
			Engine:          EngineSGLang,
			QuickParamsJSON: `{"speculative_algorithm":"` + tc.stored + `"}`,
			ExtraArgsText:   "--speculative-algorithm-note mtp",
		}
		first, changed, err := NormalizeStoredLaunchConfig(row)
		if err != nil {
			t.Fatal(err)
		}
		var quick LaunchConfig
		if err := json.Unmarshal([]byte(first.QuickParamsJSON), &quick); err != nil {
			t.Fatal(err)
		}
		if !changed || quick.SpeculativeAlgorithm != tc.want {
			t.Fatalf("%s: changed=%v stored=%q, want %q", tc.stored, changed, quick.SpeculativeAlgorithm, tc.want)
		}
		if err := ValidateLaunchConfigForEngine(EngineSGLang, quick); err != nil {
			t.Fatalf("%s: renamed row must validate: %v", tc.stored, err)
		}
		if first.ExtraArgsText != row.ExtraArgsText {
			t.Fatalf("%s: user arguments rewritten to %q", tc.stored, first.ExtraArgsText)
		}
		built := sglang.Build(Params{ModelPath: "/model", APIPort: 30000, Quick: quick})
		if !argvHas(built.Entrypoint, "--speculative-algorithm", tc.want) {
			t.Fatalf("%s: argv %v", tc.stored, built.Entrypoint)
		}
		second, changed, err := NormalizeStoredLaunchConfig(first)
		if err != nil || changed || second.QuickParamsJSON != first.QuickParamsJSON {
			t.Fatalf("%s: second pass changed=%v err=%v %q", tc.stored, changed, err, second.QuickParamsJSON)
		}
	}

	// A user-typed SGLang token is not a stored dictionary value: it stays in
	// the command exactly as written, for SGLang to judge.
	typed, _, err := NormalizeStoredLaunchConfig(StoredLaunchConfig{Engine: EngineSGLang, ExtraArgsText: "--speculative-algorithm mtp"})
	if err != nil {
		t.Fatal(err)
	}
	if typed.ExtraArgsText != "--speculative-algorithm mtp" {
		t.Fatalf("typed token rewritten: %q (quick %s)", typed.ExtraArgsText, typed.QuickParamsJSON)
	}

	vllm, _, err := NormalizeStoredLaunchConfig(StoredLaunchConfig{Engine: EngineVLLM, QuickParamsJSON: `{"speculative_algorithm":"mtp"}`})
	if err != nil {
		t.Fatal(err)
	}
	var quick LaunchConfig
	if err := json.Unmarshal([]byte(vllm.QuickParamsJSON), &quick); err != nil {
		t.Fatal(err)
	}
	if quick.SpeculativeAlgorithm != "mtp" {
		t.Fatalf("vLLM mtp renamed to %q", quick.SpeculativeAlgorithm)
	}
}
