package analytics

import (
	"context"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/rollup"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestGPUDetailIncludesInventoryCardsWithoutAnySamples(t *testing.T) {
	f := newFixture(t)
	node := store.Node{UUID: "hygon-node", Name: "hygon-node", State: store.NodeStateOnline}
	if err := f.st.DB().Create(&node).Error; err != nil {
		t.Fatal(err)
	}
	for i, uuid := range []string{"sampled", "unobserved"} {
		card := store.GPU{NodeID: node.ID, Ident: uuid, Index: i, UUID: uuid, Name: "K100_AI"}
		if err := f.st.DB().Create(&card).Error; err != nil {
			t.Fatal(err)
		}
	}
	measured := rollup.GPUKey{NodeUUID: node.UUID, GPUIndex: 0, GPUUUID: "sampled", HourMS: f.at(22, 10)}
	removed := rollup.GPUKey{NodeUUID: node.UUID, GPUIndex: 9, GPUUUID: "removed", HourMS: f.at(22, 10)}
	if err := f.stats.AddGPUs(context.Background(), map[rollup.GPUKey]rollup.GPU{
		measured: {UtilSum: 0, UtilN: 2, PowerSum: 244, PowerN: 2}, removed: {UtilSum: 90, UtilN: 1},
	}, map[rollup.GPUKey]string{{NodeUUID: node.UUID, GPUIndex: 0, GPUUUID: "sampled"}: "K100_AI", {NodeUUID: node.UUID, GPUIndex: 9, GPUUUID: "removed"}: "K100_AI"}); err != nil {
		t.Fatal(err)
	}
	got, err := f.svc.GPUs(context.Background(), GPUQuery{Query: Query{FromMS: f.at(22, 0), ToMS: f.at(23, 0), Granularity: Hour}, GroupBy: "gpu"})
	if err != nil {
		t.Fatal(err)
	}
	present, unknown, historical := 0, 0, 0
	for _, c := range got.Cards {
		if !c.Present {
			historical++
			continue
		}
		present++
		if c.GPUUUID == "unobserved" {
			unknown++
			if c.UtilSamples != 0 || c.UtilAvg != nil || c.PowerAvg != nil || c.TempAvg != nil {
				t.Fatalf("unobserved card invents samples: %+v", c)
			}
		}
	}
	if got.CardCount != 2 || present != 2 || unknown != 1 || historical != 1 {
		t.Fatalf("inventory and detail disagree: count=%d present=%d unknown=%d historical=%d cards=%+v", got.CardCount, present, unknown, historical, got.Cards)
	}
	if got.Totals.UtilSamples != 3 {
		t.Fatalf("historical samples lost or unknown samples invented: %+v", got.Totals)
	}
}
