package commandlog

import (
	"reflect"
	"testing"
)

func TestRedactArgvPreservesTokenBoundaries(t *testing.T) {
	in := []string{"python", "--token", "one two", "--api-key=three", "https://user:pass@example.test/x?access_key=four", `{"api_token":"five","mode":"fast"}`, "tail"}
	want := []string{"python", "--token", "<redacted>", "--api-key=<redacted>", "https://<redacted>@example.test/x?access_key=<redacted>", "<redacted>", "tail"}
	if got := RedactArgv(in); !reflect.DeepEqual(got, want) {
		t.Fatalf("RedactArgv=%q, want %q", got, want)
	}
	if in[2] != "one two" {
		t.Fatal("RedactArgv mutated its input")
	}
}

func TestRedactEnvPreservesPresenceAndRedactsURLCredentials(t *testing.T) {
	in := map[string]string{"HF_TOKEN": "secret", "AWS_SECRET_ACCESS_KEY": "aws-secret", "ENDPOINT": "https://user:pass@x.test/?token=secret", "CONFIG": `{"api_token":"json-secret","mode":"fast"}`, "LOG_LEVEL": "debug"}
	got := RedactEnv(in)
	if got["HF_TOKEN"] != "<redacted>" || got["AWS_SECRET_ACCESS_KEY"] != "<redacted>" || got["ENDPOINT"] != "https://<redacted>@x.test/?token=<redacted>" || got["CONFIG"] != "<redacted>" || got["LOG_LEVEL"] != "debug" {
		t.Fatalf("RedactEnv=%q", got)
	}
}

func TestRedactArgvTreatsShellCommandAsScript(t *testing.T) {
	in := []string{"/bin/sh", "-c", "cat <<'EOF'\n{\"api_token\":\"wire-secret\"}\nEOF\nrun"}
	want := []string{"/bin/sh", "-c", "<redacted>"}
	if got := RedactArgv(in); !reflect.DeepEqual(got, want) {
		t.Fatalf("RedactArgv=%q, want %q", got, want)
	}
}

func TestRedactScriptDropsSensitiveMultilineScript(t *testing.T) {
	for _, script := range []string{
		"echo setup\nexport HF_TOKEN='secret'\nrun",
		"PRIVATE_KEY='-----BEGIN PRIVATE KEY-----\nsecret-lines\n-----END PRIVATE KEY-----'\nrun",
	} {
		if got := RedactScript(script); got != "<redacted>" {
			t.Fatalf("RedactScript=%q", got)
		}
	}
}

func TestRedactStructuredRecursesByKeyAndStringValue(t *testing.T) {
	in := map[string]any{
		"outer":  []any{map[string]any{"api_token": "nested-secret", "endpoint": "https://user:pass@x.test/?token=url-secret"}},
		"config": `{"password":"json-secret"}`,
		"count":  3.0,
	}
	got := RedactStructured(in).(map[string]any)
	nested := got["outer"].([]any)[0].(map[string]any)
	if nested["api_token"] != "<redacted>" || nested["endpoint"] != "https://<redacted>@x.test/?token=<redacted>" || got["config"] != "<redacted>" || got["count"] != 3.0 {
		t.Fatalf("RedactStructured=%#v", got)
	}
	if in["config"] == "<redacted>" {
		t.Fatal("RedactStructured mutated input")
	}
}
