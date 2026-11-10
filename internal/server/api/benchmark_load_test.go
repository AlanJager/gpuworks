package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// benchmarkLoadRouter serves the benchmark API with an agent "agent-1"
// advertising caps; its models probe answers like a real endpoint.
func benchmarkLoadRouter(t *testing.T, name string, caps []string) (http.Handler, *store.Store, string) {
	t.Helper()
	st, err := store.Open("file:benchmark_load_"+name+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	signer, _ := auth.NewTokenSigner("benchmark-load-"+name, time.Hour)
	token, _ := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	h := Router(Deps{
		Store: st, Bus: bus.New(), AuthSigner: signer,
		NodeCaps: func(uuid string) []string {
			if uuid == "agent-1" {
				return caps
			}
			return nil
		},
		CallNode: func(_ context.Context, uuid, method string, _ any) (json.RawMessage, error) {
			if uuid != "agent-1" || method != proto.RPCBenchProbe {
				return nil, fmt.Errorf("unexpected call %s %s", uuid, method)
			}
			return json.RawMessage(`{"data":[{"id":"m"}]}`), nil
		},
		NodeSync: nodeSyncCallbacks{full: func(string) error { return nil }},
	})
	return h, st, token
}

func postBenchmark(t *testing.T, h http.Handler, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/benchmarks", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func agentBenchmarkBody(mode, search string) string {
	return `{"name":"load","target_kind":"external","base_url":"http://endpoint.test:8000","model":"m","load_origin":"agent-1","dataset":{"kind":"random","input_tokens":8,"output_tokens":4,"ignore_eos":true},"mode":"` + mode + `","search":` + search + `}`
}

var currentBenchCaps = []string{proto.CapBench, proto.CapBenchWarmup, proto.CapBenchArrival, proto.CapBenchInteractivity, proto.CapBenchEvidence, proto.CapBenchRequestTimeout}

func TestCreateBenchmarkValidatesLoadSettings(t *testing.T) {
	h, _, token := benchmarkLoadRouter(t, "validate", currentBenchCaps)
	for _, tc := range []struct {
		name, mode, search, code, field string
	}{
		{"open loop outside grid", "saturation", `{"concurrency":1,"request_rates":[1]}`, ErrorBenchmarkOpenLoopMode, ""},
		{"open loop in sla", "sla", `{"concurrency":1,"request_rates":[1],"min_interactivity":20}`, ErrorBenchmarkOpenLoopMode, ""},
		{"zero rate", "grid", `{"request_rates":[0]}`, ErrorBenchmarkLoadValueInvalid, "request_rates"},
		{"negative rate", "grid", `{"request_rates":[1,-2]}`, ErrorBenchmarkLoadValueInvalid, "request_rates"},
		{"rate above max", "grid", `{"request_rates":[10001]}`, ErrorBenchmarkLoadValueInvalid, "request_rates"},
		{"too many rates", "grid", `{"request_rates":[` + strings.Repeat("1,", 32) + `1]}`, ErrorBenchmarkLoadValueInvalid, "request_rates"},
		{"negative burstiness", "grid", `{"request_rates":[1],"burstiness":-1}`, ErrorBenchmarkLoadValueInvalid, "burstiness"},
		{"short point", "grid", `{"request_rates":[1],"point_duration_s":1}`, ErrorBenchmarkLoadValueInvalid, "point_duration_s"},
		{"negative cap", "grid", `{"request_rates":[1],"max_in_flight":-1}`, ErrorBenchmarkLoadValueInvalid, "max_in_flight"},
		{"too much warmup", "grid", `{"concurrency":[1],"warmup_requests":1001}`, ErrorBenchmarkLoadValueInvalid, "warmup_requests"},
		{"session floor in saturation", "saturation", `{"concurrency":1,"min_session_completion":0.95}`, ErrorBenchmarkParameterInvalid, "search.min_session_completion"},
		{"negative session floor", "sla", `{"concurrency":1,"min_session_completion":-0.1}`, ErrorBenchmarkLoadValueInvalid, "min_session_completion"},
		{"session floor above one", "sla", `{"concurrency":1,"min_session_completion":1.01}`, ErrorBenchmarkLoadValueInvalid, "min_session_completion"},
		{"negative warmup", "saturation", `{"concurrency":1,"warmup_requests":-1}`, ErrorBenchmarkLoadValueInvalid, "warmup_requests"},
		{"request timeout below min", "grid", `{"concurrency":[1],"request_timeout_s":9}`, ErrorBenchmarkLoadValueInvalid, "request_timeout_s"},
		{"request timeout above max", "grid", `{"concurrency":[1],"request_timeout_s":7201}`, ErrorBenchmarkLoadValueInvalid, "request_timeout_s"},
		{"negative request timeout", "saturation", `{"concurrency":1,"request_timeout_s":-1}`, ErrorBenchmarkLoadValueInvalid, "request_timeout_s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := postBenchmark(t, h, token, agentBenchmarkBody(tc.mode, tc.search))
			var body struct {
				Code   string         `json:"code"`
				Params map[string]any `json:"params"`
			}
			_ = json.Unmarshal(w.Body.Bytes(), &body)
			if w.Code != http.StatusBadRequest || body.Code != tc.code || (tc.field != "" && body.Params["field"] != tc.field) {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
	w := postBenchmark(t, h, token, agentBenchmarkBody("grid", `"not an object"`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("malformed search: status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestCreateBenchmarkGatesArrivalAndWarmupOnAgentCapability(t *testing.T) {
	openLoop := `{"request_rates":[1,2],"burstiness":0.5,"max_in_flight":8,"point_duration_s":30,"isl":[8],"osl":[4],"warmup_requests":1}`
	closedWithWarmup := `{"concurrency":[1,2],"isl":[8],"osl":[4],"warmup_requests":1}`
	slaFloor := `{"concurrency":1,"max_concurrency":8,"isl":[8],"osl":[4],"min_interactivity":30}`
	oldAgent := []string{proto.CapBench}
	currentAgent := currentBenchCaps
	for i, tc := range []struct {
		name, mode, search string
		caps               []string
		status             int
		code               string
		warnings           string
	}{
		{name: "session floor on an old agent", mode: "sla", search: `{"concurrency":1,"min_session_completion":0.95}`, caps: oldAgent, status: http.StatusBadRequest, code: ErrorBenchmarkAgentEvidenceUnsupported},
		{name: "session floor on a current agent", mode: "sla", search: `{"concurrency":1,"min_session_completion":0.95}`, caps: currentAgent, status: http.StatusAccepted},
		{name: "open loop on an old agent", search: openLoop, caps: oldAgent, status: http.StatusBadRequest, code: ErrorBenchmarkAgentArrivalUnsupported},
		// An older agent would search past a floor it never checks.
		{name: "interactivity floor on an old agent", mode: "sla", search: slaFloor, caps: oldAgent, status: http.StatusBadRequest, code: ErrorBenchmarkAgentInteractivityUnsupported},
		{name: "interactivity floor on a current agent", mode: "sla", search: slaFloor, caps: currentAgent, status: http.StatusAccepted},
		// Each feature is gated on its own capability.
		{name: "warmup without the warmup capability", search: closedWithWarmup, caps: []string{proto.CapBench, proto.CapBenchArrival, proto.CapBenchInteractivity, proto.CapBenchEvidence, proto.CapBenchRequestTimeout}, status: http.StatusAccepted, warnings: `["` + proto.BenchmarkWarningWarmupUnsupported + `"]`},
		// An agent without the capability bounds every request by five
		// minutes, which differs from the default and any other timeout.
		{name: "default request timeout without the capability", search: `{"concurrency":[1]}`, caps: []string{proto.CapBench, proto.CapBenchWarmup, proto.CapBenchArrival, proto.CapBenchInteractivity, proto.CapBenchEvidence}, status: http.StatusAccepted, warnings: `["` + proto.BenchmarkWarningRequestTimeoutUnsupported + `"]`},
		{name: "five-minute request timeout on an old agent", search: `{"concurrency":[1],"request_timeout_s":300}`, caps: oldAgent, status: http.StatusAccepted},
		{name: "long request timeout on a current agent", search: `{"concurrency":[1],"request_timeout_s":1800}`, caps: currentAgent, status: http.StatusAccepted},
		{name: "open loop without the arrival capability", search: openLoop, caps: []string{proto.CapBench, proto.CapBenchWarmup}, status: http.StatusBadRequest, code: ErrorBenchmarkAgentArrivalUnsupported},
		// Warmup only buys the first point its cold start; an old agent runs
		// without it and the run says so.
		{name: "warmup on an old agent", search: closedWithWarmup, caps: oldAgent, status: http.StatusAccepted, warnings: `["` + proto.BenchmarkWarningWarmupUnsupported + `","` + proto.BenchmarkWarningRequestTimeoutUnsupported + `"]`},
		{name: "no warmup on an old agent", search: `{"concurrency":[1],"warmup_requests":0,"request_timeout_s":300}`, caps: oldAgent, status: http.StatusAccepted},
		{name: "open loop on a current agent", search: openLoop, caps: currentAgent, status: http.StatusAccepted},
		{name: "warmup on a current agent", search: closedWithWarmup, caps: currentAgent, status: http.StatusAccepted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, st, token := benchmarkLoadRouter(t, fmt.Sprintf("gate_%d", i), tc.caps)
			mode := tc.mode
			if mode == "" {
				mode = "grid"
			}
			w := postBenchmark(t, h, token, agentBenchmarkBody(mode, tc.search))
			if w.Code != tc.status || (tc.code != "" && !strings.Contains(w.Body.String(), `"code":"`+tc.code+`"`)) {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if tc.status != http.StatusAccepted {
				if !strings.Contains(w.Body.String(), `"node":"agent-1"`) {
					t.Fatalf("refusal does not name the agent: %s", w.Body.String())
				}
				return
			}
			var created struct{ ID uint }
			_ = json.Unmarshal(w.Body.Bytes(), &created)
			run, err := st.GetBenchmarkRun(created.ID)
			if err != nil || run.Warnings != tc.warnings || run.Search != tc.search || !strings.Contains(run.Dataset, `"ignore_eos":true`) {
				t.Fatalf("stored run = %+v err=%v", run, err)
			}
		})
	}
}

func TestBenchmarkDetailExposesPointRequestRate(t *testing.T) {
	h, st, token := benchmarkLoadRouter(t, "detail_rate", nil)
	run, err := st.CreateBenchmarkRun(&store.BenchmarkRun{Name: "rates", LoadOrigin: "server", Mode: "grid", Search: `{"request_rates":[2.5]}`})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []store.BenchmarkPoint{{RunID: run.ID, Seq: 1, State: "done", RequestRate: 2.5, Concurrency: 0}, {RunID: run.ID, Seq: 2, State: "done", Concurrency: 4}} {
		if err := st.UpsertBenchmarkPoint(&p); err != nil {
			t.Fatal(err)
		}
	}
	r := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/benchmarks/%d", run.ID), nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	body := w.Body.String()
	if w.Code != http.StatusOK || !strings.Contains(body, `"seq":1,"concurrency":0,"target_isl":0,"target_osl":0,"request_rate":2.5,`) || !strings.Contains(body, `"seq":2,"concurrency":4,"target_isl":0,"target_osl":0,"request_rate":0,`) {
		t.Fatalf("status=%d body=%s", w.Code, body)
	}
}
