package aihelper

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSensitiveKeys(t *testing.T) {
	for key, want := range map[string]bool{
		"token": true, "Token": true, "tokens": true, "hf_token": true, "secret": true, "client_secret": true, "password": true, "passwd": true,
		"api_key": true, "apikey": true, "API_KEY": true, "AWS_SECRET_ACCESS_KEY": true, "access_key": true, "passphrase": true, "authorization": true, "private_key": true, "privatekey": true, "credential": true, "ssh_credentials": true,
		"prompt_tokens":      true, // matches, but a count is a number and is never redacted
		"has_ssh_credential": true, // matches, but a bool is never redacted
		"token_prefix":       false, "tokenizer_ref": false, "tokens_per_iteration": false, "secretary": false,
		"public_key": false, "label_key": false, "key": false, "max_tokens_hint": false, "mytoken": false,
	} {
		if got := sensitiveKey.MatchString(key); got != want {
			t.Errorf("sensitiveKey(%q) = %v, want %v", key, got, want)
		}
	}
}

func TestRedactReplacesOnlyNonEmptyStrings(t *testing.T) {
	var v any
	_ = json.Unmarshal([]byte(`{"token":"t","hf_token":"","prompt_tokens":12,"password":null,"nested":[{"secret":"s","name":"n"}],"credential":{"user":"u"}}`), &v)
	b, _ := json.Marshal(redact(v))
	if string(b) != `{"credential":{"user":"u"},"hf_token":"","nested":[{"name":"n","secret":"[redacted]"}],"password":null,"prompt_tokens":12,"token":"[redacted]"}` {
		t.Fatalf("redacted = %s", b)
	}
}

// Secrets written inside values: flags, assignments, URLs and env pairs.
// Counts and look-alike names stay readable, since a diagnosis needs them.
func TestRedactFindsSecretsInsideValues(t *testing.T) {
	for in, want := range map[string]string{
		"--api-key sk-abc123 --max-model-len 4096":     "--api-key [redacted] --max-model-len 4096",
		"--hf-token=hf_abcdef --tokenizer Qwen/Qwen3":  "--hf-token=[redacted] --tokenizer Qwen/Qwen3",
		"--max-num-batched-tokens 8192":                "--max-num-batched-tokens 8192",
		"vers=4.1,username=u,password=p4ss,hard":       "vers=4.1,username=u,password=[redacted],hard",
		"export HF_TOKEN=hf_abcdef; vllm serve m":      "export HF_TOKEN=[redacted]; vllm serve m",
		"max_tokens=512 temperature=0.7":               "max_tokens=512 temperature=0.7",
		"https://user:p4ssw0rd@host.example.com/m/":    "https://user:[redacted]@host.example.com/m/",
		"https://host.example.com:8443/m/":             "https://host.example.com:8443/m/",
		"Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.x": "Authorization: Bearer [redacted]",
	} {
		if got := redactString(in); got != want {
			t.Errorf("redactString(%q) = %q, want %q", in, got, want)
		}
	}
	var v any
	_ = json.Unmarshal([]byte(`{"argv":["vllm","serve","--api-key","sk-abc123","--max-num-batched-tokens","8192"],`+
		`"common_env":[{"name":"HF_TOKEN","value":"hf_x"},{"name":"LOG_LEVEL","value":"info"}],"env":{"AWS_SECRET_ACCESS_KEY":"aws"}}`), &v)
	b, _ := json.Marshal(redact(v))
	if string(b) != `{"argv":["vllm","serve","--api-key","[redacted]","--max-num-batched-tokens","8192"],`+
		`"common_env":[{"name":"HF_TOKEN","value":"[redacted]"},{"name":"LOG_LEVEL","value":"info"}],"env":{"AWS_SECRET_ACCESS_KEY":"[redacted]"}}` {
		t.Fatalf("redacted = %s", b)
	}
}

func TestAgedBuckets(t *testing.T) {
	atFakeNow(t)
	for d, want := range map[time.Duration]string{
		-time.Minute:                   "2026-10-11T12:01:00Z (in the future)",
		0:                              "2026-10-11T12:00:00Z (0s ago)",
		59 * time.Second:               "2026-10-11T11:59:01Z (59s ago)",
		time.Minute:                    "2026-10-11T11:59:00Z (1m ago)",
		59 * time.Minute:               "2026-10-11T11:01:00Z (59m ago)",
		time.Hour:                      "2026-10-11T11:00:00Z (1.0h ago)",
		71 * time.Hour:                 "2026-10-08T13:00:00Z (71.0h ago)",
		72 * time.Hour:                 "2026-10-08T12:00:00Z (3.0d ago)",
		36*24*time.Hour + 12*time.Hour: "2026-09-05T00:00:00Z (36.5d ago)",
	} {
		if got := aged(fakeNow.Add(-d)); got != want {
			t.Errorf("aged(now-%v) = %q, want %q", d, got, want)
		}
	}
	// A time in another zone is shown in UTC.
	at := time.Date(2026, 10, 11, 19, 0, 0, 0, time.FixedZone("CST", 8*3600))
	if got := aged(at); got != "2026-10-11T11:00:00Z (1.0h ago)" {
		t.Fatalf("aged(CST) = %q", got)
	}
}

func TestHumanizeTouchesOnlyTimes(t *testing.T) {
	atFakeNow(t)
	var v any
	_ = json.Unmarshal([]byte(`{"created_at":"2026-10-11T18:00:00.25+08:00","at":"2026-10-11T11:30:00Z","time_ms":1791720000000,`+
		`"ttft_ms":350,"cursor_ts":1791720000000,"format":"not a time","updated_at":"yesterday","deleted_at":null,"items":[{"last_seen_at":"2026-10-11T11:59:00Z"}]}`), &v)
	b, _ := json.Marshal(humanize(v))
	want := `{"at":"2026-10-11T11:30:00Z (30m ago)","created_at":"2026-10-11T10:00:00Z (2.0h ago)","cursor_ts":1791720000000,"deleted_at":null,"format":"not a time",` +
		`"items":[{"last_seen_at":"2026-10-11T11:59:00Z (1m ago)"}],"time_ms":"` + aged(time.UnixMilli(1791720000000)) + `","ttft_ms":350,"updated_at":"yesterday"}`
	if string(b) != want {
		t.Fatalf("got  %s\nwant %s", b, want)
	}
}

func TestOutline(t *testing.T) {
	var v any
	_ = json.Unmarshal([]byte(`{"a":{"b":{"c":{"d":1}}},"list":[{"x":[1,2],"y":true}],"empty":[],"n":null,"s":"x"}`), &v)
	b, _ := json.Marshal(outline(v, 0))
	if string(b) != `{"a":{"b":{"c":"{…}"}},"empty":[],"list":[{"x":["number","×2"],"y":"bool"},"×1"],"n":"null","s":"string"}` {
		t.Fatalf("outline = %s", b)
	}
	wide := map[string]any{}
	for i := 0; i < 45; i++ {
		wide[string(rune('A'+i))] = 1
	}
	o := outline(wide, 0).(map[string]any)
	if len(o) != 41 || o["…"] != "5 more keys" {
		t.Fatalf("wide outline has %d keys, marker %v", len(o), o["…"])
	}
}

func TestIsEmptyAnswer(t *testing.T) {
	for raw, want := range map[string]bool{
		`null`: true, `[]`: true, `{}`: true, `{"items":[],"total":0}`: true, `{"items":[]}`: true,
		`0`: false, `""`: false, `false`: false, `[0]`: false, `{"items":[1]}`: false, `{"total":0}`: false, `{"items":null}`: false,
	} {
		var v any
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			t.Fatal(err)
		}
		if got := isEmptyAnswer(v); got != want {
			t.Errorf("isEmptyAnswer(%s) = %v, want %v", raw, got, want)
		}
	}
}

func TestRunJQ(t *testing.T) {
	ctx := context.Background()
	in := map[string]any{"items": []any{1.0, 2.0, 3.0}}
	for filter, want := range map[string]string{
		".items | length": `3`,
		".items[]":        `[1,2,3]`,
		"empty":           `[]`,
		".items[] | if . == 3 then halt else . end": `[1,2]`,
		".items[] | if . == 2 then halt else . end": `1`,
		"halt": `[]`,
	} {
		got, err := runJQ(ctx, filter, in)
		if err != nil {
			t.Fatalf("%s: %v", filter, err)
		}
		if b, _ := json.Marshal(got); string(b) != want {
			t.Errorf("%s = %s, want %s", filter, b, want)
		}
	}
	// halt_error carries a value: an error, not a quiet stop.
	if _, err := runJQ(ctx, `"stop" | halt_error`, in); err == nil {
		t.Fatal("halt_error was not an error")
	}
	if _, err := runJQ(ctx, "range(20000)", in); err == nil || err.Error() != "jq produced more than 10000 results; aggregate or select first" {
		t.Fatalf("unbounded output: %v", err)
	}
	if _, err := runJQ(ctx, "$undefined", in); err == nil {
		t.Fatal("a compile error was not reported")
	}
	// A filter that never ends is stopped by the deadline.
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := runJQ(short, "last(range(1e15))", in)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > jqTimeout {
		t.Fatalf("endless filter: %v after %v", err, time.Since(start))
	}
}

func TestShapeCountsRunesNotBytes(t *testing.T) {
	s := strings.Repeat("节", maxStringChars)
	if got := shape(s, 1); got != s {
		t.Fatal("a string at the limit was cut")
	}
	if got := shape(s+"点", 1); got != s+"…[1 more chars]" {
		t.Fatalf("got %q", got)
	}
	got := shape([]any{[]any{"a", "b"}, "c"}, 1)
	b, _ := json.Marshal(got)
	if string(b) != `[["a","…1 more items (raise limit, page with the endpoint's own parameters, or filter with jq)"],"…1 more items (raise limit, page with the endpoint's own parameters, or filter with jq)"]` {
		t.Fatalf("nested arrays = %s", b)
	}
}

func TestCompactStaysWithinTheCap(t *testing.T) {
	if got := compact(map[string]any{"a": 1}, "hint"); got != `{"a":1}` {
		t.Fatalf("small = %q", got)
	}
	big := []any{strings.Repeat("中", 5000)} // 15000 bytes of 3-byte runes
	got := compact(big, "Select fields with jq.")
	if len(got) > maxAnswerChars || !strings.HasSuffix(got, " of 15004 chars shown. Select fields with jq.]") {
		t.Fatalf("%d bytes, tail %q", len(got), got[len(got)-80:])
	}
	head := got[:strings.Index(got, "\n[truncated")]
	if !strings.HasSuffix(head, "…") || !strings.HasPrefix(got[len(head):], "\n[truncated: "+itoa(len(head)-len("…"))+" of") {
		t.Fatalf("note does not count what is shown: %q", got[len(head)-10:])
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// A jq error names the fields the filter used that the answer lacks, with
// the closest one it has - the case seen on gpu3, where the model guessed
// memory_bytes for memory_total_bytes and model for name.
func TestFieldHintsNameMissingFieldsAndTheirNearest(t *testing.T) {
	data := map[string]any{"items": []any{map[string]any{
		"node_name": "ppu-1", "name": "PPU-ZW810E", "vendor": "ppu", "memory_total_bytes": 1.0,
		"allocation_state": "available",
	}}, "overview": map[string]any{"total": 43.0}}
	filter := `.items[] | {node: .node_name, model, vendor, memory_gib: (.memory_bytes/1073741824|floor), alloc: .allocation_state}`
	want := ` The answer has no field "memory_bytes" (did you mean "memory_total_bytes"?), "model".`
	if got := fieldHints(filter, data); got != want {
		t.Fatalf("hints = %q\nwant  %q", got, want)
	}
	if got := fieldHints(`.items[] | {name, vendor} | if .vendor == "ppu" then . else empty end`, data); got != "" {
		t.Fatalf("a filter naming only real fields got hints %q", got)
	}
}

func TestJQFieldNamesReadsDottedAndShorthandKeys(t *testing.T) {
	got := jqFieldNames(`.items[] | {a, b: .c, d} | select(.e? != null) | {f}`)
	want := []string{"items", "c", "e", "a", "d", "f"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("fields = %v, want %v", got, want)
	}
}
