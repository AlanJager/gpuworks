package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/store"
	benchmeta "gpuworks.ai/gpuworks/pkg/bench"
	"gpuworks.ai/gpuworks/pkg/bench/dataset"
	"gpuworks.ai/gpuworks/pkg/bench/loadgen"
	"gpuworks.ai/gpuworks/pkg/bench/search"
)

func TestCreateBenchmarkRejectsInvalidParametersBeforeAdmission(t *testing.T) {
	h, st, token := benchmarkLoadRouter(t, "invalid_parameters", currentBenchCaps)
	for _, tc := range []struct{ name, old, replacement string }{
		{"concurrency type", `"concurrency":1`, `"concurrency":"oops"`},
		{"negative concurrency", `"concurrency":1`, `"concurrency":-2`},
		{"null search", `"search":{"concurrency":1}`, `"search":null`},
		{"unknown mode", `"mode":"grid"`, `"mode":"invalid"`},
		{"unknown target", `"target_kind":"external"`, `"target_kind":"invalid"`},
		{"unknown dataset", `"kind":"random"`, `"kind":"invalid"`},
		{"negative input length", `"input_tokens":8`, `"input_tokens":-1`},
		{"empty model", `"model":"m"`, `"model":" "`},
		{"negative detail cap", `"name":"load"`, `"name":"load","detail_cap_mb":-1`},
		{"null dataset", `"dataset":{"kind":"random","input_tokens":8,"output_tokens":4,"ignore_eos":true}`, `"dataset":null`},
		{"empty imported dataset", `"kind":"random"`, `"kind":"imported"`},
		{"invalid agentic preset", `"kind":"random"`, `"kind":"agentic","preset":"invalid"`},
		{"negative output length", `"output_tokens":4`, `"output_tokens":-1`},
		{"negative turns", `"kind":"random"`, `"kind":"multiturn","turns":-1`},
		{"imported message without role", `"kind":"random"`, `"kind":"imported","samples":[{"messages":[{"content":"hi"}]}]`},
		{"imported follow-up without content", `"kind":"random"`, `"kind":"imported","samples":[{"messages":[{"role":"user","content":"hi"}],"follow_ups":[{"role":"user"}]}]`},
		{"extra JSON document", `"search":{"concurrency":1}}`, `"search":{"concurrency":1}} {}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := strings.Replace(agentBenchmarkBody("grid", `{"concurrency":1}`), tc.old, tc.replacement, 1)
			w := postBenchmark(t, h, token, body)
			if w.Code != http.StatusBadRequest {
				t.Errorf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
	runs, err := st.ListBenchmarkRuns()
	if err != nil || len(runs) != 0 {
		t.Fatalf("invalid requests persisted %d runs, err=%v", len(runs), err)
	}
}

func TestCreateBenchmarkRejectsInvalidSearchFields(t *testing.T) {
	h, st, token := benchmarkLoadRouter(t, "invalid_search_fields", currentBenchCaps)
	for _, search := range []string{
		`{"concurrency":0}`, `{"concurrency":null}`, `{"concurrency":[]}`, `{"concurrency":[1,0]}`, `{"concurrency":[null]}`, `{"concurrency":1.5}`,
		`{"fixed_concurrency":[-1]}`, `{"isl":[-1]}`, `{"osl":[0]}`,
		`{"max_concurrency":-1}`, `{"concurrency":4,"max_concurrency":2}`,
		`{"min_throughput_gain":-1}`, `{"requests_per_point_multiplier":-1}`, `{"sessions_per_point":{"1":0}}`, `{"sessions_per_point":{"oops":2}}`,
		`{"max_avg_ttft":-1}`, `{"min_interactivity":-1}`, `{"min_rps":2,"max_rps":1}`, `{"min_tps":3,"max_tps":2}`, `{"objective":"invalid"}`,
		`{"burstiness":-1}`, `{"point_duration_s":-1}`, `{"max_in_flight":-1}`, `{"concurency":4}`,
	} {
		t.Run(search, func(t *testing.T) {
			w := postBenchmark(t, h, token, agentBenchmarkBody("saturation", search))
			if w.Code != http.StatusBadRequest {
				t.Errorf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
	runs, err := st.ListBenchmarkRuns()
	if err != nil || len(runs) != 0 {
		t.Fatalf("invalid requests persisted %d runs, err=%v", len(runs), err)
	}
}

func TestCreateBenchmarkPreservesSearchDefaultsAndSupportedShapes(t *testing.T) {
	for i, search := range []string{`{}`, `{"concurrency":2}`, `{"concurrency":[1,2]}`, `{"concurrency":1,"max_concurrency":0,"requests_per_point_multiplier":0,"min_throughput_gain":0}`, `{"fixed_concurrency":[1,2],"max_concurrency":2}`, `{"concurrency":1,"sessions_per_point":{"1":4}}`} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			h, st, token := benchmarkLoadRouter(t, fmt.Sprintf("valid_search_%d", i), currentBenchCaps)
			w := postBenchmark(t, h, token, agentBenchmarkBody("grid", search))
			if w.Code != http.StatusAccepted {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			var response struct {
				ID uint `json:"id"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			run, err := st.GetBenchmarkRun(response.ID)
			if err != nil || run.Search != search {
				t.Fatalf("run=%+v err=%v", run, err)
			}
		})
	}
}

func TestCreateBenchmarkValidatesBeforeProbeAndConflict(t *testing.T) {
	f := newBenchmarkAdmission(t, "benchmark_validation_priority")
	f.activeRun(t, store.BenchmarkRun{Name: "existing", LoadOrigin: "agent-1"})
	probes := 0
	f.handlers.d.CallNode = func(context.Context, string, string, any) (json.RawMessage, error) {
		probes++
		return nil, errors.New("endpoint offline")
	}
	for _, allow := range []bool{false, true} {
		body := strings.TrimSuffix(agentBenchmarkBody("grid", `{"concurrency":"oops"}`), "}") + fmt.Sprintf(`,"allow_conflict":%t}`, allow)
		w := f.create(t, body)
		var response struct {
			Code   string `json:"code"`
			Params struct {
				Field string `json:"field"`
			} `json:"params"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if w.Code != http.StatusBadRequest || response.Code != ErrorBenchmarkParameterInvalid || response.Params.Field != "search.concurrency" {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
	}
	if probes != 0 {
		t.Fatalf("invalid requests probed endpoint %d times", probes)
	}
	runs, err := f.store.ListBenchmarkRuns()
	if err != nil || len(runs) != 1 || runs[0].Name != "existing" {
		t.Fatalf("runs=%+v err=%v", runs, err)
	}
}

func TestCreateBenchmarkOmittedOptionsKeepDefaults(t *testing.T) {
	h, st, token := benchmarkLoadRouter(t, "validation_defaults", currentBenchCaps)
	w := postBenchmark(t, h, token, `{"base_url":"http://endpoint.test:8000","model":"m","load_origin":"agent-1"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var response benchmarkDTO
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	run, err := st.GetBenchmarkRun(response.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.TargetKind != "external" || run.Mode != "grid" || run.Search != `{"concurrency":1}` || run.DetailCapMB != 20 || run.Dataset != `{"kind":"random","input_tokens":4096,"output_tokens":1024}` || run.NumGPUs != 0 {
		t.Fatalf("defaults=%+v", run)
	}
	if strings.Contains(w.Body.String(), `"api_key"`) {
		t.Fatal("response exposed api_key")
	}
}

func TestCreateBenchmarkAcceptsDictionaryModeDefaults(t *testing.T) {
	for mode, defaults := range benchmeta.Dictionary().ModeDefaults {
		t.Run(mode, func(t *testing.T) {
			h, _, token := benchmarkLoadRouter(t, "dictionary_defaults_"+mode, currentBenchCaps)
			raw, err := json.Marshal(defaults)
			if err != nil {
				t.Fatal(err)
			}
			w := postBenchmark(t, h, token, agentBenchmarkBody(mode, string(raw)))
			if w.Code != http.StatusAccepted {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestCreateBenchmarkRejectsRequestCountOverflow(t *testing.T) {
	h, st, token := benchmarkLoadRouter(t, "request_count_overflow", currentBenchCaps)
	limit := math.Ldexp(1, strconv.IntSize-1)
	maxInt := int(^uint(0) >> 1)
	for _, tc := range []struct{ name, mode, config string }{
		{"huge multiplier", "grid", `{"concurrency":1,"requests_per_point_multiplier":1e308}`},
		{"default multiplier", "grid", fmt.Sprintf(`{"concurrency":%d}`, maxInt)},
		{"zero uses default multiplier", "grid", fmt.Sprintf(`{"concurrency":%d,"requests_per_point_multiplier":0}`, maxInt)},
		{"default concurrency", "grid", fmt.Sprintf(`{"requests_per_point_multiplier":%.0f}`, limit)},
		{"product reaches exclusive boundary", "grid", fmt.Sprintf(`{"concurrency":2,"requests_per_point_multiplier":%.0f}`, limit/2)},
		{"fixed points", "sla", fmt.Sprintf(`{"fixed_concurrency":[1,2],"requests_per_point_multiplier":%.0f}`, limit/2)},
		{"adaptive maximum", "saturation", fmt.Sprintf(`{"concurrency":1,"max_concurrency":2,"requests_per_point_multiplier":%.0f}`, limit/2)},
		{"adaptive default start", "sla", fmt.Sprintf(`{"max_concurrency":2,"requests_per_point_multiplier":%.0f}`, limit/2)},
		{"adaptive override leaves overflowing intermediate point", "sla", fmt.Sprintf(`{"concurrency":1,"max_concurrency":4,"requests_per_point_multiplier":%.0f,"sessions_per_point":{"4":2}}`, limit/2)},
		{"partial grid override", "grid", `{"concurrency":[1,2],"requests_per_point_multiplier":1e308,"sessions_per_point":{"1":2}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := postBenchmark(t, h, token, agentBenchmarkBody(tc.mode, tc.config))
			var response struct {
				Code   string `json:"code"`
				Params struct {
					Field string `json:"field"`
				} `json:"params"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if w.Code != http.StatusBadRequest || response.Code != ErrorBenchmarkParameterInvalid || response.Params.Field != "search.requests_per_point_multiplier" {
				t.Errorf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
	runs, err := st.ListBenchmarkRuns()
	if err != nil || len(runs) != 0 {
		t.Fatalf("overflowing requests persisted %d runs: %v", len(runs), err)
	}
	// A large unused multiplier is legal when explicit session counts provide
	// the actual request count, including through the HTTP admission boundary.
	w := postBenchmark(t, h, token, agentBenchmarkBody("grid", `{"concurrency":[1,2],"requests_per_point_multiplier":1e308,"sessions_per_point":{"1":2,"2":3}}`))
	if w.Code != http.StatusAccepted {
		t.Fatalf("explicit sessions: status=%d body=%s", w.Code, w.Body.String())
	}
}

// Exercise the same planner as benchrun, including ignored fields retained by
// UI edits and old API callers. Do not manufacture a point to match validation.
func TestBenchmarkAdmissionMatchesExecutedPoints(t *testing.T) {
	start := int(^uint(0)>>1)/2 + 1
	tinyCount := max(1, int(math.Ceil(float64(start)*1e-18)))
	for _, tc := range []struct {
		name, mode, config            string
		wantConcurrency, wantRequests []int
	}{
		{"large adaptive concurrency capped safely", "sla", fmt.Sprintf(`{"concurrency":%d,"max_concurrency":%d,"isl":[1],"osl":[1],"requests_per_point_multiplier":1e-18}`, start, start+1), []int{start, start + 1}, []int{tinyCount, tinyCount}},
		{"both defaults", "grid", `{}`, []int{1}, []int{10}},
		{"zero multiplier", "grid", `{"requests_per_point_multiplier":0}`, []int{1}, []int{10}},
		{"grid ignores fixed concurrency", "grid", `{"concurrency":1,"fixed_concurrency":[2],"requests_per_point_multiplier":1e308,"sessions_per_point":{"1":1}}`, []int{1}, []int{1}},
		{"grid defaults ignore fixed concurrency", "grid", `{"fixed_concurrency":[2],"requests_per_point_multiplier":1e308,"sessions_per_point":{"1":1}}`, []int{1}, []int{1}},
		{"fixed points ignore max", "sla", `{"fixed_concurrency":[1,4],"max_concurrency":2,"requests_per_point_multiplier":1}`, []int{1, 4}, []int{1, 4}},
		{"adaptive ignores later concurrency", "sla", `{"concurrency":[1,4],"max_concurrency":2,"requests_per_point_multiplier":1}`, []int{1, 2}, []int{1, 2}},
		{"adaptive ignores later input length", "sla", fmt.Sprintf(`{"concurrency":1,"max_concurrency":1,"isl":[1,%d],"requests_per_point_multiplier":1}`, int(^uint(0)>>1)), []int{1}, []int{1}},
		{"fixed overrides", "sla", `{"concurrency":3,"fixed_concurrency":[1,2],"max_concurrency":1,"requests_per_point_multiplier":1e308,"sessions_per_point":{"1":2,"2":3}}`, []int{1, 2}, []int{2, 3}},
		{"adaptive overrides", "sla", `{"max_concurrency":2,"requests_per_point_multiplier":1e308,"sessions_per_point":{"1":2,"2":3}}`, []int{1, 2}, []int{2, 3}},
		{"open loop ignores multiplier", "grid", `{"request_rates":[1],"requests_per_point_multiplier":1e308}`, []int{0}, []int{60}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, st, token := benchmarkLoadRouter(t, "effective_"+strings.ReplaceAll(tc.name, " ", "_"), currentBenchCaps)
			w := postBenchmark(t, h, token, agentBenchmarkBody(tc.mode, tc.config))
			if w.Code != http.StatusAccepted {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			runs, err := st.ListBenchmarkRuns()
			if err != nil || len(runs) != 1 {
				t.Fatalf("runs=%v err=%v", runs, err)
			}
			doc, err := search.ParseDocument([]byte(runs[0].Search))
			if err != nil {
				t.Fatal(err)
			}
			var points []search.Point
			if tc.mode == "grid" {
				points, err = doc.Expand()
				if err != nil {
					t.Fatal(err)
				}
			} else {
				planner, err := search.NewDocument(tc.mode, doc)
				if err != nil {
					t.Fatal(err)
				}
				for i := 0; i <= len(tc.wantConcurrency); i++ {
					p, ok := planner.Next()
					if !ok {
						break
					}
					points = append(points, p)
					planner.Observe(search.Observation{Point: p, SuccessRate: 1, Throughput: float64(p.Concurrency)})
				}
			}
			if len(points) != len(tc.wantConcurrency) {
				t.Fatalf("points=%+v", points)
			}
			for i, p := range points {
				n, err := doc.RequestsForPoint(p)
				if err != nil || n != tc.wantRequests[i] || p.Concurrency != tc.wantConcurrency[i] {
					t.Fatalf("point=%+v n=%d err=%v", p, n, err)
				}
				samples, err := dataset.Generate(dataset.Config{Kind: "random", InputTokens: p.ISL, OutputTokens: p.OSL}, n)
				if err != nil || len(samples) != n {
					t.Fatalf("generated=%d err=%v", len(samples), err)
				}
			}
		})
	}
}

func TestCreateBenchmarkRejectsUnexecutableAllocationsBeforeProbe(t *testing.T) {
	f := newBenchmarkAdmission(t, "benchmark_allocation_admission")
	probes := 0
	f.handlers.d.CallNode = func(context.Context, string, string, any) (json.RawMessage, error) {
		probes++
		return nil, errors.New("must not probe invalid allocation")
	}
	maxInt := int(^uint(0) >> 1)
	for _, tc := range []struct{ name, config, field string }{
		{"grid ignores override for fixed point", `{"concurrency":1,"fixed_concurrency":[2],"requests_per_point_multiplier":1e308,"sessions_per_point":{"2":1}}`, "search.requests_per_point_multiplier"},
		{"runner array bound", fmt.Sprintf(`{"concurrency":1,"isl":[1],"sessions_per_point":{"1":%d}}`, dataset.MaxAllocationBytes/uint64(reflect.TypeFor[loadgen.Request]().Size()+reflect.TypeFor[loadgen.Result]().Size())+1), "search.sessions_per_point"},
		{"sample size overflow", fmt.Sprintf(`{"concurrency":1,"isl":[1],"requests_per_point_multiplier":%d}`, maxInt/2), "search.requests_per_point_multiplier"},
		{"allocation beyond heap address space", fmt.Sprintf(`{"concurrency":1,"isl":[1],"requests_per_point_multiplier":%d}`, dataset.MaxAllocationBytes), "search.requests_per_point_multiplier"},
		{"session count override", fmt.Sprintf(`{"concurrency":1,"sessions_per_point":{"1":%d}}`, maxInt), "search.sessions_per_point"},
		{"prompt multiplication overflow", fmt.Sprintf(`{"concurrency":1,"isl":[%d],"requests_per_point_multiplier":1}`, maxInt/4+1), "search.isl"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := f.create(t, agentBenchmarkBody("grid", tc.config))
			var response struct {
				Code   string `json:"code"`
				Params struct {
					Field string `json:"field"`
				} `json:"params"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if w.Code != http.StatusBadRequest || response.Code != ErrorBenchmarkParameterInvalid || response.Params.Field != tc.field {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
	if probes != 0 {
		t.Fatalf("invalid allocations triggered %d probes", probes)
	}
	runs, err := f.store.ListBenchmarkRuns()
	if err != nil || len(runs) != 0 {
		t.Fatalf("invalid allocations persisted runs=%v err=%v", runs, err)
	}
}

func TestCreateBenchmarkSharesRunnerAllocationBound(t *testing.T) {
	f := newBenchmarkAdmission(t, "benchmark_shared_allocation")
	probes := 0
	f.handlers.d.CallNode = func(context.Context, string, string, any) (json.RawMessage, error) {
		probes++
		return nil, errors.New("invalid allocation probed")
	}
	count := dataset.MaxAllocationBytes/uint64(reflect.TypeFor[loadgen.Request]().Size()+reflect.TypeFor[loadgen.Result]().Size()) + 1
	for _, mode := range []string{"grid", "sla", "saturation"} {
		for _, kind := range []string{"random", "imported", "multiturn"} {
			t.Run(mode+"/"+kind, func(t *testing.T) {
				config := fmt.Sprintf(`{"concurrency":1,"max_concurrency":1,"isl":[1],"osl":[1],"sessions_per_point":{"1":%d}}`, count)
				body := agentBenchmarkBody(mode, config)
				replacement := fmt.Sprintf(`"kind":%q`, kind)
				if kind == "imported" {
					replacement += `,"samples":[{"messages":[{"role":"user","content":"hi"}]}]`
				}
				body = strings.Replace(body, `"kind":"random"`, replacement, 1)
				w := f.create(t, body)
				if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), ErrorBenchmarkParameterInvalid) || !strings.Contains(w.Body.String(), "search.sessions_per_point") {
					t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
				}
			})
		}
	}
	if probes != 0 {
		t.Fatalf("invalid allocation probed %d times", probes)
	}
	runs, err := f.store.ListBenchmarkRuns()
	if err != nil || len(runs) != 0 {
		t.Fatalf("persisted runs=%v err=%v", runs, err)
	}
}
