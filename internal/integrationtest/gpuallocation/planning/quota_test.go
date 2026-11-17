package planning_test

import (
	"fmt"
	"testing"

	"gpuworks.ai/gpuworks/internal/integrationtest/gpuallocation/internal/fixtures"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/planning"
)

type quotaRow struct {
	foreign, both bool
	limit         int64
}

// Both replicas hold a 16 GiB slice on one 64 GiB card. Each selected replica
// needs the new slice claim; an unselected sibling keeps its old claim, and a
// foreign holder takes its memory and headroom from the room.
func (r quotaRow) expectation() (need, room int64) {
	need, room = fixtures.NVIDIASliceClaim(r.limit), 64*gib
	if r.foreign {
		room -= fixtures.ForeignHolderBytes + fixtures.ForeignHeadroom
	}
	if r.both {
		need *= 2
	} else {
		room -= fixtures.NVIDIASliceClaim(16 * gib)
	}
	return need, room
}

// A quota applies to each selected replica. Updating only one replica must
// leave its sibling's old claim in the ledger. A foreign holder is an
// additional budget constraint, not a replacement for those claims.
func TestQuotaReplicaSelectionByForeignOccupancy(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		for _, both := range []bool{false, true} {
			for _, limit := range []int64{8 * gib, 24 * gib, 32 * gib} {
				row := quotaRow{foreign: foreign, both: both, limit: limit}
				t.Run(fmt.Sprintf("foreign-%v/both-%v/limit-%d", foreign, both, limit/gib), func(t *testing.T) {
					w := fixtures.NewWorld(t)
					dep := w.Deployment(t, "replicas", 16*gib)
					w.Instance(t, dep, "first", proto.InstRunning, "", 0, 0)
					w.Instance(t, dep, "second", proto.InstRunning, "", 0, 0)
					if row.foreign {
						held := fmt.Sprintf(`[{"pid":42,"name":"outside","memory_bytes":%d}]`, fixtures.ForeignHolderBytes)
						if err := w.Store.DB().Model(&w.Node.GPUs[0]).Update("foreign_processes", held).Error; err != nil {
							t.Fatal(err)
						}
					}
					live := []string{"first"}
					if row.both {
						live = append(live, "second")
					}
					need, room := row.expectation()
					short, err := planning.QuotaRoom(w.Store, dep, row.limit, live)
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
					assertAllocation(t, w, allocationExpectation{allocated: 2 * fixtures.NVIDIASliceClaim(16*gib), instances: 2})
				})
			}
		}
	}
}
