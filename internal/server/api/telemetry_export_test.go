package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// fakeTelemetry records what it was asked and answers what it was told.
type fakeTelemetry struct {
	export     dto.TelemetryExport
	updates    []dto.TelemetryExportUpdate
	updateErr  error
	tests      []dto.TelemetryExportUpdate
	testResult dto.TelemetryExportTest
	testErr    error
}

func (f *fakeTelemetry) Export() dto.TelemetryExport { return f.export }

func (f *fakeTelemetry) UpdateExport(u dto.TelemetryExportUpdate) (dto.TelemetryExport, error) {
	f.updates = append(f.updates, u)
	return f.export, f.updateErr
}

func (f *fakeTelemetry) TestExport(_ context.Context, u dto.TelemetryExportUpdate) (dto.TelemetryExportTest, error) {
	f.tests = append(f.tests, u)
	return f.testResult, f.testErr
}

func telemetryRouter(t *testing.T, fake TelemetryService) (http.Handler, *store.Store, string, string) {
	t.Helper()
	signer, err := auth.NewTokenSigner("telemetry-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	st := managementSessionTestStore(t)
	admin, _ := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	viewer, _ := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 2, Username: "viewer", Role: "viewer"})
	return Router(Deps{Store: st, AuthSigner: signer, Telemetry: fake}), st, admin, viewer
}

func telemetryCall(r http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func TestTelemetryExportWireJSON(t *testing.T) {
	at := int64(1760500000000)
	fake := &fakeTelemetry{export: dto.TelemetryExport{
		Enabled: true, Endpoint: "http://collector:4318", Headers: []dto.TelemetryHeaderName{{Name: "Authorization"}},
		SampleRatio: 0.5, TraceLink: "https://grafana/explore?trace={trace_id}",
		Status: dto.TelemetryExportStatus{Active: true, TracesURL: "http://collector:4318/v1/traces", Exported: 10, Dropped: 1, Failed: 2,
			Queued: 3, LastError: "collector answered 503", LastErrorAt: &at},
	}}
	r, _, _, viewer := telemetryRouter(t, fake)
	// Any signed-in user reads it: the request view needs the trace link.
	rec := telemetryCall(r, http.MethodGet, "/api/v1/telemetry/export", viewer, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d %s", rec.Code, rec.Body.String())
	}
	want := `{"enabled":true,"endpoint":"http://collector:4318","headers":[{"name":"Authorization"}],"sample_ratio":0.5,` +
		`"trace_link":"https://grafana/explore?trace={trace_id}","status":{"active":true,"traces_url":"http://collector:4318/v1/traces",` +
		`"exported":10,"dropped":1,"failed":2,"queued":3,"last_error":"collector answered 503","last_error_at":1760500000000,` +
		`"last_export_at":null,"setting_error":""}}`
	if got := strings.TrimSpace(rec.Body.String()); got != want {
		t.Fatalf("body =\n%s\nwant\n%s", got, want)
	}
	var back dto.TelemetryExport
	if err := json.Unmarshal(rec.Body.Bytes(), &back); err != nil || back.Status.LastExportAt != nil || *back.Status.LastErrorAt != at {
		t.Fatalf("round trip %+v %v", back, err)
	}
}

func TestTelemetryExportUpdateAndTest(t *testing.T) {
	fake := &fakeTelemetry{export: dto.TelemetryExport{Endpoint: "http://c", Headers: []dto.TelemetryHeaderName{}},
		testResult: dto.TelemetryExportTest{OK: false, TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", TracesURL: "http://c/v1/traces", Error: "collector answered 401: bad token"}}
	r, _, admin, _ := telemetryRouter(t, fake)
	body := `{"enabled":true,"endpoint":"http://c","headers":[{"name":"Authorization","value":"Bearer x"},{"name":"X-Scope-OrgID","value":null},{"name":"X-Kept"}],"sample_ratio":0.1,"trace_link":""}`
	rec := telemetryCall(r, http.MethodPut, "/api/v1/telemetry/export", admin, body)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"endpoint":"http://c"`) {
		t.Fatalf("PUT %d %s", rec.Code, rec.Body.String())
	}
	value, ratio := "Bearer x", 0.1
	want := dto.TelemetryExportUpdate{Enabled: true, Endpoint: "http://c", SampleRatio: &ratio, Headers: []dto.TelemetryHeaderUpdate{
		{Name: "Authorization", Value: &value}, {Name: "X-Scope-OrgID"}, {Name: "X-Kept"}}}
	if len(fake.updates) != 1 || !reflect.DeepEqual(fake.updates[0], want) {
		t.Fatalf("update = %+v", fake.updates)
	}
	// A test answers 200 with what the collector said.
	rec = telemetryCall(r, http.MethodPost, "/api/v1/telemetry/export/test", admin, `{"endpoint":"http://c"}`)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"ok":false,"trace_id":"4bf92f3577b34da6a3ce929d0e0e4736","traces_url":"http://c/v1/traces","error":"collector answered 401: bad token"}` {
		t.Fatalf("test %d %s", rec.Code, rec.Body.String())
	}
	if len(fake.tests) != 1 || fake.tests[0].SampleRatio != nil || fake.tests[0].Endpoint != "http://c" {
		t.Fatalf("tests = %+v", fake.tests)
	}
}

func TestTelemetryExportRefusals(t *testing.T) {
	fake := &fakeTelemetry{}
	r, _, admin, viewer := telemetryRouter(t, fake)
	for _, c := range []struct{ method, path string }{{http.MethodPut, "/api/v1/telemetry/export"}, {http.MethodPost, "/api/v1/telemetry/export/test"}} {
		if rec := telemetryCall(r, c.method, c.path, "", "{}"); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without a token = %d", c.path, rec.Code)
		}
		if rec := telemetryCall(r, c.method, c.path, viewer, "{}"); rec.Code != http.StatusForbidden {
			t.Errorf("%s as a viewer = %d", c.path, rec.Code)
		}
		for _, bad := range []string{`{"enabled":"yes"}`, `{"endpointt":"x"}`, `not json`} {
			if rec := telemetryCall(r, c.method, c.path, admin, bad); rec.Code != http.StatusBadRequest {
				t.Errorf("%s %s = %d", c.path, bad, rec.Code)
			}
		}
	}
	if rec := telemetryCall(r, http.MethodGet, "/api/v1/telemetry/export", "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET without a token = %d", rec.Code)
	}
	if rec := telemetryCall(r, http.MethodDelete, "/api/v1/telemetry/export", admin, ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE = %d", rec.Code)
	}
	if len(fake.updates)+len(fake.tests) != 0 {
		t.Fatal("a refused request reached the service")
	}
	// The service's refusal is a product error; anything else a 500.
	fake.updateErr = &TelemetryError{Code: ErrorTelemetryHeaderValueRequired, Fallback: "header X needs a value", Params: map[string]any{"name": "X"}}
	rec := telemetryCall(r, http.MethodPut, "/api/v1/telemetry/export", admin, `{"headers":[{"name":"X"}]}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":"TELEMETRY_HEADER_VALUE_REQUIRED"`) || !strings.Contains(rec.Body.String(), `"name":"X"`) {
		t.Fatalf("refused update %d %s", rec.Code, rec.Body.String())
	}
	fake.testErr = errors.New("disk on fire")
	if rec := telemetryCall(r, http.MethodPost, "/api/v1/telemetry/export/test", admin, `{}`); rec.Code != http.StatusInternalServerError {
		t.Fatalf("broken test = %d", rec.Code)
	}
	// Without the service the routes are not there.
	bare, _, admin2, _ := telemetryRouter(t, nil)
	for _, c := range []struct{ method, path string }{{http.MethodGet, "/api/v1/telemetry/export"}, {http.MethodPut, "/api/v1/telemetry/export"}, {http.MethodPost, "/api/v1/telemetry/export/test"}} {
		if rec := telemetryCall(bare, c.method, c.path, admin2, "{}"); rec.Code != http.StatusNotFound {
			t.Errorf("%s %s without a service = %d", c.method, c.path, rec.Code)
		}
	}
}

// The generic settings routes neither list the telemetry setting (it holds
// header ciphertext) nor write it past its own validation.
func TestTheGenericSettingsRoutesLeaveTheTelemetrySettingAlone(t *testing.T) {
	r, st, admin, _ := telemetryRouter(t, &fakeTelemetry{})
	if err := st.SetSetting(store.SettingTelemetryExport, `{"enabled":true,"headers":[{"name":"A","value_cipher":"zzz"}]}`); err != nil {
		t.Fatal(err)
	}
	rec := telemetryCall(r, http.MethodGet, "/api/v1/settings", admin, "")
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "telemetry_export") || strings.Contains(rec.Body.String(), "value_cipher") {
		t.Fatalf("GET /settings %d %s", rec.Code, rec.Body.String())
	}
	rec = telemetryCall(r, http.MethodPut, "/api/v1/settings/telemetry_export", admin, `{"enabled":true,"endpoint":"http://evil"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":"SETTING_HAS_OWN_ROUTE"`) {
		t.Fatalf("PUT /settings/telemetry_export %d %s", rec.Code, rec.Body.String())
	}
	if raw := st.GetSetting(store.SettingTelemetryExport); strings.Contains(raw, "evil") {
		t.Fatalf("the generic route wrote the setting: %s", raw)
	}
}
