package deploy

import (
	"slices"
	"strings"
	"testing"
)

// argvLintCase is one already-tokenized argv and everything the lint is
// expected to say about it. want lists Flag/Key/Reason triples in order.
type argvLintCase struct {
	name   string
	engine string
	args   []string
	want   []ArgProblem
}

// TestLintArgsReportsOnlyValuesTheDictionaryOwnsAndCannotRead covers both
// halves of the contract with argv written the way vLLM and SGLang document
// them: what the lint refuses, and the larger set of spellings it must leave
// alone because the dictionary has no claim on them.
func TestLintArgsReportsOnlyValuesTheDictionaryOwnsAndCannotRead(t *testing.T) {
	cases := []argvLintCase{
		{
			name:   "vllm launch the catalog actually ships",
			engine: EngineVLLM,
			args: []string{
				"--max-model-len", "32768",
				"--dtype", "half",
				"--gpu-memory-utilization", "0.92",
				"--tensor-parallel-size", "8",
				"--trust-remote-code",
				"--enable-prefix-caching",
				"--limit-mm-per-prompt", `{"image": 2, "video": 0}`,
				"--reasoning-parser", "qwen3",
				"-O", "O2",
			},
		},
		{
			name:   "sglang launch with an upstream-cased enum",
			engine: EngineSGLang,
			args: []string{
				"--context-length", "32768",
				"--mem-fraction-static", "0.88",
				"--tp-size", "4",
				"--speculative-algorithm", "EAGLE3",
				"--speculative-num-steps", "3",
				"--speculative-eagle-topk", "4",
				"--speculative-num-draft-tokens", "4",
				"--disable-radix-cache",
				"--enable-metrics",
			},
		},
		{
			name:   "flags no dictionary key owns stay the author's",
			engine: EngineVLLM,
			args:   []string{"--served-model-name", "deepseek", "--hf-overrides", "{not json}", "--compilation-config", "{\"level\": 3}"},
		},
		{
			name:   "flags several keys share carry no single type",
			engine: EngineVLLM,
			args: []string{
				"--speculative-config", "{not json at all}",
				"--kv-transfer-config", "{also not json}",
			},
		},
		{
			name:   "a boolean switch followed by another flag states its own value",
			engine: EngineVLLM,
			args:   []string{"--trust-remote-code", "--enforce-eager", "--async-scheduling", "--no-enable-prefix-caching"},
		},
		{
			name:   "a boolean at the end of argv needs no value",
			engine: EngineVLLM,
			args:   []string{"--max-model-len", "4096", "--enable-auto-tool-choice"},
		},
		{
			name:   "an engine with no parameter dictionary is not judged",
			engine: EngineCallable,
			args:   []string{"--max-model-len", "abc", "--dtype", "nonsense"},
		},
		{
			name:   "an integer key given a word",
			engine: EngineVLLM,
			args:   []string{"--max-model-len", "abc", "--dtype", "bfloat16"},
			want:   []ArgProblem{{Flag: "--max-model-len", Value: "abc", Key: "max_model_len", Reason: "is not an integer"}},
		},
		{
			name:   "an enum key given an option vLLM does not have",
			engine: EngineVLLM,
			args:   []string{"--kv-cache-dtype", "fp16"},
			want:   []ArgProblem{{Flag: "--kv-cache-dtype", Value: "fp16", Key: "kv_cache_dtype", Reason: "is not one of " + strings.Join(vllmKVCacheDtypes, ", ")}},
		},
		{
			name:   "the Ascend vLLM extension's INT8 cache is accepted",
			engine: EngineVLLM,
			args:   []string{"--kv-cache-dtype", "int8"},
		},
		{
			name:   "an enum key given an option this engine dropped",
			engine: EngineSGLang,
			args:   []string{"--kv-cache-dtype", "fp8"},
			want:   []ArgProblem{{Flag: "--kv-cache-dtype", Value: "fp8", Key: "kv_cache_dtype", Reason: "is not one of auto, bfloat16, fp8_e4m3, fp8_e5m2, mxfp8, nvfp4, fp4_mx_block16"}},
		},
		{
			name:   "an inline enum value no alias covers",
			engine: EngineVLLM,
			args:   []string{"--dtype=fp32"},
			want:   []ArgProblem{{Flag: "--dtype", Value: "fp32", Key: "dtype", Reason: "is not one of auto, float16, bfloat16, float32"}},
		},
		{
			name:   "json with an unquoted key, the shape that shipped in the catalog",
			engine: EngineVLLM,
			args:   []string{"--limit-mm-per-prompt", "{image:1}"},
			want:   []ArgProblem{{Flag: "--limit-mm-per-prompt", Value: "{image:1}", Key: "limit_mm_per_prompt", Reason: "is not valid JSON"}},
		},
		{
			name:   "a number key given a percentage",
			engine: EngineVLLM,
			args:   []string{"--gpu-memory-utilization", "92%"},
			want:   []ArgProblem{{Flag: "--gpu-memory-utilization", Value: "92%", Key: "gpu_memory_utilization", Reason: "is not a number"}},
		},
		{
			name:   "a value-taking flag at the end of argv has no value to read",
			engine: EngineSGLang,
			args:   []string{"--tp-size", "4", "--context-length"},
			want:   []ArgProblem{{Flag: "--context-length", Key: "max_model_len", Reason: "has no value"}},
		},
		{
			name:   "the flag swallowed as a value is still judged on its own terms",
			engine: EngineVLLM,
			args:   []string{"--max-model-len", "--dtype", "half"},
			want:   []ArgProblem{{Flag: "--max-model-len", Value: "--dtype", Key: "max_model_len", Reason: "is not an integer"}},
		},
		{
			name:   "every problem in one argv is reported, not just the first",
			engine: EngineVLLM,
			args:   []string{"--max-model-len", "abc", "--gpu-memory-utilization", "high", "--tensor-parallel-size", "2"},
			want: []ArgProblem{
				{Flag: "--max-model-len", Value: "abc", Key: "max_model_len", Reason: "is not an integer"},
				{Flag: "--gpu-memory-utilization", Value: "high", Key: "gpu_memory_utilization", Reason: "is not a number"},
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := LintArgs(testCase.engine, testCase.args)
			if len(got) != len(testCase.want) {
				t.Fatalf("problems = %v, want %v", got, testCase.want)
			}
			for index, want := range testCase.want {
				if got[index] != want {
					t.Fatalf("problem %d = %+v, want %+v", index, got[index], want)
				}
			}
			assertProblemsMatchReverseArgs(t, testCase.engine, testCase.args, got)
		})
	}
}

// assertProblemsMatchReverseArgs holds the lint to the reader it explains. A
// token the lint calls a problem is exactly a token ReverseArgs could not turn
// into a value and passed through instead; if the two ever walked the argv
// differently, the import error would name a flag the deployment does not
// actually mis-read.
func assertProblemsMatchReverseArgs(t *testing.T, engine string, args []string, problems []ArgProblem) {
	t.Helper()
	if engine == EngineCallable {
		return
	}
	params, residual, err := ReverseArgs(engine, args)
	if err != nil {
		t.Fatal(err)
	}
	for _, problem := range problems {
		found := false
		for _, token := range residual {
			if token == problem.Flag || strings.HasPrefix(token, problem.Flag+"=") {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s is a problem but ReverseArgs did not leave it in the residual %v", problem.Flag, residual)
		}
		if _, decoded := params[problem.Key]; decoded {
			t.Fatalf("%s is a problem but ReverseArgs still decoded %s", problem.Flag, problem.Key)
		}
	}
}

// TestLintArgsAcceptsEverySpellingTheDictionaryCanonicalizes states the other
// side of the enum rule: a value is not wrong for being spelled the way the
// engine's own documentation spells it.
func TestLintArgsAcceptsEverySpellingTheDictionaryCanonicalizes(t *testing.T) {
	for _, spelling := range []struct {
		engine    string
		args      []string
		key       string
		canonical any
	}{
		{EngineVLLM, []string{"--dtype", "half"}, "dtype", "float16"},
		{EngineVLLM, []string{"--dtype", "BFloat16"}, "dtype", "bfloat16"},
		{EngineSGLang, []string{"--speculative-algorithm", "eagle3"}, "speculative_algorithm", "EAGLE3"},
		{EngineVLLM, []string{"--kv-cache-dtype", "FP8_E5M2"}, "kv_cache_dtype", "fp8_e5m2"},
		{EngineVLLM, []string{"-O", "o3"}, "optimization_level", "O3"},
	} {
		if problems := LintArgs(spelling.engine, spelling.args); len(problems) != 0 {
			t.Errorf("%v on %s reported %v", spelling.args, spelling.engine, problems)
			continue
		}
		params, _, err := ReverseArgs(spelling.engine, spelling.args)
		if err != nil {
			t.Fatal(err)
		}
		if params[spelling.key] != spelling.canonical {
			t.Errorf("%v decoded %s = %#v, want %#v", spelling.args, spelling.key, params[spelling.key], spelling.canonical)
		}
	}
}

// TestEveryProblemIsATokenReverseArgsPassedThrough states over the whole
// dictionary what the per-case assertion above states for one argv: a problem
// is, by definition, a token the reader could not turn into a value. Since both
// answers now come out of one walk this holds by construction, and this test is
// what would notice if that walk were ever split in two again.
func TestEveryProblemIsATokenReverseArgsPassedThrough(t *testing.T) {
	values := []string{"", "not-a-value", "{", "-1", "0.5", "auto"}
	seen := 0
	for _, engine := range []string{EngineVLLM, EngineSGLang} {
		for key, spec := range engineParams(engine) {
			flag := spec.Flag
			if flag == "" {
				flag = spec.FlagOff
			}
			for _, value := range values {
				for _, trailing := range [][]string{nil, {"--served-model-name", "probe"}} {
					args := []string{flag}
					if value != "" {
						args = append(args, value)
					}
					args = append(args, trailing...)
					params, residual, err := ReverseArgs(engine, args)
					if err != nil {
						t.Fatal(err)
					}
					for _, problem := range LintArgs(engine, args) {
						seen++
						if !slices.Contains(residual, problem.Flag) {
							t.Fatalf("%s %v: %s is a problem but the residual is %v", engine, args, problem.Flag, residual)
						}
						if _, decoded := params[problem.Key]; decoded {
							t.Fatalf("%s %v: %s is a problem but %s decoded to %#v", engine, args, problem.Flag, problem.Key, params[problem.Key])
						}
						if problem.Key != key {
							t.Fatalf("%s %v: problem names key %s, want %s", engine, args, problem.Key, key)
						}
						if problem.Reason == "" {
							t.Fatalf("%s %v: problem on %s has no reason", engine, args, problem.Flag)
						}
					}
				}
			}
		}
	}
	// A sweep that reported nothing would satisfy the invariant vacuously.
	t.Logf("invariant held over %d reported problems", seen)
	if seen < 100 {
		t.Fatalf("sweep reported only %d problems, too few to hold the invariant to anything", seen)
	}
}
