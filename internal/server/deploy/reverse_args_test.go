package deploy

import (
	"reflect"
	"testing"
)

func TestReverseArgsUsesParamSpecsAndPreservesPassthrough(t *testing.T) {
	args := []string{
		"--max-model-len=32768",
		"--reasoning-parser", "qwen3",
		"--limit-mm-per-prompt", `'{"image": 2, "audio": 1}'`,
		"--allowed-local-media-path", "/model/media files",
		"--trust-remote-code",
		"--speculative-config", `'{"method":"eagle", "num_speculative_tokens":4}'`,
		"--upstream-only", "value with spaces",
	}
	params, extra, err := ReverseArgs(EngineVLLM, args)
	if err != nil {
		t.Fatal(err)
	}
	if params["max_model_len"] != int64(32768) || params["reasoning_parser"] != "qwen3" || params["allowed_local_media_path"] != "/model/media files" || params["trust_remote_code"] != true {
		t.Fatalf("structured params = %#v", params)
	}
	wantJSON := map[string]any{"image": float64(2), "audio": float64(1)}
	if !reflect.DeepEqual(params["limit_mm_per_prompt"], wantJSON) {
		t.Fatalf("JSON param = %#v, want %#v", params["limit_mm_per_prompt"], wantJSON)
	}
	wantExtra := []string{
		"--speculative-config", `'{"method":"eagle", "num_speculative_tokens":4}'`,
		"--upstream-only", "value with spaces",
	}
	if !reflect.DeepEqual(extra, wantExtra) {
		t.Fatalf("extra args = %#v, want %#v", extra, wantExtra)
	}
}

func TestReverseArgsMalformedKnownValueRemainsLossless(t *testing.T) {
	args := []string{"--max-model-len", "not-a-number", "--unknown=value"}
	params, extra, err := ReverseArgs(EngineVLLM, args)
	if err != nil {
		t.Fatal(err)
	}
	if len(params) != 0 || !reflect.DeepEqual(extra, args) {
		t.Fatalf("params=%#v extra=%#v", params, extra)
	}
}

// An enum value the engine does not accept is not decodable either: it stays a
// passthrough token so the argv the author wrote still reaches the engine, and
// so catalog validation can see a dictionary flag sitting in the residual.
func TestReverseArgsKeepsUnknownEnumValueInResidual(t *testing.T) {
	params, extra, err := ReverseArgs(EngineVLLM, []string{"--dtype", "int4"})
	if err != nil {
		t.Fatal(err)
	}
	if len(params) != 0 || !reflect.DeepEqual(extra, []string{"--dtype", "int4"}) {
		t.Fatalf("params=%#v extra=%#v", params, extra)
	}
}

func TestReverseArgsHandlesSGLangNegativeBooleanFlag(t *testing.T) {
	params, extra, err := ReverseArgs(EngineSGLang, []string{"--disable-radix-cache"})
	if err != nil {
		t.Fatal(err)
	}
	if len(extra) != 0 || params["enable_prefix_caching"] != false {
		t.Fatalf("params=%#v extra=%#v", params, extra)
	}
}

func TestReverseArgsRejectsUnknownEngine(t *testing.T) {
	if _, _, err := ReverseArgs("not-an-engine", []string{"--dtype", "auto"}); err == nil {
		t.Fatal("an unknown engine has no dictionary and must be an error")
	}
}

func TestFormatArgsTextRoundTripsThroughParseArgsText(t *testing.T) {
	for name, args := range map[string][]string{
		"plain":       {"--foo", "bar"},
		"json value":  {"--compilation-config", `{"mode":0,"cudagraph_mode":"FULL"}`},
		"spaces":      {"--chat-template", "/model/chat templates/qwen.jinja"},
		"apostrophe":  {"--served-model-name", "public'model"},
		"empty token": {"--flag", ""},
	} {
		t.Run(name, func(t *testing.T) {
			text := FormatArgsText(args)
			parsed, err := ParseArgsText(text)
			if err != nil {
				t.Fatalf("ParseArgsText(%q): %v", text, err)
			}
			if !reflect.DeepEqual(parsed, args) {
				t.Fatalf("round trip of %#v through %q gave %#v", args, text, parsed)
			}
		})
	}
	if FormatArgsText(nil) != "" {
		t.Fatalf("no arguments must format as empty text, got %q", FormatArgsText(nil))
	}
}

// vLLM accepts auto|half|float16|bfloat16|float|float32 for --dtype. The
// dictionary keeps one spelling per meaning, so both of its aliases decode to
// the canonical value the configuration stores and the estimator reads.
func TestDtypeAliasesDecodeToTheCanonicalValue(t *testing.T) {
	for alias, want := range map[string]string{
		"half":     "float16",
		"float":    "float32",
		"float16":  "float16",
		"bfloat16": "bfloat16",
		"auto":     "auto",
	} {
		t.Run(alias, func(t *testing.T) {
			params, residual, err := ReverseArgs(EngineVLLM, []string{"--dtype", alias})
			if err != nil {
				t.Fatal(err)
			}
			if params["dtype"] != want || len(residual) != 0 {
				t.Fatalf("params=%v residual=%v, want dtype %q", params, residual, want)
			}
			out, err := Materialize(MaterializeInput{Engine: EngineVLLM, Raw: map[string]any{"dtype": alias}})
			if err != nil {
				t.Fatal(err)
			}
			if out.Quick.Dtype != want {
				t.Fatalf("form field %q materialized to %q, want %q", alias, out.Quick.Dtype, want)
			}
		})
	}
	// An alias is not a licence to invent values.
	params, residual, err := ReverseArgs(EngineVLLM, []string{"--dtype", "quarter"})
	if err != nil {
		t.Fatal(err)
	}
	if len(params) != 0 || !reflect.DeepEqual(residual, []string{"--dtype", "quarter"}) {
		t.Fatalf("params=%v residual=%v", params, residual)
	}
}

func TestReverseArgsHandlesCacheReportBooleanFlag(t *testing.T) {
	for engine, flag := range map[string]string{EngineVLLM: "--enable-prompt-tokens-details", EngineSGLang: "--enable-cache-report"} {
		params, residual, err := ReverseArgs(engine, []string{flag})
		if err != nil {
			t.Fatal(err)
		}
		if len(residual) != 0 || params["enable_cache_report"] != true {
			t.Fatalf("%s: params=%#v residual=%#v", engine, params, residual)
		}
	}
}
