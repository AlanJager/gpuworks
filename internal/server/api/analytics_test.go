package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type fakeAnalytics struct {
	view    dto.DataRetentionView
	set     []dto.DataRetention
	setErr  error
	status  dto.DataRetentionStatus
	statErr error
}

func (f *fakeAnalytics) Retention() dto.DataRetentionView { return f.view }

func (f *fakeAnalytics) SetRetention(r dto.DataRetention) (dto.DataRetentionView, error) {
	f.set = append(f.set, r)
	if f.setErr != nil {
		return dto.DataRetentionView{}, f.setErr
	}
	f.view.Value = r
	return f.view, nil
}

func (f *fakeAnalytics) RetentionStatus(context.Context) (dto.DataRetentionStatus, error) {
	return f.status, f.statErr
}

const retentionBody = `{"metrics_hours":72,"logs_hours":48,"request_log_days":7,"request_log_max_mb":2048,` +
	`"usage_hourly_days":31,"usage_daily_days":400,"stats_hourly_days":31,"stats_daily_days":400,` +
	`"quality":true,"gpu":false,"gpu_time":true,"time_zone":"Asia/Shanghai"}`

func TestDataRetentionRoutes(t *testing.T) {
	fake := &fakeAnalytics{view: dto.DataRetentionView{
		Limits:         map[string]dto.DataRetentionLimit{"metrics_hours": {Min: 24, Max: 720}},
		ServerTimeZone: "Asia/Shanghai",
	}}
	signer, err := auth.NewTokenSigner("analytics-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	st := managementSessionTestStore(t)
	admin, _ := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	viewer, _ := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 2, Username: "viewer", Role: "viewer"})
	rig := struct {
		st      *store.Store
		session string
	}{st, admin}
	r := Router(Deps{Store: st, AuthSigner: signer, Analytics: fake})
	call := func(method, path, token, body string) (int, string) {
		rec := telemetryCall(r, method, path, token, body)
		return rec.Code, rec.Body.String()
	}
	if code, _ := call(http.MethodGet, "/api/v1/data-retention", "", ""); code != http.StatusUnauthorized {
		t.Fatalf("anonymous get = %d", code)
	}
	if code, _ := call(http.MethodPut, "/api/v1/data-retention", "", retentionBody); code != http.StatusUnauthorized {
		t.Fatalf("anonymous put = %d", code)
	}
	if code, _ := call(http.MethodPost, "/api/v1/data-retention", rig.session, retentionBody); code != http.StatusMethodNotAllowed {
		t.Fatalf("post = %d", code)
	}

	// Anyone signed in reads it; only an administrator changes it.
	if code, _ := call(http.MethodGet, "/api/v1/data-retention", viewer, ""); code != http.StatusOK {
		t.Fatalf("viewer get = %d", code)
	}
	if code, body := call(http.MethodPut, "/api/v1/data-retention", viewer, retentionBody); code != http.StatusForbidden || len(fake.set) != 0 {
		t.Fatalf("viewer put = %d %s", code, body)
	}
	code, body := call(http.MethodPut, "/api/v1/data-retention", rig.session, retentionBody)
	if code != http.StatusOK || len(fake.set) != 1 {
		t.Fatalf("put = %d %s", code, body)
	}
	if got := fake.set[0]; got.LogsHours != 48 || got.GPU || !got.GPUTime || got.TimeZone != "Asia/Shanghai" || got.UsageDailyDays != 400 {
		t.Fatalf("decoded = %+v", got)
	}
	// The wire form: the value, the bounds and the server's zone.
	var view map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &view); err != nil || string(view["value"]) != retentionBody ||
		string(view["limits"]) != `{"metrics_hours":{"min":24,"max":720}}` || string(view["server_time_zone"]) != `"Asia/Shanghai"` {
		t.Fatalf("response = %s", body)
	}
	if code, body := call(http.MethodGet, "/api/v1/data-retention", rig.session, ""); code != http.StatusOK || !strings.Contains(body, `"logs_hours":48`) {
		t.Fatalf("get = %d %s", code, body)
	}

	// The setting is replaced whole: a missing, unknown or mistyped value is
	// refused before it reaches the service.
	for _, tc := range []struct{ body, field string }{
		{strings.Replace(retentionBody, `"gpu":false,`, ``, 1), "gpu"},
		{strings.Replace(retentionBody, `}`, `,"extra":1}`, 1), "extra"},
		{strings.Replace(retentionBody, `"logs_hours":48`, `"logs_hours":"48"`, 1), "logs_hours"},
	} {
		code, body := call(http.MethodPut, "/api/v1/data-retention", rig.session, tc.body)
		if code != http.StatusBadRequest || !strings.Contains(body, `"code":"FIELD_INVALID"`) || !strings.Contains(body, `"field":"`+tc.field+`"`) {
			t.Errorf("%s: %d %s", tc.field, code, body)
		}
	}
	if code, _ := call(http.MethodPut, "/api/v1/data-retention", rig.session, `not json`); code != http.StatusBadRequest {
		t.Fatalf("not json = %d", code)
	}
	if len(fake.set) != 1 {
		t.Fatalf("refused bodies reached the service: %d", len(fake.set))
	}
	// The service's validation error names its field.
	fake.setErr = &store.DataRetentionError{Field: "usage_hourly_days", Detail: "must be between 7 and 93"}
	if code, body := call(http.MethodPut, "/api/v1/data-retention", rig.session, retentionBody); code != http.StatusBadRequest ||
		!strings.Contains(body, `"field":"usage_hourly_days"`) || !strings.Contains(body, `"detail":"must be between 7 and 93"`) {
		t.Fatalf("invalid = %d %s", code, body)
	}
	fake.setErr = errors.New("disk full")
	if code, _ := call(http.MethodPut, "/api/v1/data-retention", rig.session, retentionBody); code != http.StatusInternalServerError {
		t.Fatalf("failure = %d", code)
	}

	fake.status = dto.DataRetentionStatus{
		Stores:      []dto.DataRetentionStore{{Kind: "usage", File: "main", HourlyRows: new(int64)}},
		LiveSinceMS: 5, Backfill: dto.StatisticsBackfill{UsageDone: true, UsageThroughMS: 3},
		UsageFlush: dto.UsageFlush{AtMS: 9, Rows: 2, MS: 1},
	}
	code, body = call(http.MethodGet, "/api/v1/data-retention/status", rig.session, "")
	want := `{"stores":[{"kind":"usage","file":"main","file_bytes":null,"hourly_rows":0,"daily_rows":null,"earliest_ms":null}],` +
		`"live_since_ms":5,"backfill":{"usage_done":true,"usage_through_ms":3,"quality_done":false,"quality_through_ms":0},` +
		`"retain":{"at_ms":0},"rollup":{"at_ms":0},"usage_flush":{"at_ms":9,"rows":2,"ms":1}}` + "\n"
	if code != http.StatusOK || body != want {
		t.Fatalf("status = %d\n%s\nwant\n%s", code, body, want)
	}
	fake.statErr = errors.New("locked")
	if code, _ := call(http.MethodGet, "/api/v1/data-retention/status", rig.session, ""); code != http.StatusInternalServerError {
		t.Fatalf("status failure = %d", code)
	}

	// The generic settings routes leave it to its own route.
	if code, body := call(http.MethodPut, "/api/v1/settings/data_retention", rig.session, retentionBody); code != http.StatusBadRequest || !strings.Contains(body, ErrorSettingHasOwnRoute) {
		t.Fatalf("generic put = %d %s", code, body)
	}
	if err := rig.st.SetSetting(store.SettingDataRetention, retentionBody); err != nil {
		t.Fatal(err)
	}
	if code, body := call(http.MethodGet, "/api/v1/settings", rig.session, ""); code != http.StatusOK || strings.Contains(body, "data_retention") {
		t.Fatalf("generic get = %d %s", code, body)
	}

	// Without the service: 404.
	bare := Router(Deps{Store: st, AuthSigner: signer})
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/data-retention", ""}, {http.MethodPut, "/api/v1/data-retention", retentionBody}, {http.MethodGet, "/api/v1/data-retention/status", ""},
	} {
		if rec := telemetryCall(bare, tc.method, tc.path, rig.session, tc.body); rec.Code != http.StatusNotFound {
			t.Errorf("%s %s without the service = %d", tc.method, tc.path, rec.Code)
		}
	}
}
