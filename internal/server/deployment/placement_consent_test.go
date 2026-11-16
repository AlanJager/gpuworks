package deployment

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

// The cards a manual draft names under "gpus" are the operator's choice; an
// automatic draft's cards are the scheduler's, whatever it echoes back.
func TestEvaluationPlacementConsentsOnlyForManualCards(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  dto.DeployRequest
		want bool
	}{
		{"manual, gpus", dto.DeployRequest{PlacementMode: "manual", Placement: map[string]any{"gpus": []any{float64(0)}}}, true},
		{"manual, echoed gpu_indexes", dto.DeployRequest{PlacementMode: "manual", Placement: map[string]any{"gpu_indexes": []any{float64(0)}}}, false},
		{"automatic, gpus", dto.DeployRequest{PlacementMode: "auto", Placement: map[string]any{"gpus": []any{float64(0)}}}, false},
		{"manual, no cards", dto.DeployRequest{PlacementMode: "manual", Placement: map[string]any{"tp": float64(1)}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := evaluationPlacement(&tc.req).OperatorPinned(); got != tc.want {
				t.Fatalf("operator pinned = %v, want %v", got, tc.want)
			}
		})
	}
}
