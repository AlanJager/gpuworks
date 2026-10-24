package analytics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/rollup"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// fleetFixture is 120 online nodes of three cards (360, more than a page
// answers one by one): nodes 0-117 hold H100s, 118 and 119 L40Ss. Every card
// reported one hour at 22:00, node i's cards at i% utilization; node 0's
// cards serve the fixture's deployment.
func fleetFixture(t *testing.T) (*fixture, []string) {
	t.Helper()
	f := newFixture(t)
	ctx := context.Background()
	db := f.st.DB()
	var uuids []string
	delta := map[rollup.GPUKey]rollup.GPU{}
	models := map[rollup.GPUKey]string{}
	for i := range 120 {
		node := store.Node{UUID: fmt.Sprintf("node-%03d", i), Name: fmt.Sprintf("gpu-%03d", i), State: store.NodeStateOnline}
		if err := db.Create(&node).Error; err != nil {
			t.Fatal(err)
		}
		uuids = append(uuids, node.UUID)
		model := "H100"
		if i >= 118 {
			model = "L40S"
		}
		var idents []string
		for c := range 3 {
			card := store.GPU{NodeID: node.ID, Ident: fmt.Sprintf("GPU-%03d-%d", i, c), Index: c, UUID: fmt.Sprintf("GPU-%03d-%d", i, c), Name: model}
			if err := db.Create(&card).Error; err != nil {
				t.Fatal(err)
			}
			idents = append(idents, card.Ident)
			k := rollup.GPUKey{NodeUUID: node.UUID, GPUIndex: c, GPUUUID: card.UUID}
			models[k] = model
			k.HourMS = f.at(22, 10)
			delta[k] = rollup.GPU{UtilSum: float64(2 * i), UtilN: 2, UtilMax: float64(i), PowerSum: 600, PowerN: 2, MemUsedSum: 2e9, MemUsedN: 2, MemTotal: 80e9}
		}
		if i == 0 {
			raw, _ := json.Marshal(idents)
			if err := db.Create(&store.Instance{UUID: "inst-0", DeploymentID: f.dep, NodeID: node.ID, State: proto.InstRunning, GPUIdents: string(raw)}).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := f.stats.AddGPUs(ctx, delta, models); err != nil {
		t.Fatal(err)
	}
	return f, uuids
}

func TestGPUsOfAFleetAnswerGroupsNotCards(t *testing.T) {
	f, _ := fleetFixture(t)
	ctx := context.Background()
	q := Query{FromMS: f.at(22, 0), ToMS: f.at(23, 0), Granularity: Hour}

	byModel, err := f.svc.GPUs(ctx, GPUQuery{Query: q})
	if err != nil {
		t.Fatal(err)
	}
	if byModel.Detail || len(byModel.Cards) != 0 || byModel.CardCount != 360 {
		t.Fatalf("fleet by model: detail %v, %d cards, count %d", byModel.Detail, len(byModel.Cards), byModel.CardCount)
	}
	if fmt.Sprint(byModel.ByModel) != "[{H100 354} {L40S 6}]" {
		t.Fatalf("by model counts = %v", byModel.ByModel)
	}
	if len(byModel.Groups) != 2 || len(byModel.Heat) != 2 || byModel.HeatTotal != 2 {
		t.Fatalf("groups %d heat %d/%d", len(byModel.Groups), len(byModel.Heat), byModel.HeatTotal)
	}
	h100 := byModel.Groups[0]
	// Nodes 0-117 at 0..117%: the H100s average 58.5%.
	if h100.Key != "H100" || h100.Cards != 354 || h100.Nodes != 118 || *h100.UtilAvg != 58.5 || *h100.UtilMax != 117 ||
		fmt.Sprint(h100.Deployments) != "[{glm 3}]" || h100.Unassigned != 351 {
		t.Fatalf("H100 row = %+v", h100)
	}
	// The totals are the groups summed: 0..119% over every card.
	if *byModel.Totals.UtilAvg != 59.5 {
		t.Fatalf("totals = %+v", byModel.Totals)
	}

	byNode, err := f.svc.GPUs(ctx, GPUQuery{Query: q, GroupBy: "node"})
	if err != nil {
		t.Fatal(err)
	}
	if len(byNode.Groups) != 120 || byNode.HeatTotal != 120 || len(byNode.Heat) != maxHeatNodes {
		t.Fatalf("by node: %d groups, heat %d of %d", len(byNode.Groups), len(byNode.Heat), byNode.HeatTotal)
	}
	// The heat map holds the 50 busiest nodes and the 50 idlest.
	shown := map[string]bool{}
	for _, r := range byNode.Heat {
		shown[r.Label] = true
	}
	for _, name := range []string{"gpu-000", "gpu-049", "gpu-070", "gpu-119"} {
		if !shown[name] {
			t.Fatalf("%s missing from the heat map", name)
		}
	}
	for _, name := range []string{"gpu-050", "gpu-069"} {
		if shown[name] {
			t.Fatalf("%s, in the middle, is in the heat map", name)
		}
	}
	if g := byNode.Groups[0]; g.Label != "gpu-000" || g.Cards != 3 || fmt.Sprint(g.Models) != "[H100]" {
		t.Fatalf("node row = %+v", g)
	}

	var qe *messages.ProductError
	if _, err := f.svc.GPUs(ctx, GPUQuery{Query: q, GroupBy: "gpu"}); !errors.As(err, &qe) || qe.Code() != messages.ErrorAnalyticsTooManyCards {
		t.Fatalf("by card over the fleet = %v", err)
	}
}

func TestGPUsNarrowedToAFewCardsAnswerEachCard(t *testing.T) {
	f, uuids := fleetFixture(t)
	ctx := context.Background()
	q := Query{FromMS: f.at(22, 0), ToMS: f.at(23, 0), Granularity: Hour}

	// The nodes a label matched.
	two, err := f.svc.GPUs(ctx, GPUQuery{Query: q, GroupBy: "gpu", NodeUUIDs: []string{uuids[0], uuids[5]}})
	if err != nil {
		t.Fatal(err)
	}
	if !two.Detail || two.CardCount != 6 || len(two.Cards) != 6 || len(two.Heat) != 6 {
		t.Fatalf("two nodes: detail %v count %d cards %d heat %d", two.Detail, two.CardCount, len(two.Cards), len(two.Heat))
	}
	if c := two.Cards[0]; c.NodeName != "gpu-000" || !c.Present || fmt.Sprint(c.Deployments) != "[glm]" {
		t.Fatalf("card = %+v", c)
	}
	if *two.Totals.UtilAvg != 2.5 {
		t.Fatalf("two nodes' totals = %+v", two.Totals)
	}
	byModel, err := f.svc.GPUs(ctx, GPUQuery{Query: q, NodeUUIDs: []string{uuids[0], uuids[5]}})
	if err != nil || len(byModel.Groups) != 1 || byModel.Groups[0].Cards != 6 || *byModel.Groups[0].UtilAvg != 2.5 {
		t.Fatalf("two nodes by model = %+v %v", byModel.Groups, err)
	}

	// A rare model of a large fleet: its six cards one by one.
	l40s, err := f.svc.GPUs(ctx, GPUQuery{Query: q, GroupBy: "node", GPUModel: "L40S"})
	if err != nil {
		t.Fatal(err)
	}
	if !l40s.Detail || len(l40s.Cards) != 6 || len(l40s.Groups) != 2 || l40s.Groups[0].Label != "gpu-118" {
		t.Fatalf("L40S: detail %v cards %d groups %+v", l40s.Detail, len(l40s.Cards), l40s.Groups)
	}

	// A label matching no node answers nothing, not the fleet.
	none, err := f.svc.GPUs(ctx, GPUQuery{Query: q, NodeUUIDs: []string{}})
	if err != nil || none.CardCount != 0 || len(none.Cards) != 0 || len(none.Groups) != 0 || len(none.Heat) != 0 {
		t.Fatalf("no nodes = %+v %v", none, err)
	}
}
