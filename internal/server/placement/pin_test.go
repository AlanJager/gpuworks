package placement

import (
	"context"
	"reflect"
	"testing"
)

func TestPinCountAcceptsOnlyTheTestedShapeAndKeepsTheSourcesAnswer(t *testing.T) {
	source := "def resolve(gpu):\n\t# memory first\n\tif gpu.min_memory_gib < 32:\n\t\treturn reject(\"small\")\n\treturn accept(gpu_memory_mib_per_device=32768, variables={\"SP_DEGREE\": 1}, prefer=gpu.count)\n"
	for _, tc := range []struct {
		name, source string
		want         Result
	}{
		{"no source", "", Result{Variables: map[string]any{}}},
		{"source resolver", source, Result{Variables: map[string]any{"SP_DEGREE": int64(1)}, GPUMemoryMiBPerDevice: 32768, Prefer: 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pinned, err := PinCount(tc.source, 2, "tested on 2")
			if err != nil {
				t.Fatal(err)
			}
			got, err := Resolve(context.Background(), pinned, GPU{Count: 2, NNodes: 1, MinMemoryGiB: 80, MemoriesGiB: []float64{80, 80}})
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("tested shape = %+v %v, want %+v", got, err, tc.want)
			}
			for _, shape := range []GPU{{Count: 1, NNodes: 1, MinMemoryGiB: 80}, {Count: 4, NNodes: 1, MinMemoryGiB: 80}, {Count: 2, NNodes: 2, MinMemoryGiB: 80}} {
				if got, err := Resolve(context.Background(), pinned, shape); err != nil || got.Reason != "tested on 2" {
					t.Fatalf("%+v = %+v %v, want the pin's rejection", shape, got, err)
				}
			}
		})
	}
	if got, err := Resolve(context.Background(), mustPin(t, source), GPU{Count: 2, NNodes: 1, MinMemoryGiB: 16}); err != nil || got.Reason != "small" {
		t.Fatalf("source rejection on the tested shape = %+v %v, want the source's own", got, err)
	}
}

func TestPinCountRefusesAResolverItCannotGuard(t *testing.T) {
	for name, source := range map[string]string{
		"body on the def line": "def resolve(gpu): return {}\n",
		"not a resolver":       "x = 1\n",
	} {
		if _, err := PinCount(source, 1, "r"); err == nil {
			t.Errorf("%s: pinned", name)
		}
	}
}

func mustPin(t *testing.T, source string) string {
	t.Helper()
	pinned, err := PinCount(source, 2, "tested on 2")
	if err != nil {
		t.Fatal(err)
	}
	return pinned
}
