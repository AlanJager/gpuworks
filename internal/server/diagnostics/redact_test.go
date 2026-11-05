package diagnostics

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestRedactErrorTextKeepsSafeCauseAndRemovesSecrets(t *testing.T) {
	const knownToken = "literal-node-credential"
	input := "source read failed: EOF; node=" + knownToken + " url=https://user:password@files.example/model?token=query-credential&part=1 Authorization: Bearer header-credential"
	got := RedactErrorText(input, 1024, "", knownToken)
	for _, secret := range []string{knownToken, "user:password", "query-credential", "header-credential"} {
		if strings.Contains(got, secret) {
			t.Fatalf("secret %q remained in %q", secret, got)
		}
	}
	if !strings.Contains(got, "source read failed: EOF") || !strings.Contains(got, "files.example/model") {
		t.Fatalf("safe cause/context lost: %q", got)
	}
}

func TestRedactErrorTextBoundsUTF8WithoutPartialSecret(t *testing.T) {
	for _, limit := range []int{-1, 0, 1, 2, 3, 4, 7, 1024} {
		got := RedactErrorText("目标错误："+strings.Repeat("长", 1000)+"token-whole-secret", limit, "token-whole-secret")
		if !utf8.ValidString(got) || len(got) > max(limit, 0) {
			t.Fatalf("limit=%d bytes=%d valid=%v output=%q", limit, len(got), utf8.ValidString(got), got)
		}
		if strings.Contains(got, "token-whole-secret") {
			t.Fatalf("limit=%d secret remained", limit)
		}
	}
	if got := RedactErrorText("故障abc", 4); got != "故" {
		t.Fatalf("split multibyte boundary=%q", got)
	}
	if got := RedactErrorText("redacted then EOF", 100, "redacted"); strings.Contains(got, "redacted") || !strings.Contains(got, "EOF") {
		t.Fatalf("explicit replacement marker reintroduced a secret=%q", got)
	}
	if got := RedactErrorText("plain cause", 100, ""); got != "plain cause" {
		t.Fatalf("empty explicit secret changed text=%q", got)
	}
	if got := RedactErrorText("token-whole-secret then EOF", 7, "token-whole-secret"); strings.Contains(got, "token-") {
		t.Fatalf("truncation happened before credential replacement=%q", got)
	}
	if got := RedactErrorText("bad\xffUTF8\xfe cause", 100); !utf8.ValidString(got) || !strings.Contains(got, "cause") {
		t.Fatalf("invalid source UTF8=%q", got)
	}
}

func TestRedactErrorTextDoesNotExposeJSONOrShellSecrets(t *testing.T) {
	for _, input := range []string{`failed: {"password":"json-secret"}`, "failed: API_TOKEN='shell-secret'"} {
		got := RedactErrorText(input, 1024)
		if strings.Contains(got, "json-secret") || strings.Contains(got, "shell-secret") {
			t.Fatalf("sensitive text remained: %q", got)
		}
	}
}

func TestErrorTextHelperPreservesDiagnosticJSONPrefixContract(t *testing.T) {
	got, err := Redact(map[string]any{"token": "abcdefgh-private-tail", "nested": `{"password":"12345678-private-tail"}`, "cause": "EOF"})
	if err != nil {
		t.Fatal(err)
	}
	result := got.(map[string]any)
	if result["token"] != "abcdefgh***" || result["nested"] != `{"password":"12345678***"}` || result["cause"] != "EOF" {
		t.Fatalf("existing diagnostic JSON redaction changed=%v", result)
	}
}
