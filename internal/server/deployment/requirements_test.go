package deployment

import (
	"encoding/json"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// An edit that moves a deployment onto another node stores only the cards that
// node reports: an index names a card on one machine, and the old node's card
// is no choice on the new one (GWKS-522).
func TestPlacementForNodeDropsCardsTheNodeDoesNotReport(t *testing.T) {
	oneCard := &store.Node{UUID: "one-card", GPUs: []store.GPU{{Index: 0}}}
	for _, tc := range []struct {
		name      string
		placement map[string]any
		node      *store.Node
		automatic bool
		want      string
	}{
		{"the old node's card goes", map[string]any{"gpus": []any{float64(1)}, "tp": float64(1)}, oneCard, false, `{"tp":1}`},
		{"the node's own card stays", map[string]any{"gpus": []any{float64(0)}, "tp": float64(1)}, oneCard, false, `{"gpus":[0],"tp":1}`},
		{"the echoed spelling goes too", map[string]any{"gpu_indexes": []any{float64(1)}}, oneCard, false, `{}`},
		{"no node to judge against", map[string]any{"gpus": []any{float64(1)}, "tp": float64(1)}, nil, false, `{"gpus":[1],"tp":1}`},
		{"an automatic placement keeps no cards anyway", map[string]any{"gpus": []any{float64(1)}, "tp": float64(1)}, oneCard, true, `{"tp":1}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stored := PlacementForNode(tc.placement, tc.automatic, tc.node)
			encoded, err := json.Marshal(stored)
			if err != nil {
				t.Fatal(err)
			}
			if string(encoded) != tc.want {
				t.Fatalf("stored placement = %s, want %s", encoded, tc.want)
			}
		})
	}
}
