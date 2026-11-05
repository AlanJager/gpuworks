package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// fakeCatalogTemplates answers like the app's exporter: a fixed export, or
// the error it is told to return, and records what it was asked.
type fakeCatalogTemplates struct {
	out   dto.CatalogTemplateExport
	err   error
	calls []string
}

func (f *fakeCatalogTemplates) Export(_ context.Context, id uint, includeEnv []string) (dto.CatalogTemplateExport, error) {
	f.calls = append(f.calls, fmt.Sprintf("%d %v", id, includeEnv))
	return f.out, f.err
}

func catalogTemplateRouter(t *testing.T, exporter CatalogTemplates) (http.Handler, string, string) {
	t.Helper()
	st, err := store.Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	signer, member := roleTestToken(t, st, "member")
	_, admin := roleTestToken(t, st, "admin")
	return Router(Deps{Store: st, AuthSigner: signer, CatalogTemplates: exporter}), member, admin
}

func callRouter(t *testing.T, router http.Handler, token, method, path, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func TestCatalogTemplateExportWire(t *testing.T) {
	exporter := &fakeCatalogTemplates{out: dto.CatalogTemplateExport{
		ModelID: "allenai/olmo-2-0425-1b-instruct", SpecName: "vllm-1xnvidia-h20-d7", YAML: "models: []\n",
		Verified: true, Saveable: true,
		Checks:      []dto.CatalogTemplateCheck{{Operation: "openai.chat", Endpoint: "/v1/chat/completions", Tested: true, Passed: true, HTTPStatus: 200}},
		Notes:       []messages.Warning{messages.NewWarning("verified_on", map[string]any{"gpu": "NVIDIA H20", "count": 1})},
		ExcludedEnv: []string{"HF_TOKEN"}, Evidence: "record",
	}}
	router, member, _ := catalogTemplateRouter(t, exporter)
	if code, _ := callRouter(t, router, "", http.MethodPost, "/api/v1/deployments/7/catalog-template", ""); code != http.StatusUnauthorized {
		t.Fatalf("anonymous = %d, want 401", code)
	}
	// Any signed-in user may export: saving is the import, which is not.
	code, body := callRouter(t, router, member, http.MethodPost, "/api/v1/deployments/7/catalog-template", `{"include_env":["LOG"]}`)
	if code != http.StatusOK {
		t.Fatalf("member export = %d %s", code, body)
	}
	want := `{"model_id":"allenai/olmo-2-0425-1b-instruct","spec_name":"vllm-1xnvidia-h20-d7","yaml":"models: []\n","verified":true,"saveable":true,` +
		`"checks":[{"operation":"openai.chat","endpoint":"/v1/chat/completions","tested":true,"passed":true,"http_status":200}],` +
		`"notes":[{"code":"verified_on","params":{"count":1,"gpu":"NVIDIA H20"}}],"excluded_env":["HF_TOKEN"],"evidence":"record"}`
	if strings.TrimSpace(body) != want {
		t.Fatalf("wire =\n%s\nwant\n%s", strings.TrimSpace(body), want)
	}
	if !slices.Equal(exporter.calls, []string{"7 [LOG]"}) {
		t.Fatalf("calls = %q", exporter.calls)
	}
	// No body is the same as no env to keep.
	if code, _ := callRouter(t, router, member, http.MethodPost, "/api/v1/deployments/7/catalog-template", ""); code != http.StatusOK || exporter.calls[1] != "7 []" {
		t.Fatalf("empty body = %d calls %q", code, exporter.calls)
	}
}

func TestCatalogTemplateExportWireKeysRoundTrip(t *testing.T) {
	encoded, err := json.Marshal(dto.CatalogTemplateExport{Checks: []dto.CatalogTemplateCheck{{Operation: "openai.video.generate"}}})
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(wire))
	for key := range wire {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if want := []string{"checks", "evidence", "excluded_env", "model_id", "notes", "saveable", "spec_name", "verified", "yaml"}; !slices.Equal(keys, want) {
		t.Fatalf("keys = %q, want %q", keys, want)
	}
	// An untested operation carries no status or message.
	if got := string(wire["checks"]); got != `[{"operation":"openai.video.generate","tested":false,"passed":false}]` {
		t.Fatalf("checks = %s", got)
	}
	var back dto.CatalogTemplateExport
	if err := json.Unmarshal(encoded, &back); err != nil || back.Checks[0].Operation != "openai.video.generate" {
		t.Fatalf("round trip = %+v %v", back, err)
	}
}

func TestCatalogTemplateExportErrors(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"unknown deployment", store.ErrNotFound, http.StatusNotFound, ""},
		{"not exportable", messages.NewProductError(messages.ErrorTemplateExportUnavailable, "x", map[string]any{"reason": "not_running"}), http.StatusConflict, messages.ErrorTemplateExportUnavailable},
		{"command not reproducible", messages.NewProductError(deploy.ErrorLaunchNotExportable, "x", map[string]any{"engine": "vllm"}), http.StatusConflict, deploy.ErrorLaunchNotExportable},
		{"store failure", fmt.Errorf("disk I/O error"), http.StatusInternalServerError, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router, member, _ := catalogTemplateRouter(t, &fakeCatalogTemplates{err: tc.err})
			code, body := callRouter(t, router, member, http.MethodPost, "/api/v1/deployments/7/catalog-template", "")
			if code != tc.status {
				t.Fatalf("status = %d %s, want %d", code, body, tc.status)
			}
			var wire struct {
				Code string `json:"code"`
			}
			_ = json.Unmarshal([]byte(body), &wire)
			if wire.Code != tc.code {
				t.Fatalf("code = %q, want %q", wire.Code, tc.code)
			}
		})
	}
	exporter := &fakeCatalogTemplates{}
	router, member, _ := catalogTemplateRouter(t, exporter)
	if code, _ := callRouter(t, router, member, http.MethodGet, "/api/v1/deployments/7/catalog-template", ""); code != http.StatusMethodNotAllowed {
		t.Fatalf("GET = %d, want 405", code)
	}
	if code, _ := callRouter(t, router, member, http.MethodPost, "/api/v1/deployments/7/catalog-template", `{"include_env":`); code != http.StatusBadRequest {
		t.Fatalf("malformed body = %d, want 400", code)
	}
	if len(exporter.calls) != 0 {
		t.Fatalf("a malformed request reached the exporter: %q", exporter.calls)
	}
	unwired, member, _ := catalogTemplateRouter(t, nil)
	if code, _ := callRouter(t, unwired, member, http.MethodPost, "/api/v1/deployments/7/catalog-template", ""); code != http.StatusNotFound {
		t.Fatalf("no exporter = %d, want 404", code)
	}
}

// Cloning a template writes a user spec into the shared catalog, as import
// does, so it is an administrator's action too.
func TestInferenceTemplateCloneRequiresAdministrator(t *testing.T) {
	router, member, admin := catalogTemplateRouter(t, nil)
	body := `{"source_model_id":"no/such-model"}`
	if code, resp := callRouter(t, router, member, http.MethodPost, "/api/v1/inference-templates/clone", body); code != http.StatusForbidden || !strings.Contains(resp, ErrorAdministratorRequired) {
		t.Fatalf("member clone = %d %s, want 403 %s", code, resp, ErrorAdministratorRequired)
	}
	if code, resp := callRouter(t, router, admin, http.MethodPost, "/api/v1/inference-templates/clone", body); code == http.StatusForbidden || code == http.StatusUnauthorized {
		t.Fatalf("admin clone = %d %s, want the handler to answer", code, resp)
	}
}
