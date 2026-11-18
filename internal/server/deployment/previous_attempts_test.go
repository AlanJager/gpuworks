package deployment

import (
	"reflect"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestPreviousAttemptsKeepOnlyTheNewestFailureOfAnUnfilledClassCurrent(t *testing.T) {
	old := store.Instance{UUID: "old", CreatedAt: time.Unix(1, 0)}
	latest := store.Instance{UUID: "latest", CreatedAt: time.Unix(2, 0)}
	other := store.Instance{UUID: "other-unit", CreatedAt: time.Unix(3, 0)}
	for _, tc := range []struct {
		name  string
		slots []planning.UnitSlot
		want  map[string]bool
	}{
		{"exhausted", []planning.UnitSlot{{Key: "pool"}, {Key: "named"}}, map[string]bool{"old": true}},
		{"healthy sibling", []planning.UnitSlot{{Key: "pool", Inst: &store.Instance{UUID: "healthy"}}, {Key: "pool"}, {Key: "named"}}, map[string]bool{"old": true}},
		{"all filled", []planning.UnitSlot{{Key: "pool", Inst: &store.Instance{UUID: "healthy"}}, {Key: "named"}}, map[string]bool{"old": true, "latest": true}},
		{"other unit filled", []planning.UnitSlot{{Key: "pool"}, {Key: "named", Inst: &store.Instance{UUID: "other-healthy"}}}, map[string]bool{"old": true, "other-unit": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Input order does not determine the latest failure.
			got := PreviousAttempts(planning.UnitMatch{Slots: tc.slots, Failed: map[string][]store.Instance{"pool": {latest, old}, "named": {other}}})
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("previous=%v want %v", got, tc.want)
			}
		})
	}
}
