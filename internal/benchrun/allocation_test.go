package benchrun

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/bench/dataset"
	"gpuworks.ai/gpuworks/pkg/bench/loadgen"
)

func TestStoredRunRejectsImpossibleAllocations(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	for _, raw := range []string{
		`{"concurrency":1,"isl":[1],"osl":[1],"requests_per_point_multiplier":1e308}`,
		fmt.Sprintf(`{"concurrency":1,"isl":[1],"osl":[1],"requests_per_point_multiplier":%d}`, maxInt/2),
		fmt.Sprintf(`{"concurrency":1,"isl":[1],"sessions_per_point":{"1":%d}}`, maxInt),
		fmt.Sprintf(`{"concurrency":1,"isl":[%d],"requests_per_point_multiplier":1}`, maxInt/4+1),
		fmt.Sprintf(`{"concurrency":1,"isl":[1],"warmup_requests":%d}`, maxInt),
		`{"request_rates":[1e308],"isl":[1],"point_duration_s":60}`,
	} {
		t.Run(raw, func(t *testing.T) {
			endpoint := &fakeEndpoint{}
			srv := endpoint.serve(t)
			rec := &recorder{}
			Execute(context.Background(), Run{ID: 1, Model: "m", Mode: "grid", Client: loadgen.Client{BaseURL: srv.URL}, Dataset: json.RawMessage(`{"kind":"random"}`), Search: json.RawMessage(raw)}, rec.hooks())
			if last := rec.last(); last.State != "failed" || last.StopReason != proto.BenchmarkStopFatal || last.Message == "" {
				t.Fatalf("terminal=%+v", last)
			}
			if len(endpoint.requests()) != 0 {
				t.Fatal("invalid stored run issued HTTP requests")
			}
		})
	}
}

func TestStoredRunUsesGridConcurrencyAndBoundsWorkersBySessions(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	for _, raw := range []string{
		`{"concurrency":1,"fixed_concurrency":[2],"isl":[1],"osl":[1],"requests_per_point_multiplier":1e308,"sessions_per_point":{"1":1}}`,
		fmt.Sprintf(`{"concurrency":%d,"isl":[1],"osl":[1],"sessions_per_point":{"%d":1}}`, maxInt, maxInt),
	} {
		t.Run(raw, func(t *testing.T) {
			endpoint := &fakeEndpoint{}
			srv := endpoint.serve(t)
			rec := &recorder{}
			Execute(context.Background(), Run{ID: 1, Model: "m", Mode: "grid", Client: loadgen.Client{BaseURL: srv.URL}, Dataset: json.RawMessage(`{"kind":"random"}`), Search: json.RawMessage(raw)}, rec.hooks())
			if last := rec.last(); last.State != "done" {
				t.Fatalf("terminal=%+v", last)
			}
			if len(endpoint.requests()) != 1 || len(rec.points(t)) != 1 || rec.points(t)[0].Success != 1 {
				t.Fatalf("requests=%d points=%+v", len(endpoint.requests()), rec.points(t))
			}
		})
	}
}

func TestStoredRunsUseSharedAllocationBound(t *testing.T) {
	count := int(dataset.MaxAllocationBytes/uint64(reflect.TypeFor[loadgen.Request]().Size()+reflect.TypeFor[loadgen.Result]().Size()) + 1)
	for _, kind := range []string{"random", "imported", "multiturn"} {
		t.Run(kind, func(t *testing.T) {
			dc := dataset.Config{Kind: kind, InputTokens: 1, Samples: []dataset.Sample{{Messages: []dataset.Message{{Role: "user", Content: "hi"}}}}}
			if err := dataset.ValidateAllocation(dc, count); err != nil {
				t.Fatalf("fixture must pass dataset-only bound: %v", err)
			}
			want := ValidateAllocation(dc, count)
			if want == nil {
				t.Fatal("shared preflight accepted impossible runner allocation")
			}
			raw, _ := json.Marshal(dc)
			for _, field := range []string{"sessions_per_point", "warmup_requests"} {
				config := fmt.Sprintf(`{"concurrency":1,"isl":[1],"sessions_per_point":{"1":%d}}`, count)
				if field == "warmup_requests" {
					config = fmt.Sprintf(`{"concurrency":1,"isl":[1],"warmup_requests":%d}`, count)
				}
				rec := &recorder{}
				Execute(context.Background(), Run{ID: 1, Model: "m", Mode: "grid", Dataset: raw, Search: json.RawMessage(config)}, rec.hooks())
				if last := rec.last(); last.State != "failed" || last.StopReason != proto.BenchmarkStopFatal || last.Message != want.Error() {
					t.Fatalf("%s terminal=%+v want=%v", field, last, want)
				}
			}
		})
	}
}
