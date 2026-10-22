package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestImportManifestV2ReconcilesOfficialAndSupportsSourceFilter(t *testing.T) {
	st, err := store.Open("file:api_manifest_v2_import?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	h := &apiHandlers{d: Deps{Store: st}}
	body := `{"schema_version":2,"catalog_version":"v9","series":[{"name":"Community-like official","engine":"vllm","purpose":"inference","framework":"cuda","versions":[{"image_ref":"registry.example/vllm:v9","expected_image_id":"sha256:abc","run_spec":{"model_mount_path":"/models","env":{"X":"1"}}}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/images/import", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.importImages(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	img, err := st.ImageByRef("registry.example/vllm:v9")
	if err != nil {
		t.Fatal(err)
	}
	// run_spec is a retired manifest key: it parses and reaches nothing.
	if img.Source != "official" {
		t.Fatalf("image=%+v", img)
	}
	if !strings.Contains(rec.Body.String(), `"catalog_version":"v9"`) {
		t.Fatalf("import response = %s", rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/api/v1/images?source=official", nil)
	rec = httptest.NewRecorder()
	h.listImages(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Community-like official") {
		t.Fatalf("filtered response=%d %s", rec.Code, rec.Body.String())
	}
}

func TestCommunityImportGroupsRepositoryByFramework(t *testing.T) {
	st, err := store.Open("file:api_community_framework_grouping?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	h := &apiHandlers{d: Deps{Store: st}}
	imports := []string{
		`{"series":[{"engine":"vllm","purpose":"inference","framework":"cuda","source":"community","versions":[{"image_ref":"example/mixed:cuda","framework_version":"12.4"}]}]}`,
		`{"series":[{"engine":"vllm","purpose":"inference","framework":"cann","source":"community","versions":[{"image_ref":"example/mixed:cann","framework_version":"8.0"}]}]}`,
	}
	for _, body := range imports {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/images/import", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.importImages(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	}
	series, err := st.ListImageSeries()
	if err != nil {
		t.Fatal(err)
	}
	community := make([]store.ImageSeries, 0, 2)
	for _, row := range series {
		if row.Source == "community" {
			community = append(community, row)
		}
	}
	if len(community) != 2 {
		t.Fatalf("community series=%+v", community)
	}
	for _, row := range community {
		versions, err := st.VersionsForSeries(row.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(versions) != 1 || !strings.EqualFold(row.Framework, versions[0].Framework) {
			t.Fatalf("series=%q framework=%q versions=%+v", row.Name, row.Framework, versions)
		}
	}
}

// A version patch carries only whether the version is offered and where it is
// fetched from. A retired launch-default field is refused, and an empty patch
// says what it needs.
func TestPatchImageVersionAcceptsOnlyAcquisitionFields(t *testing.T) {
	st, err := store.Open("file:api_image_version_patch?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	community, _ := st.UpsertImage(&store.Image{ImageRef: "example/community:v1", Source: "community", Engine: "vllm", Enabled: true})
	h := &apiHandlers{d: Deps{Store: st}}
	patch := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPatch, "/api/v1/images/versions/1", strings.NewReader(body))
		req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"id": fmt.Sprint(community.ID)}))
		rec := httptest.NewRecorder()
		h.patchImageVersion(rec, req)
		return rec
	}
	for _, body := range []string{`{"model_mount_path":"/models"}`, `{"env":{"A":"B"}}`, `{"command_mode":"manual"}`, `{"enabled":true,"entrypoint_display":"[\"/x\"]"}`} {
		rec := patch(body)
		var wire struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &wire)
		if rec.Code != http.StatusBadRequest || wire.Code != ErrorImageVersionPatchInvalid {
			t.Fatalf("%s: status=%d body=%s", body, rec.Code, rec.Body.String())
		}
	}
	if rec := patch(`{}`); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), ErrorImageVersionPatchEmpty) {
		t.Fatalf("empty patch: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := patch(`{"enabled":false}`); rec.Code != http.StatusOK {
		t.Fatalf("enabled patch: status=%d body=%s", rec.Code, rec.Body.String())
	}
	updated, _ := st.ImageByRef(community.ImageRef)
	if updated.Enabled {
		t.Fatalf("enabled patch did not apply: %+v", updated)
	}
}

func TestPatchCommunitySeriesCommonCandidates(t *testing.T) {
	st, err := store.Open("file:api_series_common_patch?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	series, err := st.UpsertImageSeries(&store.ImageSeries{Name: "community-series", Source: "community", Engine: "vllm", Purpose: store.ImagePurposeInference, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	h := &apiHandlers{d: Deps{Store: st}}
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/images/1", strings.NewReader(`{"common_args":[{"flag":"--trust-remote-code"}],"common_env":[{"key":"HF_HUB_OFFLINE","value":"1"}]}`))
	req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"id": fmt.Sprint(series.ID)}))
	rec := httptest.NewRecorder()
	h.patchImageSeries(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	updated, err := st.GetImageSeries(series.ID)
	if err != nil || !strings.Contains(updated.CommonArgs, "trust-remote-code") || !strings.Contains(updated.CommonEnv, "HF_HUB_OFFLINE") {
		t.Fatalf("updated series=%+v err=%v", updated, err)
	}
	official, err := st.UpsertImageSeries(&store.ImageSeries{Name: "official-series", Source: "official", Engine: "vllm", Purpose: store.ImagePurposeInference, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPatch, "/api/v1/images/2", strings.NewReader(`{"common_args":[]}`))
	req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"id": fmt.Sprint(official.ID)}))
	rec = httptest.NewRecorder()
	h.patchImageSeries(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("official status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// An import file written before ADR 0031 still states launch defaults per
// version. They are no longer catalog data: the import succeeds, keeps the
// version facts, and the retired keys reach nothing.
func TestImportCommunityImageIgnoresRetiredLaunchDefaults(t *testing.T) {
	st, err := store.Open("file:api_community_runspec_import?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	h := &apiHandlers{d: Deps{Store: st}}
	body := `{"series":[{"name":"community/runspec","engine":"vllm","purpose":"inference","framework":"cuda","source":"community","versions":[{"image_ref":"community/runspec:v0.9.1","framework_version":"12.4","source":"community","command_mode":"image-entry","model_mount_path":"/models","entrypoint_display":"[\"/entrypoint.sh\"]","cmd_display":"[\"--serve\"]","env":{"HF_HUB_OFFLINE":"1"},"docker_args":["--shm-size=8g"],"extra_mounts":[{"host_path":"/cache","container_path":"/cache","purpose":"kv-cache","read_only":true}],"supported_gpus":["sm90"],"unsupported_gpus":["sm70"]}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/images/import", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.importImages(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	image, err := st.ImageByRef("community/runspec:v0.9.1")
	if err != nil {
		t.Fatal(err)
	}
	if image.EngineVersion != "0.9.1" || image.FrameworkVersion != "12.4" {
		t.Fatalf("version facts not persisted: %+v", image)
	}
	for _, retired := range []string{"entrypoint_display", "cmd_display", "command_mode", "model_mount_path", "docker_args", "extra_mounts"} {
		if strings.Contains(rec.Body.String(), `"`+retired+`"`) {
			t.Fatalf("import response still carries %s: %s", retired, rec.Body.String())
		}
	}
}

// TestImportRejectsFrameworkWithoutFrameworkVersion is the data-hygiene gate:
// a catalog entry that declares a framework must also declare its framework
// version. Unlike the engine version — display metadata the tag may imply —
// the framework version gates automatic selection, so it is never guessed
// from the tag: the half-declared row it would admit is exactly the shape
// that once made automatic image selection flip between call sites.
func TestImportRejectsFrameworkWithoutFrameworkVersion(t *testing.T) {
	st, err := store.Open("file:api_framework_version_gate?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	h := &apiHandlers{d: Deps{Store: st}}
	importOne := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/images/import", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.importImages(rec, req)
		return rec
	}
	rec := importOne(`{"series":[{"engine":"vllm","purpose":"inference","framework":"cuda","source":"community","versions":[{"image_ref":"example/undeclared:cuda13.0-vllm0.27.1"}]}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var failure struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &failure); err != nil || failure.Code != ErrorImageFrameworkVersionRequired {
		t.Fatalf("code=%q err=%v body=%s", failure.Code, err, rec.Body.String())
	}

	rec = importOne(`{"series":[{"engine":"vllm","purpose":"inference","framework":"cuda","source":"community","versions":[{"image_ref":"example/declared:cuda13.0-vllm0.27.1","framework_version":"13.0"}]}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("declared status=%d body=%s", rec.Code, rec.Body.String())
	}
	image, err := st.ImageByRef("example/declared:cuda13.0-vllm0.27.1")
	if err != nil {
		t.Fatal(err)
	}
	if image.FrameworkVersion != "13.0" {
		t.Fatalf("framework_version = %q, want the declared 13.0", image.FrameworkVersion)
	}
}
