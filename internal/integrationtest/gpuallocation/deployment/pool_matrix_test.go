package deployment_test

import (
	"context"
	"fmt"
	"testing"

	"gpuworks.ai/gpuworks/internal/integrationtest/gpuallocation/internal/fixtures"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deployment"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Both nodes can fit the requested shape. Each gate excludes the node that
// would otherwise win, so it must change the choice rather than being masked
// by ranking or a capacity refusal. Preview must also name the exact gate.
func TestPoolStrategyByEligibilityAndReplicaShape(t *testing.T) {
	for _, strategy := range []store.PlacementStrategy{store.PlacementSpread, store.PlacementBinpack} {
		for _, shape := range []string{"whole", "tp2", "partition"} {
			for _, gate := range []struct{ name, reason string }{
				{"open", ""},
				{"cordoned", deployment.ReasonNodeCordoned},
				{"offline", deployment.ReasonNodeOffline},
				{"requirements", deployment.ReasonRequirementsUnmet},
			} {
				t.Run(fmt.Sprintf("%s/%s/%s", strategy, shape, gate.name), func(t *testing.T) {
					w := fixtures.NewWorld(t)
					other := w.AddNode(t, "other-node", 2)
					preferred, fallback := w.Node, other
					if shape != "tp2" {
						// Binpack prefers the fuller node; spread prefers the emptier
						// one. TP2 instead uses two equally empty nodes, so either can
						// take over and the stable tie-break chooses the first node.
						holder := w.Deployment(t, "holder", 16*fixtures.GiB)
						w.Instance(t, holder, "holder", proto.InstRunning, "", 0, 0)
						if strategy == store.PlacementSpread {
							preferred, fallback = other, w.Node
						}
					}
					req := request(w)
					req.PlacementStrategy = string(strategy)
					if shape == "tp2" {
						req.Placement["tp"] = 2
					}
					if shape == "partition" {
						req.VRAMLimitGiB = 16
					}
					if gate.name == "cordoned" {
						if _, err := w.Store.SetNodeScheduling(preferred.UUID, store.NodeCordoned); err != nil {
							t.Fatal(err)
						}
					}
					if gate.name == "offline" {
						if err := w.Store.SetNodeState(preferred.UUID, store.NodeStateOffline); err != nil {
							t.Fatal(err)
						}
					}
					if gate.name == "requirements" {
						req.NodeRequirements = map[string][]string{"gpuworks.ai/node": {fallback.UUID}}
					}
					svc := deployment.New(deployment.Deps{Store: w.Store, Online: func(uuid string) bool { return gate.name != "offline" || uuid != preferred.UUID }})
					want := preferred
					if gate.name != "open" {
						want = fallback
					}
					preview, err := svc.Evaluate(context.Background(), dto.EvaluateRequest{DeployRequest: req})
					if err != nil || !preview.OK || len(preview.Errors) > 0 || preview.SelectedNode == nil || preview.SelectedNode.UUID != want.UUID {
						t.Fatalf("preview selected=%+v errors=%+v candidates=%+v err=%v, want %s", preview.SelectedNode, preview.Errors, preview.Candidates, err, want.UUID)
					}
					found := false
					for _, candidate := range preview.Candidates {
						if candidate.NodeUUID != preferred.UUID {
							continue
						}
						found = true
						if candidate.OK != (gate.name == "open") || (gate.reason != "" && candidate.Reason.Code != gate.reason) {
							t.Fatalf("preferred candidate=%+v, want gate %q", candidate, gate.reason)
						}
					}
					if !found {
						t.Fatal("preview omitted the preferred node's admission result")
					}
					created, err := svc.Create(context.Background(), req)
					if err != nil || created == nil || created.NodeID != want.ID {
						t.Fatalf("create=%+v err=%v, want node %s", created, err, want.UUID)
					}
				})
			}
		}
	}
}
