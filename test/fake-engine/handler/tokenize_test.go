package handler

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// The tokenizer endpoints answer with vLLM's shapes, defaults and errors,
// as recorded from vLLM 0.30 (see tokenize.go); the token ids are the
// fixture's own byte-level ones.
func TestTokenizeAnswersLikeVLLM(t *testing.T) {
	server := httptest.NewServer(New(Options{Model: "served"}))
	defer server.Close()
	call := func(method, path, contentType, body string) (int, http.Header, string) {
		t.Helper()
		req, err := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header, strings.TrimSuffix(string(raw), "\n")
	}
	for _, tc := range []struct {
		name, method, path, contentType, body string
		status                                int
		want                                  string
	}{
		{"prompt", "POST", "/tokenize", "application/json", `{"model":"served","prompt":"Hi you"}`, 200,
			`{"count":6,"max_model_len":40960,"tokens":[72,105,32,121,111,117],"token_strs":null}`},
		{"model is optional, content type too", "POST", "/tokenize", "", `{"prompt":"Hi"}`, 200,
			`{"count":2,"max_model_len":40960,"tokens":[72,105],"token_strs":null}`},
		{"empty prompt", "POST", "/tokenize", "application/json", `{"prompt":""}`, 200,
			`{"count":0,"max_model_len":40960,"tokens":[],"token_strs":null}`},
		{"token strings", "POST", "/tokenize", "application/json", `{"prompt":"a\n","return_token_strs":true}`, 200,
			`{"count":2,"max_model_len":40960,"tokens":[97,10],"token_strs":["a","<0x0A>"]}`},
		{"messages add the generation prompt by default", "POST", "/tokenize", "application/json", `{"messages":[{"role":"user","content":"H"}],"return_token_strs":true}`, 200,
			wantTokens(t, "<|im_start|>user\nH<|im_end|>\n<|im_start|>assistant\n", true)},
		{"messages win over a prompt", "POST", "/tokenize", "application/json", `{"prompt":"x","messages":[{"role":"user","content":[{"type":"text","text":"H"}]}],"add_generation_prompt":false,"chat_template_kwargs":{"enable_thinking":false}}`, 200,
			wantTokens(t, "<|im_start|>user\nH<|im_end|>\n", false)},
		{"detokenize", "POST", "/detokenize", "application/json", `{"model":"served","tokens":[72,105]}`, 200, `{"prompt":"Hi"}`},
		{"detokenize drops unknown ids", "POST", "/detokenize", "application/json", `{"tokens":[99999999]}`, 200, `{"prompt":""}`},
		{"detokenize nothing", "POST", "/detokenize", "application/json", `{"tokens":[]}`, 200, `{"prompt":""}`},
		{"wrong model", "POST", "/tokenize", "application/json", `{"model":"nope","prompt":"x"}`, 404,
			`{"error":{"message":"The model ` + "`nope`" + ` does not exist.","type":"NotFoundError","param":"model","code":404}}`},
		{"wrong model detokenize", "POST", "/detokenize", "application/json", `{"model":"nope","tokens":[1]}`, 404,
			`{"error":{"message":"The model ` + "`nope`" + ` does not exist.","type":"NotFoundError","param":"model","code":404}}`},
		{"neither prompt nor messages", "POST", "/tokenize", "application/json", `{}`, 400,
			`{"error":{"message":"2 validation errors:\n  {'type': 'missing', 'loc': 'body.prompt', 'msg': 'Field required'}\n  {'type': 'missing', 'loc': 'body.messages', 'msg': 'Field required'}","type":"Bad Request","param":"body.prompt","code":400}}`},
		{"prompt not a string", "POST", "/tokenize", "application/json", `{"prompt":5}`, 400,
			`{"error":{"message":"1 validation error:\n  {'type': 'string_type', 'loc': 'body.prompt', 'msg': 'Input should be a valid string'}","type":"Bad Request","param":"body.prompt","code":400}}`},
		{"empty conversation", "POST", "/tokenize", "application/json", `{"messages":[]}`, 400,
			`{"error":{"message":"Cannot apply chat template to an empty conversation. Provide at least one message.","type":"BadRequestError","param":null,"code":400}}`},
		{"bad json", "POST", "/tokenize", "application/json", `{bad`, 400,
			`{"error":{"message":"1 validation error:\n  {'type': 'json_invalid', 'loc': 'body', 'msg': 'JSON decode error'}","type":"Bad Request","param":"body","code":400}}`},
		{"detokenize without tokens", "POST", "/detokenize", "application/json", `{"model":"served"}`, 400,
			`{"error":{"message":"1 validation error:\n  {'type': 'missing', 'loc': 'body.tokens', 'msg': 'Field required'}","type":"Bad Request","param":"body.tokens","code":400}}`},
		{"detokenize a string", "POST", "/detokenize", "application/json", `{"tokens":["a"]}`, 400,
			`{"error":{"message":"1 validation error:\n  {'type': 'int_parsing', 'loc': 'body.tokens.0', 'msg': 'Input should be a valid integer'}","type":"Bad Request","param":"body.tokens.0","code":400}}`},
		{"GET", "GET", "/tokenize", "", ``, 405, `{"detail":"Method Not Allowed"}`},
		{"PUT detokenize", "PUT", "/detokenize", "", ``, 405, `{"detail":"Method Not Allowed"}`},
		// vLLM serves neither under /v1.
		{"not under v1", "POST", "/v1/tokenize", "application/json", `{"prompt":"x"}`, 404, `404 page not found`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, header, body := call(tc.method, tc.path, tc.contentType, tc.body)
			if status != tc.status || body != tc.want {
				t.Fatalf("= %d %s\nwant %d %s", status, body, tc.status, tc.want)
			}
			if status == http.StatusMethodNotAllowed && header.Get("Allow") != http.MethodPost {
				t.Fatalf("Allow = %q", header.Get("Allow"))
			}
		})
	}
}

// wantTokens is the fixture's /tokenize reply for text, spelled out.
func wantTokens(t *testing.T, text string, withStrings bool) string {
	t.Helper()
	ids := make([]string, 0, len(text))
	strs := make([]string, 0, len(text))
	for _, b := range []byte(text) {
		ids = append(ids, strconv.Itoa(int(b)))
		if b == '\n' {
			strs = append(strs, `"<0x0A>"`)
		} else {
			strs = append(strs, `"`+string(rune(b))+`"`)
		}
	}
	tokenStrs := "null"
	if withStrings {
		tokenStrs = "[" + strings.Join(strs, ",") + "]"
	}
	return fmt.Sprintf(`{"count":%d,"max_model_len":40960,"tokens":[%s],"token_strs":%s}`, len(ids), strings.Join(ids, ","), tokenStrs)
}

// Tokenize then detokenize is the identity, and a handler serving several
// models answers for each.
func TestTokenizeRoundTripAndSharedModels(t *testing.T) {
	server := httptest.NewServer(New(Options{Models: func() []string { return []string{"a", "b"} }}))
	defer server.Close()
	for _, model := range []string{"a", "b"} {
		resp, err := http.Post(server.URL+"/detokenize", "application/json", strings.NewReader(`{"model":"`+model+`","tokens":[104,195,169]}`))
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || strings.TrimSpace(string(raw)) != `{"prompt":"hé"}` {
			t.Fatalf("%s: %d %s", model, resp.StatusCode, raw)
		}
	}
	resp, err := http.Post(server.URL+"/tokenize", "application/json", strings.NewReader(`{"model":"c","prompt":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a model the handler does not serve = %d", resp.StatusCode)
	}
}
