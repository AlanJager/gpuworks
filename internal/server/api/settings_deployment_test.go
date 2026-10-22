package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// PUT /settings/deployment bounds every deployment in the cluster: only an
// administrator writes it, a body naming one field leaves the rest of the
// stored policy as it was, and an out-of-range value is refused before it is
// stored (DeploymentPolicy would otherwise silently fall back to defaults).
func TestPutSettingDeploymentPolicy(t *testing.T) {
	st, err := store.Open("file:api_settings_deployment_policy?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	signer, memberToken := roleTestToken(t, st, "member")
	_, adminToken := roleTestToken(t, st, "admin")
	r := Router(Deps{Store: st, AuthSigner: signer})

	do := func(method, token, body string) (*httptest.ResponseRecorder, map[string]any) {
		req := httptest.NewRequest(method, "/api/v1/settings/deployment", strings.NewReader(body))
		if method == http.MethodGet {
			req = httptest.NewRequest(method, "/api/v1/settings", nil)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec, out
	}
	before := st.DeploymentPolicy()

	if rec, out := do(http.MethodPut, memberToken, `{"start_timeout_sec":3600}`); rec.Code != http.StatusForbidden || out["code"] != ErrorAdministratorRequired {
		t.Fatalf("member PUT = %d %v, want 403 %s", rec.Code, out, ErrorAdministratorRequired)
	}
	if got := st.DeploymentPolicy(); got != before {
		t.Fatalf("member PUT changed the policy: %+v", got)
	}

	// One field named: the stored pending timeout and retention survive.
	if rec, _ := do(http.MethodPut, adminToken, `{"start_timeout_sec":3600}`); rec.Code != http.StatusOK {
		t.Fatalf("admin PUT = %d %s", rec.Code, rec.Body.String())
	}
	want := before
	want.StartTimeoutSec = 3600
	if got := st.DeploymentPolicy(); got != want {
		t.Fatalf("policy after partial PUT = %+v, want %+v", got, want)
	}
	if rec, _ := do(http.MethodPut, adminToken, `{"pending_timeout_sec":120}`); rec.Code != http.StatusOK {
		t.Fatalf("admin PUT = %d %s", rec.Code, rec.Body.String())
	}
	want.PendingTimeoutSec = 120
	if got := st.DeploymentPolicy(); got != want {
		t.Fatalf("second partial PUT clobbered the first: %+v, want %+v", got, want)
	}

	for _, body := range []string{`{"start_timeout_sec":59}`, `{"start_timeout_sec":86401}`, `{"start_timeout_sec":0}`, `{"pending_timeout_sec":-1}`} {
		rec, out := do(http.MethodPut, adminToken, body)
		if rec.Code != http.StatusBadRequest || out["code"] != ErrorFieldInvalid {
			t.Fatalf("PUT %s = %d %v, want 400 %s", body, rec.Code, out, ErrorFieldInvalid)
		}
		if got := st.DeploymentPolicy(); got != want {
			t.Fatalf("rejected PUT %s changed the policy: %+v", body, got)
		}
	}
	if rec, _ := do(http.MethodPut, adminToken, `not json`); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed PUT = %d, want 400", rec.Code)
	}

	// GET serves the stored document verbatim, with the wire key the form reads.
	rec, out := do(http.MethodGet, memberToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET settings = %d", rec.Code)
	}
	policy, ok := out["deployment"].(map[string]any)
	if !ok || policy["start_timeout_sec"] != float64(3600) || policy["pending_timeout_sec"] != float64(120) {
		t.Fatalf("GET deployment policy = %#v", out["deployment"])
	}
}

// A policy stored before start_timeout_sec existed is served with the value
// that actually applies, so the settings card and the deploy form's
// placeholder show the shipped default instead of an empty field.
func TestGetSettingsServesTheEffectiveDeploymentPolicy(t *testing.T) {
	st, err := store.Open("file:api_settings_deployment_effective?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.SetSetting(store.SettingDeploymentPolicy, `{"pending_timeout_sec":120,"event_retention_days":30,"event_retention_bytes":1073741824}`); err != nil {
		t.Fatal(err)
	}
	signer, token := roleTestToken(t, st, "member")
	r := Router(Deps{Store: st, AuthSigner: signer})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET settings = %d %s", rec.Code, rec.Body)
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	const want = `{"pending_timeout_sec":120,"start_timeout_sec":1800,"event_retention_days":30,"event_retention_bytes":1073741824}`
	if string(out["deployment"]) != want {
		t.Fatalf("deployment = %s, want %s", out["deployment"], want)
	}
}
