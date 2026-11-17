package planning_test

import (
	"fmt"
	"testing"

	"gpuworks.ai/gpuworks/internal/integrationtest/gpuallocation/internal/fixtures"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/planning"
)

// A quota applies to each selected replica. Updating only one replica must
// leave its sibling's old claim in the ledger. A foreign holder is an
// additional budget constraint, not a replacement for those claims.
func TestQuotaReplicaSelectionByForeignOccupancy(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		for _, both := range []bool{false, true} {
			for _, limit := range []int64{8 * gib, 24 * gib, 32 * gib} {
				t.Run(fmt.Sprintf("foreign-%v/both-%v/limit-%d", foreign, both, limit/gib), func(t *testing.T) {
					w := fixtures.NewWorld(t)
					dep := w.Deployment(t, "replicas", 16*gib)
					w.Instance(t, dep, "first", proto.InstRunning, "", 0, 0)
					w.Instance(t, dep, "second", proto.InstRunning, "", 0, 0)
					room := int64(64 * gib)
					if foreign {
						if err := w.Store.DB().Model(&w.Node.GPUs[0]).Update("foreign_processes", `[{"pid":42,"name":"outside","memory_bytes":17179869184}]`).Error; err != nil {
							t.Fatal(err)
						}
						room -= 20 * gib // 16 GiB holder and 4 GiB headroom on this card.
					}
					live := []string{"first"}
					need := limit + gib/2
					if both {
						live = append(live, "second")
						need *= 2
					} else {
						room -= 16*gib + gib/2
					}
					short, err := planning.QuotaRoom(w.Store, dep, limit, live)
					if err != nil {
						t.Fatal(err)
					}
					if (short != nil) != (need > room) {
						t.Fatalf("shortfall=%+v, want need=%d room=%d", short, need, room)
					}
					if short != nil && (short.GPUIndex != 0 || short.NeedBytes != need || short.FreeBytes != room) {
						t.Fatalf("shortfall=%+v, want need=%d room=%d", short, need, room)
					}
					// QuotaRoom is a preview, so neither replica has changed quota.
					assertAllocation(t, w, allocationExpectation{allocated: 33 * gib, instances: 2})
				})
			}
		}
	}
}
