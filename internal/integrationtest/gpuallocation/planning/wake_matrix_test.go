package planning_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/internal/integrationtest/gpuallocation/internal/fixtures"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func sleepingRanks(t *testing.T, w fixtures.World) (*store.Deployment, []fixtures.World, [][]store.Instance) {
	t.Helper()
	remote := w
	remote.Node = w.AddNode(t, "wake-follower", 2)
	worlds := []fixtures.World{w, remote}
	dep := w.Deployment(t, "sleepers", 0)
	if err := w.Store.SetDeploymentSleep(dep.ID, 2); err != nil {
		t.Fatal(err)
	}
	var groups [][]store.Instance
	for group := 0; group < 2; group++ {
		var members []store.Instance
		for rank, world := range worlds {
			uuid := fmt.Sprintf("g%d-r%d", group, rank)
			raw, err := json.Marshal(proto.InstanceDesiredState{UUID: uuid, Engine: "vllm", DesiredState: "running", Sleepable: true, DesiredSleep: 2})
			if err != nil {
				t.Fatal(err)
			}
			row := store.Instance{UUID: uuid, DeploymentID: dep.ID, NodeID: world.Node.ID, State: proto.InstRunning, GPUIndexes: fmt.Sprintf("[%d]", group), NNodes: 2, DistGroup: fmt.Sprintf("g%d", group), DistAttempt: 1, NodeRank: rank, Headless: rank > 0, SleepResidueBytes: gib, DesiredStateJSON: string(raw)}
			if rank == 0 {
				row.SleepLevel = 2
			}
			created, err := w.Store.CreateInstance(&row)
			if err != nil {
				t.Fatal(err)
			}
			members = append(members, *created)
		}
		groups = append(groups, members)
	}
	return dep, worlds, groups
}

func instanceIDs(rows []store.Instance) []string {
	var out []string
	for _, row := range rows {
		out = append(out, row.UUID)
	}
	return out
}

func TestWakeAtomicityByBlockedRankAndHolderState(t *testing.T) {
	for _, state := range []struct {
		name, state, phase string
		holds              bool
	}{
		{"running", proto.InstRunning, "", true}, {"removing", proto.InstRemoving, "", true},
		{"failed-sleep", proto.InstFailed, proto.PhaseSleep, true}, {"failed-launch", proto.InstFailed, "pull", false}, {"stopped", proto.InstStopped, "", false},
	} {
		for _, rank := range []int{0, 1} {
			for _, partial := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/rank-%d/partial-%v", state.name, rank, partial), func(t *testing.T) {
					w := fixtures.NewWorld(t)
					_, worlds, groups := sleepingRanks(t, w)
					blocked := worlds[rank]
					other := blocked.Deployment(t, "blocker", 0)
					blocked.Instance(t, other, "blocker", state.state, state.phase, 1, 0)
					admitted, refused, err := planning.AdmitWake(w.Store, groups, partial)
					want := []string{"g0-r0", "g0-r1", "g1-r0", "g1-r1"}
					if state.holds {
						want = nil
						if partial {
							want = []string{"g0-r0", "g0-r1"}
						}
					}
					if err != nil || !slices.Equal(instanceIDs(admitted), want) || (len(refused) > 0) != state.holds {
						t.Fatalf("admitted=%v refused=%+v err=%v, want %v", instanceIDs(admitted), refused, err, want)
					}
					if state.holds {
						if len(refused) != 1 || refused[0].Node.ID != blocked.Node.ID {
							t.Fatalf("refusal named the wrong rank: %+v", refused)
						}
						if partial && !slices.Equal(instanceIDs(refused[0].Instances), []string{"g1-r0", "g1-r1"}) {
							t.Fatalf("partial wake split a replica: %+v", refused)
						}
					}
					for _, group := range groups {
						for _, row := range group {
							stored, err := w.Store.InstanceByUUID(row.UUID)
							if err != nil || stored.SleepIntent() != 2 {
								t.Fatalf("admission changed sleep intent for %s: %+v %v", row.UUID, stored, err)
							}
						}
					}
					// Confirmation that the co-tenant is gone unblocks every rank.
					if err := w.Store.UpdateInstanceStatus(proto.InstanceStatus{UUID: "blocker", State: proto.InstStopped}); err != nil {
						t.Fatal(err)
					}
					admitted, refused, err = planning.AdmitWake(w.Store, groups, partial)
					if err != nil || len(refused) != 0 || len(admitted) != 4 {
						t.Fatalf("wake after release: %v %+v %v", instanceIDs(admitted), refused, err)
					}
				})
			}
		}
	}
}

// A failed wake must roll back only its replica and generation; another
// replica's in-flight wake remains fully reserved on both nodes.
func TestFailedWakeRollbackIsLocalToItsReplica(t *testing.T) {
	w := fixtures.NewWorld(t)
	dep, worlds, groups := sleepingRanks(t, w)
	ids := append(instanceIDs(groups[0]), instanceIDs(groups[1])...)
	if err := w.Store.OrderInstancesSleep(dep.ID, ids, 0, ""); err != nil {
		t.Fatal(err)
	}
	zero := 0
	if err := w.Store.UpdateInstanceStatus(proto.InstanceStatus{UUID: "g0-r0", State: proto.InstRunning, SleepLevel: 2, SleepTarget: &zero, Phase: proto.PhaseSleep, Message: "injected wake failure"}); err != nil {
		t.Fatal(err)
	}
	for group, members := range groups {
		for rank, row := range members {
			stored, err := w.Store.InstanceByUUID(row.UUID)
			want := 0
			if group == 0 {
				want = 2
			}
			if err != nil || stored.SleepIntent() != want {
				t.Fatalf("%s intent=%+v err=%v, want %d", row.UUID, stored, err, want)
			}
			groups[group][rank] = *stored
		}
	}
	for _, world := range worlds {
		ledger := planning.GPUAllocations(w.Store, world.Node)
		if ledger[0].AllocatedBytes != 0 || ledger[0].SleepingBytes != gib || ledger[1].AllocatedBytes != 64*gib || ledger[1].SleepingBytes != 0 {
			t.Fatalf("rollback affected the other replica on %s: %+v", world.Node.UUID, ledger)
		}
	}
	admitted, refused, err := planning.AdmitWake(w.Store, groups, false)
	if err != nil || len(admitted) != 4 || len(refused) != 0 {
		t.Fatalf("retry double charged a wake in flight: admitted=%v refused=%+v err=%v", instanceIDs(admitted), refused, err)
	}
}
