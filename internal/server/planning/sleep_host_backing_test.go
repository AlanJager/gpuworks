package planning

import (
	"errors"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestSleepHostBackingRefusal(t *testing.T) {
	const g = int64(1) << 30
	host := func(available, total int64, ok bool) HostMemory {
		return func(uuid string) (int64, int64, bool) {
			if uuid != "node-uuid" {
				t.Fatalf("host memory read for %q", uuid)
			}
			return available, total, ok
		}
	}
	hygon := &store.Node{UUID: "node-uuid", Name: "haiguang", GPUVendor: "hygon"}
	pool := 576 * g / 10

	err := SleepHostBackingRefusal(hygon, pool, host(555*g/10, 624*g/10, true))
	var product *messages.ProductError
	if !errors.As(err, &product) || product.Code() != "SLEEP_HOST_BACKING_INSUFFICIENT" {
		t.Fatalf("err = %v, want SLEEP_HOST_BACKING_INSUFFICIENT", err)
	}
	want := map[string]any{"node": hygon.DisplayName(), "vendor": "Hygon", "need_gib": "65.6", "available_gib": "55.5"}
	if !reflect.DeepEqual(product.Params(), want) {
		t.Fatalf("params = %v, want %v", product.Params(), want)
	}

	for name, err := range map[string]error{
		"a pool the host backs":       SleepHostBackingRefusal(hygon, 16*g, host(555*g/10, 624*g/10, true)),
		"host memory not observed":    SleepHostBackingRefusal(hygon, pool, host(0, 0, false)),
		"no host memory reader":       SleepHostBackingRefusal(hygon, pool, nil),
		"no pool":                     SleepHostBackingRefusal(hygon, 0, host(555*g/10, 624*g/10, true)),
		"no node":                     SleepHostBackingRefusal(nil, pool, nil),
		"a vendor that backs nothing": SleepHostBackingRefusal(&store.Node{UUID: "node-uuid", GPUVendor: "nvidia"}, pool, host(555*g/10, 624*g/10, true)),
		"ascend":                      SleepHostBackingRefusal(&store.Node{UUID: "node-uuid", GPUVendor: "ascend"}, pool, host(555*g/10, 624*g/10, true)),
	} {
		if err != nil {
			t.Errorf("%s: err = %v, want none", name, err)
		}
	}
}
