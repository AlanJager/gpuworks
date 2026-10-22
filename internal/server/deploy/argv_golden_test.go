package deploy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// argvGoldenDir holds one file per engine command that has to survive the move
// of flag emission onto the parameter dictionary byte for byte. Each file is
// the complete command a fixed configuration builds: the executable and its
// parameters, in order, as a JSON array.
const argvGoldenDir = "testdata/argv"

// argvGoldenUpdateEnv regenerates the goldens instead of comparing against them.
const argvGoldenUpdateEnv = "GPUWORKS_UPDATE_ARGV_GOLDEN"

// TestEngineCommandGoldens pins three representative commands per engine: one
// that states nothing, one that states every parameter the dictionary
// publishes for it, and one that states the grouped flags together with
// template and user arguments. The refactor that emits flags from the table
// instead of from the adapters has to reproduce them exactly.
func TestEngineCommandGoldens(t *testing.T) {
	for _, test := range argvGoldenCases(t) {
		t.Run(test.name, func(t *testing.T) {
			built := test.engine.Build(test.params)
			command := append(append([]string{}, built.Entrypoint...), built.Argv...)
			encoded, err := json.MarshalIndent(command, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			got := append(encoded, '\n')
			path := filepath.Join(argvGoldenDir, test.name+".golden")
			if os.Getenv(argvGoldenUpdateEnv) != "" {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden (regenerate with %s=1): %v", argvGoldenUpdateEnv, err)
			}
			if string(got) != string(want) {
				t.Fatalf("%s changed:\n%s", path, diffCommandLines(string(want), string(got)))
			}
		})
	}
}

// diffCommandLines keeps a golden mismatch readable: the tokens that differ,
// rather than two JSON blobs.
func diffCommandLines(want, got string) string {
	wantTokens := commandTokens(want)
	gotTokens := commandTokens(got)
	var out strings.Builder
	for index := 0; index < len(wantTokens) || index < len(gotTokens); index++ {
		var wantToken, gotToken string
		if index < len(wantTokens) {
			wantToken = wantTokens[index]
		}
		if index < len(gotTokens) {
			gotToken = gotTokens[index]
		}
		if wantToken == gotToken {
			continue
		}
		out.WriteString("  index " + strconv.Itoa(index) + ": want " + wantToken + ", got " + gotToken + "\n")
	}
	return out.String()
}

func commandTokens(encoded string) []string {
	var tokens []string
	_ = json.Unmarshal([]byte(encoded), &tokens)
	return tokens
}

type argvGoldenCase struct {
	name   string
	engine Engine
	params Params
}

func argvGoldenCases(t *testing.T) []argvGoldenCase {
	t.Helper()
	vllm, sglang := mustEngine(t, EngineVLLM), mustEngine(t, EngineSGLang)
	vllmUnit := goldenParams(LaunchConfig{})
	sglangUnit := goldenParams(LaunchConfig{})
	vllmStated := goldenParams(statedEveryKey(t, EngineVLLM))
	sglangStated := goldenParams(statedEveryKey(t, EngineSGLang))
	vllmComposed := goldenParams(statedKeys(t, EngineVLLM, map[string]any{
		"speculative_algorithm": "eagle3", "speculative_draft_model": "org/draft", "speculative_tokens": 3,
		"kv_disk_offload": true, "kv_offloading_size": 16, "shared_kv_dir": "/kv-cache",
		"kv_offload_read_threads": 8, "kv_offload_write_threads": 4, "optimization_level": "O3",
	}))
	vllmComposed.TemplateArgs = []string{"--served-model-name", "template-name"}
	vllmComposed.ExtraArgs = []string{"--enable-metrics"}
	sglangComposed := goldenParams(statedKeys(t, EngineSGLang, map[string]any{
		"speculative_algorithm": "eagle3", "speculative_draft_model": "org/draft",
		"speculative_num_steps": 3, "speculative_eagle_top_k": 4, "speculative_tokens": 5,
	}))
	sglangComposed.TemplateArgs = []string{"--load-format", "auto"}
	sglangComposed.ExtraArgs = []string{"--enable-metrics"}
	// Stating every boolean as false pins both the negative polarities
	// (--no-enable-chunked-prefill, --disable-radix-cache) and the rows where
	// false is the same as saying nothing at all. The memory KV tier rides
	// along on vLLM's case: it is the only path that emits
	// --kv-offloading-size, because the disk tier folds the same key into its
	// transfer config.
	vllmOffRaw := offRawBooleans(t, EngineVLLM)
	vllmOffRaw["kv_offloading_size"] = 8
	vllmOff := goldenParams(statedKeys(t, EngineVLLM, vllmOffRaw))
	sglangOff := goldenParams(statedKeys(t, EngineSGLang, offRawBooleans(t, EngineSGLang)))
	return []argvGoldenCase{
		{"vllm_default", vllm, vllmUnit},
		{"vllm_stated", vllm, vllmStated},
		{"vllm_composed", vllm, vllmComposed},
		{"vllm_off", vllm, vllmOff},
		{"vllm_ui_default", vllm, goldenParams(materializeRaw(t, EngineVLLM, nil))},
		{"sglang_default", sglang, sglangUnit},
		{"sglang_stated", sglang, sglangStated},
		{"sglang_composed", sglang, sglangComposed},
		{"sglang_off", sglang, sglangOff},
		{"sglang_ui_default", sglang, goldenParams(materializeRaw(t, EngineSGLang, nil))},
	}
}

// TestEmptyQuickParamsMatchesTheFormsDefaultPayload pins the contract that let
// the deploy form stop sending defaults: a request that states nothing
// materializes to the same command the form's old default payload produced.
// The old payload is rebuilt here from the dictionary (the controls that
// displayed a default also submitted it), and the two commands must match
// byte for byte for every engine.
func TestEmptyQuickParamsMatchesTheFormsDefaultPayload(t *testing.T) {
	for _, name := range []string{EngineVLLM, EngineSGLang} {
		engine := mustEngine(t, name)
		formPayload := map[string]any{}
		for key, spec := range engineParams(name) {
			if spec.Default == nil {
				continue
			}
			// The controls that showed a default submitted it: switches with a
			// boolean default, sliders with a number, and the log level.
			shownDefault := (spec.Type == "boolean" || spec.Control == "switch") && isBool(spec.Default)
			shownDefault = shownDefault || spec.Control == "slider" && isNumber(spec.Default)
			shownDefault = shownDefault || key == "log_level" && isString(spec.Default)
			if shownDefault {
				formPayload[key] = spec.Default
			}
		}
		fromForm := commandOf(engine, materializeRaw(t, name, formPayload))
		fromEmpty := commandOf(engine, materializeRaw(t, name, nil))
		if strings.Join(fromForm, "\x00") != strings.Join(fromEmpty, "\x00") {
			t.Fatalf("%s: an empty request must build the form's default command\nform:  %v\nempty: %v", name, fromForm, fromEmpty)
		}
	}
}

func isBool(value any) bool { _, ok := value.(bool); return ok }

func isNumber(value any) bool {
	switch value.(type) {
	case float64, float32, int, int64:
		return true
	}
	return false
}

func isString(value any) bool { _, ok := value.(string); return ok }

func commandOf(engine Engine, quick LaunchConfig) []string {
	built := engine.Build(goldenParams(quick))
	return append(append([]string{}, built.Entrypoint...), built.Argv...)
}

func mustEngine(t *testing.T, name string) Engine {
	t.Helper()
	engine, err := SelectEngine(name)
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

// goldenParams is the platform half of a launch: what the resolver and the
// scheduler set before the adapter adds engine parameters.
func goldenParams(quick LaunchConfig) Params {
	return Params{
		ModelPath: "/model", ServedName: "qwen3-8b", Host: "0.0.0.0",
		APIPort: 30000, AssignedGPUs: 1, TensorParallel: 1, Quick: quick,
	}
}

// statedEveryKey states every parameter the dictionary publishes for the
// engine, so the command exercises every flag the table can emit.
func statedEveryKey(t *testing.T, engine string) LaunchConfig {
	t.Helper()
	raw := map[string]any{}
	for key, spec := range engineParams(engine) {
		if spec.Platform {
			// A platform parameter has no flag by construction (ADR 0021).
			continue
		}
		raw[key] = exampleParameterValue(spec)
	}
	return materializeRaw(t, engine, raw)
}

// statedKeys states only the named keys, which is how the grouped-flag cases
// stay readable.
func statedKeys(t *testing.T, engine string, raw map[string]any) LaunchConfig {
	t.Helper()
	return materializeRaw(t, engine, raw)
}

// offRawBooleans states every boolean the engine publishes as false.
func offRawBooleans(t *testing.T, engine string) map[string]any {
	t.Helper()
	raw := map[string]any{}
	for key, spec := range engineParams(engine) {
		if spec.Platform || spec.Type != "boolean" {
			continue
		}
		raw[key] = false
	}
	return raw
}

func materializeRaw(t *testing.T, engine string, raw map[string]any) LaunchConfig {
	t.Helper()
	out, err := Materialize(MaterializeInput{Engine: engine, Raw: raw})
	if err != nil {
		t.Fatal(err)
	}
	return out.Quick
}

// exampleParameterValue is a value this key can hold that reaches the command:
// booleans are stated true (the coverage test states them false as well), an
// enum takes an option other than the row's default, and a number stays inside
// the bounds the row declares.
func exampleParameterValue(spec engineParam) any {
	switch spec.Type {
	case "boolean":
		return true
	case "integer", "number":
		value := 1.0
		if spec.Min != nil && *spec.Min > value {
			value = *spec.Min
		}
		if spec.Max != nil && *spec.Max < value {
			value = *spec.Max
		}
		if spec.Type == "integer" {
			return int64(value)
		}
		return value
	case "json":
		return `{"kind":"example"}`
	case "enum":
		if len(spec.Enum) > 1 {
			return spec.Enum[len(spec.Enum)-1]
		}
		if len(spec.Enum) == 1 {
			return spec.Enum[0]
		}
		return "example"
	default:
		return "example"
	}
}

// TestEveryDictionaryFlagReachesTheCommand is the coverage half of the
// contract: a row the adapter states has to reach argv, or the parameter is
// only decoration in the form. Only two things excuse a missing flag - a row
// the platform consumes itself (no flag by construction) and a spelling the
// adapter composes itself (flagPair.Composed: one flag carrying several keys,
// or a value glued into the token), whose emission the golden commands pin.
func TestEveryDictionaryFlagReachesTheCommand(t *testing.T) {
	for _, name := range []string{EngineVLLM, EngineSGLang} {
		engine := mustEngine(t, name)
		for _, stated := range []bool{true, false} {
			raw := map[string]any{}
			for key, spec := range engineParams(name) {
				if spec.Platform {
					continue
				}
				if spec.Type == "boolean" {
					raw[key] = stated
					continue
				}
				raw[key] = exampleParameterValue(spec)
			}
			built := engine.Build(goldenParams(materializeRaw(t, name, raw)))
			command := append(append([]string{}, built.Entrypoint...), built.Argv...)
			for key, spec := range engineParams(name) {
				if spec.Platform || spec.Composed {
					continue
				}
				flag := spec.Flag
				if spec.Type == "boolean" && !stated {
					flag = spec.FlagOff
				}
				if flag == "" {
					// The engine spells this polarity by saying nothing.
					continue
				}
				if !commandHasFlag(command, flag) {
					t.Errorf("%s: %s is stated but %s never reaches the command: %v", name, key, flag, command)
				}
			}
		}
	}
}

func commandHasFlag(command []string, flag string) bool {
	for _, token := range command {
		if token == flag || strings.HasPrefix(token, flag+"=") {
			return true
		}
	}
	return false
}

// TestComposedFlagsAppearInAGoldenCommand closes the escape hatch the coverage
// test above leaves open: that one skips composed rows, so a row could be
// marked composed to silence it. A composed row's adapter still has to write
// its flag, and every golden command is the place that proves it does.
func TestComposedFlagsAppearInAGoldenCommand(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join(argvGoldenDir, "*.golden"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatalf("no golden commands under %s", argvGoldenDir)
	}
	commands := map[string][][]string{}
	for _, path := range paths {
		engine := strings.SplitN(filepath.Base(path), "_", 2)[0]
		encoded, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		commands[engine] = append(commands[engine], commandTokens(string(encoded)))
	}
	for _, engine := range []string{EngineVLLM, EngineSGLang} {
		for key, spec := range engineParams(engine) {
			if !spec.Composed {
				continue
			}
			if spec.Flag == "" {
				t.Errorf("%s: %s is declared composed but owns no flag", engine, key)
				continue
			}
			found := false
			for _, command := range commands[engine] {
				if commandHasComposedFlag(command, spec.Flag) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("%s: %s is declared composed, but %s never appears in a golden command", engine, key, spec.Flag)
			}
		}
	}
}

// commandHasComposedFlag reports a composed spelling in either form an adapter
// writes it: a token of its own (--kv-transfer-config), or the value glued into
// the token (-O3). The second form is why a short option is also matched as a
// prefix; a long flag always stands alone.
func commandHasComposedFlag(command []string, flag string) bool {
	for _, token := range command {
		if commandHasFlag([]string{token}, flag) {
			return true
		}
		if !strings.HasPrefix(flag, "--") && strings.HasPrefix(token, flag) && len(token) > len(flag) && token[len(flag)] != '-' {
			return true
		}
	}
	return false
}
