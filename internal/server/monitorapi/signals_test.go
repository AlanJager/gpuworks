package monitorapi

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/monitor"
)

var errSignalsNotFound = errors.New("signals: not found")

func signalsService(t *testing.T) (*Service, func(metric string, labels map[string]string, step time.Duration, values []float64)) {
	t.Helper()
	store := newTestStore(t)
	svc := New(Deps{
		TSDB: store, NotFound: errSignalsNotFound, Now: func() time.Time { return baseTime },
		Inventory: func(context.Context) (monitor.Inventory, error) {
			return monitor.Inventory{
				Deployments: []monitor.DeploymentRef{{ID: 1, Name: "pico", Engine: "callable"}, {ID: 2, Name: "other", Engine: "vllm"}},
				Instances: []monitor.InstanceRef{
					{UUID: "i1", DeploymentID: 1, State: "running"},
					{UUID: "j1", DeploymentID: 2, State: "running"},
				},
			}, nil
		},
	})
	write := func(metric string, labels map[string]string, step time.Duration, values []float64) {
		start := baseTime.Add(-time.Duration(len(values)-1) * step)
		for i, v := range values {
			writeSample(t, store, metric, labels, v, start.Add(time.Duration(i)*step))
		}
	}
	return svc, write
}

func rampValues(n int, perTick float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = perTick * float64(i)
	}
	return out
}

func flat(n int, v float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = v
	}
	return out
}

// The wire shape for the deployment the drawer showed empty: running,
// waiting and cache from the engine per instance (cache as a percentage),
// tokens from the gateway for the deployment as a whole.
func TestSignalsWireShapeForAnEngineWithoutTokenCounters(t *testing.T) {
	svc, write := signalsService(t)
	step := 10 * time.Second
	engine := map[string]string{"deployment": "1", "instance": "i1", "node": "n1"}
	gateway := map[string]string{"deployment": "1", "node": "server"}
	write("gpuworks_infer_requests_running", engine, step, flat(4, 2))
	write("gpuworks_infer_requests_waiting", engine, step, flat(4, 1))
	write("gpuworks_infer_gpu_cache_usage_percent", engine, step, flat(4, 0.5))
	write("gpuworks_proxy_prompt_tokens_total", gateway, step, rampValues(4, 100))
	write("gpuworks_proxy_completion_tokens_total", gateway, step, rampValues(4, 50))
	// Another deployment's gateway traffic must not leak in.
	write("gpuworks_proxy_completion_tokens_total", map[string]string{"deployment": "2", "node": "server"}, step, rampValues(4, 9999))

	start := baseTime.Add(-30 * time.Second)
	got, err := svc.Signals(context.Background(), 1, start.UnixMilli(), baseTime.UnixMilli(), step)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(got)
	var wire map[string]any
	_ = json.Unmarshal(raw, &wire)
	if wire["start_ms"] != float64(start.UnixMilli()) || wire["end_ms"] != float64(baseTime.UnixMilli()) || wire["step_ms"] != float64(10_000) {
		t.Errorf("window = %v %v %v", wire["start_ms"], wire["end_ms"], wire["step_ms"])
	}
	wantSources := map[string]string{"running": "engine", "prompt_tokens": "gateway", "generation_tokens": "gateway"}
	srcs, _ := wire["sources"].(map[string]any)
	if len(srcs) != len(wantSources) {
		t.Fatalf("sources = %v, want %v", srcs, wantSources)
	}
	for k, v := range wantSources {
		if srcs[k] != v {
			t.Errorf("sources[%s] = %v, want %s", k, srcs[k], v)
		}
	}
	lines := func(group string) map[string][]map[string]any {
		out := map[string][]map[string]any{}
		for _, item := range wire[group].([]any) {
			line := item.(map[string]any)
			metric := line["labels"].(map[string]any)["metric"].(string)
			out[metric] = append(out[metric], line)
		}
		return out
	}
	last := func(line map[string]any) float64 {
		points := line["points"].([]any)
		return points[len(points)-1].([]any)[1].(float64)
	}
	conc := lines("concurrency")
	if len(conc["running"]) != 1 || conc["running"][0]["labels"].(map[string]any)["instance"] != "i1" || last(conc["running"][0]) != 2 {
		t.Errorf("running = %v", conc["running"])
	}
	if len(conc["waiting"]) != 1 || last(conc["waiting"][0]) != 1 {
		t.Errorf("waiting = %v", conc["waiting"])
	}
	kv := lines("kv")
	if len(kv["usage_pct"]) != 1 || last(kv["usage_pct"][0]) != 50 {
		t.Errorf("kv = %v, want the engine's 0.5 as 50 percent", kv)
	}
	thr := lines("throughput")
	if len(thr["generation_tok_s"]) != 1 || last(thr["generation_tok_s"][0]) != 5 {
		t.Errorf("generation_tok_s = %v, want the gateway's 5", thr["generation_tok_s"])
	}
	if _, named := thr["generation_tok_s"][0]["labels"].(map[string]any)["instance"]; named {
		t.Errorf("a gateway line names an instance: %v", thr["generation_tok_s"][0])
	}
	if last(thr["prompt_tok_s"][0]) != 10 {
		t.Errorf("prompt_tok_s = %v, want the gateway's 10", thr["prompt_tok_s"])
	}
}

func TestSignalsAreEmptyListsNotNullWithoutData(t *testing.T) {
	svc, _ := signalsService(t)
	got, err := svc.Signals(context.Background(), 1, baseTime.Add(-time.Minute).UnixMilli(), baseTime.UnixMilli(), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(got)
	var wire map[string]json.RawMessage
	_ = json.Unmarshal(raw, &wire)
	for _, k := range []string{"concurrency", "kv", "throughput"} {
		if string(wire[k]) != "[]" {
			t.Errorf("%s = %s, want []", k, wire[k])
		}
	}
	if string(wire["sources"]) != "{}" {
		t.Errorf("sources = %s, want {}", wire["sources"])
	}
}

func TestSignalsRaiseAStepThatWouldHoldTooManyPoints(t *testing.T) {
	svc, _ := signalsService(t)
	end := baseTime.UnixMilli()
	for name, tc := range map[string]struct {
		window int64
		step   time.Duration
		want   int64
	}{
		"as asked":       {10 * 60_000, 10 * time.Second, 10_000},
		"no step":        {10 * 60_000, 0, 1_000},
		"year at a day":  {365 * 24 * 3_600_000, 24 * time.Hour, 24 * 3_600_000},
		"month at 10s":   {30 * 24 * 3_600_000, 10 * time.Second, 30 * 24 * 3_600_000 / maxSignalPoints},
		"a sub-second":   {10 * 60_000, time.Millisecond, 1_000},
		"just too fine":  {maxSignalPoints * 30_000, 10 * time.Second, 30_000},
		"exactly points": {maxSignalPoints * 30_000, 30 * time.Second, 30_000},
	} {
		got, err := svc.Signals(context.Background(), 1, end-tc.window, end, tc.step)
		if err != nil {
			t.Fatal(err)
		}
		if got.StepMS != tc.want {
			t.Errorf("%s: step_ms = %d, want %d", name, got.StepMS, tc.want)
		}
	}
}

func TestSignalsUnknownDeploymentIsTheInjectedNotFound(t *testing.T) {
	svc, _ := signalsService(t)
	if _, err := svc.Signals(context.Background(), 99, 0, 60_000, time.Second); !errors.Is(err, errSignalsNotFound) {
		t.Fatalf("err = %v, want the injected not-found", err)
	}
}

func TestSignalsInventoryFailureIsReturned(t *testing.T) {
	boom := errors.New("inventory down")
	svc := New(Deps{Inventory: func(context.Context) (monitor.Inventory, error) { return monitor.Inventory{}, boom }})
	if _, err := svc.Signals(context.Background(), 1, 0, 60_000, time.Second); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the inventory's", err)
	}
}
