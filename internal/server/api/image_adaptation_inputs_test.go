package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The adaptation keys image inputs written for older releases still carry:
// vendors on a series, and vendors, supported_gpus and unsupported_gpus on a
// version. legacyImageKeys strips them from a payload for the comparison run.
var legacyImageKeys = []string{
	`"vendors":["ascend"],`,
	`"vendors":["nvidia"],`,
	`"supported_gpus":["h100"],`,
	`"unsupported_gpus":["sm70"],`,
	// Version keys first: the series key is a suffix of the version one.
	"        vendors: [nvidia]\n",
	"        supported_gpus: [h100]\n",
	"        unsupported_gpus: [sm70]\n",
	"    vendors: [nvidia]\n",
}

func withoutLegacyImageKeys(payload string) string {
	for _, key := range legacyImageKeys {
		payload = strings.ReplaceAll(payload, key, "")
	}
	return payload
}

const (
	legacyCatalogManifest = `{"schema_version":2,"catalog_version":"v9000","series":[{"name":"Legacy Catalog vLLM","engine":"vllm","purpose":"inference","framework":"cuda","vendors":["ascend"],"versions":[{"vendors":["ascend"],"supported_gpus":["h100"],"unsupported_gpus":["sm70"],"image_ref":"official/legacy-vllm:v1","engine_version":"0.9.0","framework_version":"12.4","blob":"blobs/legacy-catalog.tar"}]}]}`
	legacyCustomJSON      = `{"series":[{"name":"legacy-custom","engine":"vllm","purpose":"inference","framework":"cuda","source":"custom","vendors":["nvidia"],"versions":[{"vendors":["nvidia"],"supported_gpus":["h100"],"unsupported_gpus":["sm70"],"image_ref":"example/legacy-custom:v1","engine_version":"0.9.1","framework_version":"12.4"}]}]}`
	legacyCustomYAML      = "series:\n  - name: legacy-yaml\n    engine: sglang\n    purpose: inference\n    framework: cuda\n    source: custom\n    vendors: [nvidia]\n    versions:\n      - image_ref: example/legacy-yaml:v1\n        vendors: [nvidia]\n        supported_gpus: [h100]\n        unsupported_gpus: [sm70]\n        engine_version: \"0.4.0\"\n        framework_version: \"12.4\"\n"
)

type imageImportRun struct {
	t      *testing.T
	router http.Handler
	admin  string
	store  *store.Store
}

func newImageImportRun(t *testing.T, name string) *imageImportRun {
	t.Helper()
	st, err := store.Open("file:"+name+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	signer, admin := roleTestToken(t, st, "admin")
	return &imageImportRun{t: t, router: Router(Deps{Store: st, AuthSigner: signer}), admin: admin, store: st}
}

func (r *imageImportRun) send(req *http.Request) string {
	r.t.Helper()
	req.Header.Set("Authorization", "Bearer "+r.admin)
	rec := httptest.NewRecorder()
	r.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		r.t.Fatalf("%s %s = %d %s", req.Method, req.URL, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func (r *imageImportRun) importBody(contentType, body string) {
	r.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/images/import", strings.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	r.send(req)
}

// writeEveryImagePath feeds the same images through every path that writes the
// catalog: the embedded seed (store.Open), a catalog manifest, a custom import
// as JSON and as YAML.
func (r *imageImportRun) writeEveryImagePath(strip bool) {
	r.t.Helper()
	payload := func(s string) string {
		if strip {
			return withoutLegacyImageKeys(s)
		}
		return s
	}
	r.importBody("application/json", payload(legacyCatalogManifest))
	r.importBody("application/json", payload(legacyCustomJSON))
	r.importBody("application/yaml", payload(legacyCustomYAML))
}

// rows reads a catalog table as stored, minus the write timestamps.
func (r *imageImportRun) rows(table string) []map[string]any {
	r.t.Helper()
	var rows []map[string]any
	if err := r.store.DB().Raw(`SELECT * FROM "` + table + `" ORDER BY id`).Scan(&rows).Error; err != nil {
		r.t.Fatal(err)
	}
	for _, row := range rows {
		delete(row, "created_at")
		delete(row, "updated_at")
	}
	return rows
}

// Inputs written for older releases still carry the adaptation keys. Every
// write path accepts them and ignores them: the stored rows and the image API
// are exactly what the same inputs without those keys produce.
func TestImageWritePathsIgnoreLegacyAdaptationKeys(t *testing.T) {
	legacy, plain := newImageImportRun(t, "image_legacy_keys"), newImageImportRun(t, "image_plain_keys")
	if got := withoutLegacyImageKeys(legacyCatalogManifest); strings.Contains(got, "vendors") || strings.Contains(got, "gpus") {
		t.Fatalf("stripping left a legacy key: %s", got)
	}
	if got := withoutLegacyImageKeys(legacyCustomYAML); strings.Contains(got, "vendors") || strings.Contains(got, "gpus") {
		t.Fatalf("stripping left a legacy key: %q", got)
	}
	legacy.writeEveryImagePath(false)
	plain.writeEveryImagePath(true)
	for _, table := range []string{"image_series", "image_versions"} {
		got, want := legacy.rows(table), plain.rows(table)
		if len(got) == 0 || !reflect.DeepEqual(got, want) {
			t.Fatalf("%s with legacy keys = %v\nwithout = %v", table, got, want)
		}
	}
	for _, ref := range []string{"official/legacy-vllm:v1", "example/legacy-custom:v1", "example/legacy-yaml:v1"} {
		if _, err := legacy.store.ImageByRef(ref); err != nil {
			t.Fatalf("%s was not imported: %v", ref, err)
		}
	}
	listed := func(r *imageImportRun) string {
		return r.send(httptest.NewRequest(http.MethodGet, "/api/v1/images/versions", nil))
	}
	if got, want := listed(legacy), listed(plain); got != want {
		t.Fatalf("GET /images/versions with legacy keys = %s\nwithout = %s", got, want)
	}

	// The image API's keys: the adaptation fields never reached the wire.
	var versions []map[string]any
	if err := unmarshalList([]byte(listed(legacy)), &versions); err != nil {
		t.Fatal(err)
	}
	var custom map[string]any
	for _, version := range versions {
		if version["image_ref"] == "example/legacy-custom:v1" {
			custom = version
		}
	}
	versionKeys := []string{"deprecated", "driver_requirement", "effective_enabled", "enabled", "engine", "engine_version", "framework", "framework_version", "id", "image_ref", "local_runtime_size_bytes", "local_size_known_nodes", "name", "present_nodes", "purpose", "reference_count", "rolling_tag", "series_id", "source", "total_nodes", "upstream_gone"}
	if !reflect.DeepEqual(keys(custom), versionKeys) {
		t.Fatalf("version keys = %q", keys(custom))
	}
	for _, key := range []string{"local_runtime_size_bytes", "local_size_known_nodes"} {
		if custom[key] != float64(0) {
			t.Fatalf("unobserved image %s = %v, want 0", key, custom[key])
		}
	}
	series := map[string]any{}
	if err := json.Unmarshal([]byte(legacy.send(httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/images/%v", custom["series_id"]), nil))), &series); err != nil {
		t.Fatal(err)
	}
	if want := []string{"enabled", "engine", "framework", "id", "name", "present_nodes", "purpose", "source", "total_nodes", "version_count", "versions"}; !reflect.DeepEqual(keys(series), want) {
		t.Fatalf("series keys = %q", keys(series))
	}
	listedVersions, _ := series["versions"].([]any)
	if len(listedVersions) != 1 || !reflect.DeepEqual(keys(listedVersions[0].(map[string]any)), versionKeys) {
		t.Fatalf("series versions = %v", series["versions"])
	}
}
