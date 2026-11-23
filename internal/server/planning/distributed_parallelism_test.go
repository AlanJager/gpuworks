package planning

import (
	"context"
	"errors"
	"testing"
)

func TestDistributedParallelismRecommendation(t *testing.T) {
	for _, tc := range []struct {
		name                                              string
		tp, pp, dp, minCards, preferCards, wantTP, wantPP int
	}{
		{"small model", 0, 0, 1, 1, 0, 1, 2},
		{"larger model needs four GPUs", 0, 0, 1, 2, 0, 2, 2},
		{"three cards per node round up to four", 0, 0, 1, 3, 0, 4, 2},
		{"fixed pipeline across nodes", 0, 1, 1, 1, 0, 2, 1},
		{"fixed tensor degree", 2, 0, 1, 1, 0, 2, 1},
		{"data parallel replicas", 0, 0, 2, 1, 0, 1, 1},
		{"template preference", 0, 0, 1, 1, 4, 4, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := PlanDistributedParallelism(t.Context(), ParallelismSpec{NNodes: 2, DP: tc.dp, TP: tc.tp, PP: tc.pp}, 8, 28, tc.preferCards > 0, func(spec ParallelismSpec) (DistributedTrial, error) {
				width, err := spec.GPUsPerNode()
				if err != nil || width < tc.minCards {
					return DistributedTrial{}, errors.New("insufficient memory")
				}
				if width == tc.preferCards {
					return DistributedTrial{Preference: 1}, nil
				}
				return DistributedTrial{}, nil
			})
			if err != nil || got.TP != tc.wantTP || got.PP != tc.wantPP {
				t.Fatalf("got %+v, %v", got, err)
			}
		})
	}
}

// A node whose card count is not a power of two still reaches the layouts the
// node-derived default and a stated TP name: every card it has, and any count
// the stated TP needs.
func TestDistributedRecommendationReachesWholeNode(t *testing.T) {
	for _, tc := range []struct {
		name                                  string
		tp, pp, cardsPerNode, minCards        int
		wantTP, wantPP, wantTrials, gotTrials int
	}{
		{name: "three cards per node", cardsPerNode: 3, minCards: 3, wantTP: 3, wantPP: 2, wantTrials: 6},
		{name: "stated pipeline on six cards", pp: 2, cardsPerNode: 6, minCards: 5, wantTP: 6, wantPP: 2, wantTrials: 4},
		// Only TP6 x PP1 is tried: the other counts cannot give TP6.
		{name: "stated tensor degree of six", tp: 6, cardsPerNode: 3, minCards: 1, wantTP: 6, wantPP: 1, wantTrials: 1},
		{name: "seven cards per node", cardsPerNode: 7, minCards: 5, wantTP: 7, wantPP: 2, wantTrials: 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := PlanDistributedParallelism(t.Context(), ParallelismSpec{NNodes: 2, DP: 1, TP: tc.tp, PP: tc.pp}, tc.cardsPerNode, 28, false, func(spec ParallelismSpec) (DistributedTrial, error) {
				tc.gotTrials++
				width, err := spec.GPUsPerNode()
				if err != nil || width < tc.minCards {
					return DistributedTrial{}, errors.New("insufficient memory")
				}
				return DistributedTrial{}, nil
			})
			if err != nil || got.TP != tc.wantTP || got.PP != tc.wantPP || tc.gotTrials != tc.wantTrials {
				t.Fatalf("got %+v after %d trials, %v", got, tc.gotTrials, err)
			}
		})
	}
}

// A KV estimate below the context length does not refuse a layout, but a
// larger one that holds the context wins over packing and catalog preference.
func TestDistributedRecommendationPrefersLayoutHoldingContext(t *testing.T) {
	for _, tc := range []struct {
		name                                      string
		contextCards, preferCards, wantTP, wantPP int
	}{
		{"context needs two cards per node", 2, 0, 2, 2},
		{"no layout holds the context", 99, 0, 1, 2},
		{"preferred layout is short of context", 4, 2, 4, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := PlanDistributedParallelism(t.Context(), ParallelismSpec{NNodes: 2, DP: 1}, 8, 28, tc.preferCards > 0, func(spec ParallelismSpec) (DistributedTrial, error) {
				width, err := spec.GPUsPerNode()
				if err != nil {
					return DistributedTrial{}, err
				}
				result := DistributedTrial{ContextShort: width < tc.contextCards}
				if width == tc.preferCards {
					result.Preference = 1
				}
				return result, nil
			})
			if err != nil || got.TP != tc.wantTP || got.PP != tc.wantPP {
				t.Fatalf("got %+v, %v", got, err)
			}
		})
	}
}

func TestDistributedRecommendationStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	calls := 0
	_, err := PlanDistributedParallelism(ctx, ParallelismSpec{NNodes: 2, DP: 1}, 8, 28, false, func(ParallelismSpec) (DistributedTrial, error) {
		calls++
		cancel()
		return DistributedTrial{}, errors.New("occupied")
	})
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}
