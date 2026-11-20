package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

func getSignals(t *testing.T, deps func(*Deps), path string, withToken bool) *httptest.ResponseRecorder {
	t.Helper()
	st, signer, token := monitorTestToken(t)
	d := Deps{Store: st, AuthSigner: signer}
	deps(&d)
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if withToken {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	Router(d).ServeHTTP(rec, req)
	return rec
}

func TestMonitorSignalsRequiresAuthentication(t *testing.T) {
	rec := getSignals(t, func(d *Deps) { d.Monitor = &fakeMonitorService{} }, "/api/v1/monitor/deployments/7/signals?start=1000&end=2000", false)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d body=%s, want 401", rec.Code, rec.Body.String())
	}
}

func TestMonitorSignalsWireShapeAndArguments(t *testing.T) {
	fake := &fakeMonitorService{signals: dto.MonitorDeploymentSignals{
		StartMS: 1_000_000, EndMS: 2_000_000, StepMS: 30_000,
		Sources:     map[string]string{"prompt_tokens": "gateway", "running": "engine"},
		Concurrency: []dto.MonitorSeries{{Labels: map[string]string{"metric": "running", "instance": "i1"}, Points: []dto.Point{{1_000_000, 2}}}},
		KV:          []dto.MonitorSeries{},
		Throughput:  []dto.MonitorSeries{{Labels: map[string]string{"metric": "prompt_tok_s"}, Points: []dto.Point{{1_030_000, 12.5}}}},
	}}
	rec := getSignals(t, func(d *Deps) { d.Monitor = fake }, "/api/v1/monitor/deployments/7/signals?start=1000000&end=2000000&step=30s", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	const want = `{"start_ms":1000000,"end_ms":2000000,"step_ms":30000,` +
		`"sources":{"prompt_tokens":"gateway","running":"engine"},` +
		`"concurrency":[{"labels":{"instance":"i1","metric":"running"},"points":[[1000000,2]]}],` +
		`"kv":[],` +
		`"throughput":[{"labels":{"metric":"prompt_tok_s"},"points":[[1030000,12.5]]}]}`
	if got := rec.Body.String(); got != want+"\n" && got != want {
		t.Fatalf("body = %s\nwant   %s", got, want)
	}
	if len(fake.signalsArgs) != 1 || fake.signalsArgs[0] != (signalsCall{7, 1_000_000, 2_000_000, 30 * time.Second}) {
		t.Fatalf("Signals called with %+v", fake.signalsArgs)
	}
}

func TestMonitorSignalsEndDefaultsToNowAndStepToServer(t *testing.T) {
	fake := &fakeMonitorService{}
	before := time.Now().UnixMilli()
	rec := getSignals(t, func(d *Deps) { d.Monitor = fake }, "/api/v1/monitor/deployments/7/signals?start=1000", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	call := fake.signalsArgs[0]
	if call.endMS < before || call.endMS > time.Now().UnixMilli() || call.startMS != 1000 || call.step != 0 {
		t.Fatalf("Signals called with %+v, want end=now, step left to the server", call)
	}
}

func TestMonitorSignalsRejectsBadQueries(t *testing.T) {
	for name, tc := range map[string]struct{ path, code string }{
		"id":            {"/api/v1/monitor/deployments/x/signals?start=1000", ErrorDeploymentIDInvalid},
		"no start":      {"/api/v1/monitor/deployments/7/signals", ErrorMonitorRangeInvalid},
		"start not int": {"/api/v1/monitor/deployments/7/signals?start=abc", ErrorMonitorRangeInvalid},
		"start>=end":    {"/api/v1/monitor/deployments/7/signals?start=2000&end=2000", ErrorMonitorRangeInvalid},
		"bad end":       {"/api/v1/monitor/deployments/7/signals?start=1000&end=-5", ErrorMonitorRangeInvalid},
		"bad step":      {"/api/v1/monitor/deployments/7/signals?start=1000&end=2000&step=fast", ErrorMonitorRangeInvalid},
		"zero step":     {"/api/v1/monitor/deployments/7/signals?start=1000&end=2000&step=0s", ErrorMonitorRangeInvalid},
	} {
		fake := &fakeMonitorService{}
		rec := getSignals(t, func(d *Deps) { d.Monitor = fake }, tc.path, true)
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != http.StatusBadRequest || body["code"] != tc.code {
			t.Errorf("%s: status %d code %v body=%s, want 400 %s", name, rec.Code, body["code"], rec.Body.String(), tc.code)
		}
		if len(fake.signalsArgs) != 0 {
			t.Errorf("%s: a refused query reached the service: %+v", name, fake.signalsArgs)
		}
	}
}

func TestMonitorSignalsErrorMapping(t *testing.T) {
	path := "/api/v1/monitor/deployments/7/signals?start=1000&end=2000"
	if rec := getSignals(t, func(d *Deps) { d.Monitor = &fakeMonitorService{signalsErr: ErrMonitorDeploymentNotFound} }, path, true); rec.Code != http.StatusNotFound {
		t.Errorf("unknown deployment: status %d body=%s, want 404", rec.Code, rec.Body.String())
	}
	if rec := getSignals(t, func(d *Deps) { d.Monitor = &fakeMonitorService{signalsErr: errors.New("tsdb unavailable")} }, path, true); rec.Code != http.StatusInternalServerError {
		t.Errorf("service failure: status %d body=%s, want 500", rec.Code, rec.Body.String())
	}
	if rec := getSignals(t, func(*Deps) {}, path, true); rec.Code != http.StatusNotFound {
		t.Errorf("no monitor wired: status %d body=%s, want 404", rec.Code, rec.Body.String())
	}
}
