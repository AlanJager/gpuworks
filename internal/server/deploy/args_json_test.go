package deploy

import (
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

// The extra-args text box is shell-lexed, so each case goes through
// ParseArgsText first: the table states what the user types, not what the
// validator receives.
func TestValidateExtraArgsJSON(t *testing.T) {
	const good = `{"cudagraph_mode":"FULL_DECODE_ONLY"}`
	for name, tc := range map[string]struct {
		text      string
		wantFlag  string
		wantValue string
	}{
		"double quotes eaten by the lexer": {text: `--compilation-config {"cudagraph_mode":"FULL_DECODE_ONLY"}`, wantFlag: "--compilation-config", wantValue: "{cudagraph_mode:FULL_DECODE_ONLY}"},
		"single quoted":                    {text: `--compilation-config '` + good + `'`},
		"double quoted with escapes":       {text: `--compilation-config "{\"cudagraph_mode\":\"FULL_DECODE_ONLY\"}"`},
		"glued with equals, eaten":         {text: `--additional-config={"a":1}`, wantFlag: "--additional-config", wantValue: "{a:1}"},
		"glued with equals, single":        {text: `--additional-config='{"a":1}'`},
		"additional-config":                {text: `--additional-config '{"a":1}'`},
		"speculative-config":               {text: `--speculative-config '{"method":"ngram","num_speculative_tokens":3}'`},
		"kv-transfer-config":               {text: `--kv-transfer-config '{"kv_connector":"X"}'`},
		"limit-mm-per-prompt":              {text: `--limit-mm-per-prompt '{"image":2}'`},
		"limit-mm-per-prompt unquoted":     {text: `--limit-mm-per-prompt {"image":2}`, wantFlag: "--limit-mm-per-prompt", wantValue: "{image:2}"},
		"reasoning-config":                 {text: `--reasoning-config '{"a":true}'`},
		"hf-overrides":                     {text: `--hf-overrides '{"architectures":["X"]}'`},
		"override-generation-config":       {text: `--override-generation-config '{"temperature":0}'`},
		"numeric level is JSON":            {text: `--compilation-config 3`},
		"flag without a value":             {text: `--compilation-config`},
		"flag followed by another flag":    {text: `--compilation-config --enforce-eager`, wantFlag: "--compilation-config", wantValue: "--enforce-eager"},
		"unlisted flag with an odd value":  {text: `--served-model-name {weird:value}`},
		"unlisted flag with colons":        {text: `--chat-template-kwargs {a:b}`},
		"no arguments":                     {text: ``},
	} {
		t.Run(name, func(t *testing.T) {
			args, err := ParseArgsText(tc.text)
			if err != nil {
				t.Fatal(err)
			}
			err = ValidateExtraArgsJSON(args)
			if tc.wantFlag == "" {
				if err != nil {
					t.Fatalf("%q rejected: %v", tc.text, err)
				}
				return
			}
			var productErr *messages.ProductError
			if !errors.As(err, &productErr) {
				t.Fatalf("%q: err=%v, want a ProductError", tc.text, err)
			}
			if productErr.Code() != ErrorExtraArgsJSONInvalid || productErr.Params()["flag"] != tc.wantFlag || productErr.Params()["value"] != tc.wantValue {
				t.Fatalf("%q: code=%s params=%v", tc.text, productErr.Code(), productErr.Params())
			}
		})
	}
}

// The dictionary's json-typed flags are covered without being listed twice.
func TestValidateExtraArgsJSONCoversDictionaryJSONFlags(t *testing.T) {
	for _, flag := range []string{"--rope-scaling", "--mm-processor-kwargs", "--media-io-kwargs"} {
		if err := ValidateExtraArgsJSON([]string{flag, "{a:1}"}); err == nil {
			t.Fatalf("%s accepted a non-JSON value", flag)
		}
		if err := ValidateExtraArgsJSON([]string{flag, `{"a":1}`}); err != nil {
			t.Fatalf("%s rejected JSON: %v", flag, err)
		}
	}
}
