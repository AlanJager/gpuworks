package hub

import (
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// Both node-side entry points decide "is this already being fetched?" with this
// rule. Getting it wrong is silent in the worst way: the request returns the
// leftover row and never reaches a node at all.
func TestImageRowActiveOnlyCountsTransfersThatStillReport(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		row  store.ImageNode
		want bool
	}{
		{"present", store.ImageNode{State: store.ImageStatePresent, UpdatedAt: now.Add(-time.Hour)}, true},
		{"syncing that just reported", store.ImageNode{State: store.ImageStateSyncing, UpdatedAt: now}, true},
		{"syncing that went quiet", store.ImageNode{State: store.ImageStateSyncing, UpdatedAt: now.Add(-2 * imageDirectStallAfter)}, false},
		{"pulling that just reported", store.ImageNode{State: store.ImageStatePulling, UpdatedAt: now}, true},
		{"pulling that went quiet", store.ImageNode{State: store.ImageStatePulling, UpdatedAt: now.Add(-2 * imageDirectStallAfter)}, false},
		{"failed", store.ImageNode{State: store.ImageStateFailed, UpdatedAt: now}, false},
		{"absent", store.ImageNode{State: store.ImageStateAbsent, UpdatedAt: now}, false},
	}
	for _, tc := range cases {
		row := tc.row
		if got := imageRowActive(&row); got != tc.want {
			t.Errorf("%s: imageRowActive = %v, want %v", tc.name, got, tc.want)
		}
	}
}
