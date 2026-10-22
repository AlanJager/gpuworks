package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestInstanceModelNameFollowsConcreteLaunch(t *testing.T) {
	for _, test := range []struct {
		name, engine, want string
		launch             proto.ResolvedLaunch
	}{
		{"sglang default", "sglang", "/model", proto.ResolvedLaunch{Entrypoint: []string{"python", "-m", "sglang.launch_server", "--model-path", "/model"}, ModelMount: "/model"}},
		{"custom alias", "sglang", "custom", proto.ResolvedLaunch{Entrypoint: []string{"sglang", "serve", "--model-path", "/model", "--served-model-name=custom"}}},
		{"diffusion model id", "sglang", "wan", proto.ResolvedLaunch{Entrypoint: []string{"sglang", "serve", "--model-path", "/model", "--model-id", "wan"}}},
		{"vllm positional", "vllm", "/runtime/model", proto.ResolvedLaunch{Entrypoint: []string{"vllm", "serve", "/runtime/model"}}},
		// The same rule through the manual command editor: the model is
		// positional inside the script the engine runs, so it is read from the
		// engine's own split of those tokens.
		{"vllm manual positional", "vllm", "/models/foo", proto.ResolvedLaunch{Entrypoint: []string{"/bin/sh"}, Argv: []string{"-c", "set -a\nvllm serve /models/foo --port 30000"}, ModelMount: "/model"}},
		{"last alias wins", "sglang", "last", proto.ResolvedLaunch{Entrypoint: []string{"sglang", "serve", "--served-model-name", "first", "--served-model-name", "last"}}},
		{"manual alias", "sglang", "public model", proto.ResolvedLaunch{Entrypoint: []string{"/bin/sh"}, Argv: []string{"-c", "set -a\nsglang serve --model-path /model --served-model-name 'public model'"}, ModelMount: "/model"}},
		{"manual dynamic alias", "sglang", "", proto.ResolvedLaunch{Entrypoint: []string{"/bin/sh"}, Argv: []string{"-c", "sglang serve --served-model-name \"$MODEL_NAME\""}, ModelMount: "/model"}},
		{"callable owns identity", "callable", "", proto.ResolvedLaunch{ModelMount: "/model"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			desired, err := json.Marshal(proto.InstanceDesiredState{ResolvedLaunch: test.launch})
			if err != nil {
				t.Fatal(err)
			}
			got := instanceModelName(&store.Deployment{Engine: test.engine, ServedName: "public"}, &store.Instance{DesiredStateJSON: string(desired)})
			if got != test.want {
				t.Fatalf("engine model=%q, want %q", got, test.want)
			}
		})
	}
}

func TestProxyKeepsPublicDiscoveryAndRewritesJSONModel(t *testing.T) {
	var upstreamBody string
	p, key, closeUpstream := newProxyE2E(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		upstreamBody = string(body)
		if int64(len(body)) != r.ContentLength {
			t.Errorf("upstream length=%d, body=%d", r.ContentLength, len(body))
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer closeUpstream()
	deployment, err := p.store.DeploymentByServedName("omni")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.store.SetModelInferenceConfig(deployment.ModelID, `[{"type":"openai.chat"}]`, `["text"]`); err != nil {
		t.Fatal(err)
	}
	desired, _ := json.Marshal(proto.InstanceDesiredState{ResolvedLaunch: proto.ResolvedLaunch{Entrypoint: []string{"vllm", "serve", "/runtime/model"}, ModelMount: "/runtime/model", APIContract: json.RawMessage(`{"operations":[{"type":"openai.chat","endpoint":"/v1/chat/completions","mode":"stream"}]}`)}})
	if err := p.store.UpdateInstanceDesiredState("omni-instance", string(desired)); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		handler http.HandlerFunc
		path    string
	}{
		{p.Handler(), "/v1/chat/completions"},
		{p.PlaygroundHandler(nil), "/api/v1/playground/v1/chat/completions"},
	} {
		body := `{"model":"omni", "metadata":{"model":"leave-me"},"messages":[{"role":"user","content":"hello"}]}`
		req := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		test.handler(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d: %s", rec.Code, rec.Body.String())
		}
		if upstreamBody != strings.Replace(body, `"model":"omni"`, `"model":"/runtime/model"`, 1) {
			t.Fatalf("upstream body=%s", upstreamBody)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	p.ModelsHandler()(rec, req)
	if !strings.Contains(rec.Body.String(), `"id":"omni"`) || strings.Contains(rec.Body.String(), "/runtime/model") {
		t.Fatalf("public discovery=%s", rec.Body.String())
	}
}

func TestRewriteMultipartModelPreservesLargeFileAndOtherFields(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	file, err := writer.CreateFormFile("file", "audio.wav")
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte{0, 1, 255, '"', '{'}, 1<<20)
	_, _ = file.Write(payload)
	_ = writer.WriteField("model", "public")
	_ = writer.WriteField("language", "zh")
	_ = writer.Close()
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	cleanup, err := rewriteModel(req, "/model", DefaultMaxBodyBytes)
	defer cleanup()
	if err != nil {
		t.Fatal(err)
	}
	reader := multipart.NewReader(req.Body, writer.Boundary())
	for _, expected := range []struct {
		field, filename string
		value           []byte
	}{{"file", "audio.wav", payload}, {"model", "", []byte("/model")}, {"language", "", []byte("zh")}} {
		part, err := reader.NextPart()
		if err != nil {
			t.Fatal(err)
		}
		value, _ := io.ReadAll(part)
		if part.FormName() != expected.field || part.FileName() != expected.filename || !bytes.Equal(value, expected.value) {
			t.Fatalf("multipart field %q was corrupted", expected.field)
		}
	}
}

func TestRewriteJSONLargeBodyStreamsThroughDisk(t *testing.T) {
	body := `{"model":"public","padding":"` + strings.Repeat("x", (2<<20)-32) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	cleanup, err := rewriteModel(req, "/model", DefaultMaxBodyBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if _, ok := req.Body.(*os.File); !ok {
		t.Fatalf("rewritten body = %T, want disk spool", req.Body)
	}
	var decoded map[string]any
	if err := json.NewDecoder(req.Body).Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["model"] != "/model" {
		t.Fatalf("rewritten model = %v, want /model", decoded["model"])
	}
}

func TestRewriteModelHandlesEscapedKeysDuplicateFieldsAndWebSocket(t *testing.T) {
	for _, body := range []string{
		`{"mo\u0064el":"old","nested":{"model":"keep"},"model":"last"}`,
		`{"content":"escaped \\\" model { }","model":"old"}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		cleanup, err := rewriteModel(req, `new"model`, DefaultMaxBodyBytes)
		if err != nil {
			cleanup()
			t.Fatal(err)
		}
		var value map[string]any
		if err := json.NewDecoder(req.Body).Decode(&value); err != nil {
			t.Fatal(err)
		}
		cleanup()
		if value["model"] != `new"model` {
			t.Fatalf("rewritten body=%v", value)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/realtime?model=public&voice=alloy", nil)
	req.Header.Set("Upgrade", "websocket")
	cleanup, err := rewriteModel(req, "/model", DefaultMaxBodyBytes)
	defer cleanup()
	if err != nil || req.URL.Query().Get("model") != "/model" || req.URL.Query().Get("voice") != "alloy" {
		t.Fatalf("websocket query=%s, error=%v", req.URL.RawQuery, err)
	}
	tooBig := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"public"}`))
	cleanup, err = rewriteModel(tooBig, "/model", 8)
	defer cleanup()
	if statusForExtractError(err) != http.StatusRequestEntityTooLarge {
		t.Fatalf("body cap error=%v", err)
	}
}
