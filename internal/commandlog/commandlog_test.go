package commandlog

import (
	"strings"
	"testing"
)

func TestRedactHidesCredentialArguments(t *testing.T) {
	command := "TOKEN=env-secret curl --token 'flag-secret' --password=pass-secret -H 'Authorization: Bearer bearer-secret' 'https://example.test/path?access_key=query-secret&ok=1'"
	got := Redact(command)
	for _, secret := range []string{"env-secret", "flag-secret", "pass-secret", "bearer-secret", "query-secret"} {
		if strings.Contains(got, secret) {
			t.Fatalf("redacted command contains %q: %q", secret, got)
		}
	}
	if strings.Count(got, "<redacted>") != 5 {
		t.Fatalf("redacted command=%q", got)
	}
}

// An engine that prints its own configuration writes credentials as a Python
// dict, an argparse Namespace or JSON; a hang right after that line puts it
// in a failure's evidence. Ordinary text that only resembles one is kept.
func TestRedactHidesCredentialsInPrintedConfiguration(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"python dict, list value", `non-default args: {'model': '/model', 'api_key': ['sk-live-1234567890abcdef'], 'port': 8000}`, `non-default args: {'model': '/model', 'api_key': <redacted>, 'port': 8000}`},
		{"python dict, two keys in the list", `{'api_key': ['k-one', 'k-two'], 'host': '0.0.0.0'}`, `{'api_key': <redacted>, 'host': '0.0.0.0'}`},
		{"python dict, string value", `{'hf_token': 'abc123', 'trust_remote_code': True}`, `{'hf_token': <redacted>, 'trust_remote_code': True}`},
		{"namespace, list value", `Namespace(api_key=['k-one', 'k-two'], host='0.0.0.0', port=8000)`, `Namespace(api_key=<redacted>, host='0.0.0.0', port=8000)`},
		{"namespace, quoted value", `args: Namespace(hf_token='abc 123', model='/model')`, `args: Namespace(hf_token=<redacted>, model='/model')`},
		{"json, string value", `{"api_key": "k-live", "model": "qwen"}`, `{"api_key": <redacted>, "model": "qwen"}`},
		{"json, hyphenated key, no spaces", `{"api-key":"k123","n":1}`, `{"api-key":<redacted>,"n":1}`},
		{"environment assignment is still redacted whole", `HF_TOKEN=abc123 API_KEY='a b' vllm serve`, `HF_TOKEN=<redacted> API_KEY=<redacted> vllm serve`},
		{"json, authorization header", `headers={"Authorization": "Bearer abc.def", "Accept": "*/*"}`, `headers={"Authorization": <redacted>, "Accept": "*/*"}`},
		{"dict, authorization header", `{'Authorization': 'Bearer abc.def'}`, `{'Authorization': <redacted>}`},
		{"a hugging face token by its shape", `Using token hf_AbCdEfGhIjKlMnOpQrStUvWx for download`, `Using token <redacted> for download`},
		{"an sk- key by its shape", `key sk-proj_1234567890abcdefXYZ accepted`, `key <redacted> accepted`},

		{"a count of tokens is not a credential", `{'max_tokens': 512, "max_completion_tokens": 64, 'tokenizer': 'fast'}`, `{'max_tokens': 512, "max_completion_tokens": 64, 'tokenizer': 'fast'}`},
		{"namespace settings", `Namespace(max_tokens=[1, 2], tokenizer_mode='auto', served_model_name=['qwen'])`, `Namespace(max_tokens=[1, 2], tokenizer_mode='auto', served_model_name=['qwen'])`},
		{"prose that names a key", `set 'api_key' in the config, see "token": below`, `set 'api_key' in the config, see "token": below`},
		{"a short hf_ or sk- word", `hf_hub offline; task sk-learn; path /opt/hf_cache`, `hf_hub offline; task sk-learn; path /opt/hf_cache`},
		{"an ordinary log line", `INFO 11-19 04:43:25 [api_server.py:1] Started server process [42]`, `INFO 11-19 04:43:25 [api_server.py:1] Started server process [42]`},
		{"a traceback line", `  File "/usr/lib/python3.12/site-packages/vllm/engine.py", line 12, in start`, `  File "/usr/lib/python3.12/site-packages/vllm/engine.py", line 12, in start`},
	} {
		if got := Redact(tc.in); got != tc.want {
			t.Errorf("%s:\n got  %s\n want %s", tc.name, got, tc.want)
		}
	}
}
