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

// Ranking hints never override node admission. The fuller node has one free
// card and the other has two: binpack and spread prefer opposite nodes, until
// a node is excluded or the actual TP/quota plan says it cannot fit.
func TestPoolStrategyByEligibilityAndReplicaShape(t *testing.T) {
	for _, strategy := range []store.PlacementStrategy{store.PlacementSpread, store.PlacementBinpack} {
		for _, shape := range []string{"whole", "tp2", "partition"} {
			for _, gate := range []string{"open", "cordoned", "offline", "requirements"} {
				t.Run(fmt.Sprintf("%s/%s/%s", strategy, shape, gate), func(t *testing.T) {
					w := fixtures.NewWorld(t)
					other := w.AddNode(t, "other-node", 2)
					holder := w.Deployment(t, "holder", 16*fixtures.GiB)
					w.Instance(t, holder, "holder", proto.InstRunning, "", 0, 0)
					req := request(w)
					req.PlacementStrategy = string(strategy)
					if shape == "tp2" {
						req.Placement["tp"] = 2
					}
					if shape == "partition" {
						req.VRAMLimitGiB = 16
					}
					if gate == "cordoned" {
						if _, err := w.Store.SetNodeScheduling(w.Node.UUID, store.NodeCordoned); err != nil {
							t.Fatal(err)
						}
					}
					if gate == "offline" {
						if err := w.Store.SetNodeState(w.Node.UUID, store.NodeStateOffline); err != nil {
							t.Fatal(err)
						}
					}
					if gate == "requirements" {
						req.NodeRequirements = map[string][]string{"gpuworks.ai/node": {other.UUID}}
					}
					svc := deployment.New(deployment.Deps{Store: w.Store, Online: func(uuid string) bool { return gate != "offline" || uuid != w.Node.UUID }})
					want := other
					if gate == "open" && strategy == store.PlacementBinpack && shape != "tp2" {
						want = w.Node
					}
					preview, err := svc.Evaluate(context.Background(), dto.EvaluateRequest{DeployRequest: req})
					if err != nil || !preview.OK || len(preview.Errors) > 0 || preview.SelectedNode == nil || preview.SelectedNode.UUID != want.UUID {
						t.Fatalf("preview selected=%+v errors=%+v candidates=%+v err=%v, want %s", preview.SelectedNode, preview.Errors, preview.Candidates, err, want.UUID)
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
