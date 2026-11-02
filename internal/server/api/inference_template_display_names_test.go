package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A template written before display names still imports. Its retired keys
// are dropped, the response says so with a stable code, and nothing of them
// is stored.
func TestImportInferenceTemplatesAcceptsRetiredDisplayProfileWithWarning(t *testing.T) {
	h, st := evaluateRig(t)
	legacy := `models:
- model_id: Acme/Legacy
  specs:
  - name: bf16
    display_profile: general
    engine: vllm
    image: vllm/vllm-openai:latest
    weights: {quantization: bf16, sources: [{huggingface: acme/legacy}]}
    gpus: {vendor: nvidia, cc: ">=8.0", display_target: nvidia}
`
	rec := httptest.NewRecorder()
	h.importInferenceTemplates(rec, httptest.NewRequest(http.MethodPost, "/api/v1/inference-templates/import", strings.NewReader(legacy)))
	want := `{"models":1,"package_digest":"","warnings":[` +
		`{"code":"CATALOG_DISPLAY_PROFILE_RETIRED","params":{"model_id":"acme/legacy","spec":"bf16"},"error":"Spec bf16 of acme/legacy sets display_profile or gpus.display_target, which are no longer used and were ignored; name the spec with display_name_zh_cn and display_name_en."}]}` + "\n"
	if rec.Code != http.StatusOK || rec.Body.String() != want {
		t.Fatalf("status=%d\n got: %s\nwant: %s", rec.Code, rec.Body.String(), want)
	}
	model, err := st.InferenceModelByModelID("acme/legacy")
	if err != nil || len(model.Specs) != 1 || strings.Contains(model.Specs[0].GPUs, "display_target") ||
		model.Specs[0].DisplayNameZHCN != "" || model.Specs[0].DisplayNameEN != "" {
		t.Fatalf("model=%+v err=%v", model, err)
	}
}

// Two specs of one engine with the same display name are rejected before
// anything is written.
func TestImportInferenceTemplatesRejectsSiblingSpecsWithOneName(t *testing.T) {
	h, st := evaluateRig(t)
	clash := `models:
- model_id: Acme/Clash
  specs:
  - name: bf16
    display_name_zh_cn: Ampere 及以上 · BF16
    display_name_en: Ampere+ · BF16
    engine: vllm
    image: vllm/vllm-openai:latest
    weights: {quantization: bf16, sources: [{huggingface: acme/clash}]}
    gpus: {vendor: nvidia, cc: ">=8.0"}
  - name: bf16-again
    display_name_zh_cn: Ampere 及以上 · BF16 · 投机解码
    display_name_en: Ampere+ · BF16
    engine: vllm
    image: vllm/vllm-openai:latest
    weights: {quantization: bf16, sources: [{huggingface: acme/clash}]}
    gpus: {vendor: nvidia, cc: ">=8.0"}
`
	rec := httptest.NewRecorder()
	h.importInferenceTemplates(rec, httptest.NewRequest(http.MethodPost, "/api/v1/inference-templates/import", strings.NewReader(clash)))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "share display_name_en") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if _, err := st.InferenceModelByModelID("acme/clash"); err == nil {
		t.Fatal("rejected document was stored")
	}
}
