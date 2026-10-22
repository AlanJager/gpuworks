package api

import (
	"encoding/json"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

// The template page's usage example is generated from the spec's whole
// declaration: the request defaults a fixed-format model needs and its
// working example prompt, not the operation type alone.
func TestTemplateUsageExampleCarriesTheSpecDeclaration(t *testing.T) {
	raw := `{"health_endpoint":"/health","operations":[{"type":"openai.completions","endpoint":"/v1/completions","mode":"stream",
		"request_defaults":{"stop":["<|plamo:op|>"],"temperature":0},
		"example_input":{"prompt":"<|plamo:op|>dataset\ntranslation\n<|plamo:op|>input lang=English\nHello\n<|plamo:op|>output lang=Japanese\n"}}]}`
	var contract *dto.APIContract
	if err := json.Unmarshal([]byte(raw), &contract); err != nil {
		t.Fatal(err)
	}
	enrichTemplateAPIContract(contract, json.RawMessage(raw))
	op := contract.Operations[0]
	if op.Example == nil {
		t.Fatal("no example")
	}
	var body map[string]any
	if err := json.Unmarshal(op.Example.Request.Body, &body); err != nil {
		t.Fatal(err)
	}
	if stop, _ := body["stop"].([]any); len(stop) != 1 || stop[0] != "<|plamo:op|>" || body["temperature"] != float64(0) {
		t.Fatalf("defaults missing from the example: %s", op.Example.Request.Body)
	}
	if prompt, _ := body["prompt"].(string); !strings.HasPrefix(prompt, "<|plamo:op|>dataset\ntranslation\n") || !strings.HasSuffix(prompt, "lang=Japanese\n") {
		t.Fatalf("example prompt = %q", body["prompt"])
	}
	// The snippets show the tags as written, not HTML-escaped.
	if !strings.Contains(op.Example.Curl, `"prompt":"<|plamo:op|>dataset\ntranslation\n<|plamo:op|>input lang=English\nHello`) ||
		!strings.Contains(op.Example.Python, `"stop": [
        "<|plamo:op|>"`) || strings.Contains(op.Example.Curl+op.Example.Python, `\u003c`) {
		t.Fatalf("snippets:\n%s\n%s", op.Example.Curl, op.Example.Python)
	}
}
