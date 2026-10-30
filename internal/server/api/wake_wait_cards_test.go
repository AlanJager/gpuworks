package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// wakeCardsFixture is the sleep fixture's node with a second 24 GiB card.
func wakeCardsFixture(t *testing.T) wakeWaitFixture {
	t.Helper()
	f := newSleepFixture(t)
	gpu := store.GPU{Index: 1, UUID: "gpu-1", NodeID: f.node.ID, Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0", MemoryTotal: 24 << 30}
	if err := f.st.DB().Create(&gpu).Error; err != nil {
		t.Fatal(err)
	}
	f.node.GPUs = append(f.node.GPUs, gpu)
	return wakeWaitFixture{sleepFixture: f}
}

// replica is an instance on gpus admitted with 20 GiB of each, asleep at L1
// when asleep.
func (f wakeWaitFixture) replica(t *testing.T, dep *store.Deployment, uuid, gpus string, asleep bool) {
	t.Helper()
	spec, _ := json.Marshal(proto.InstanceDesiredState{UUID: uuid, DesiredState: "running", Sleepable: dep.Sleepable, APIPort: 8000, MemoryClaimBytes: 20 << 30})
	if _, err := f.st.CreateInstance(&store.Instance{UUID: uuid, DeploymentID: dep.ID, NodeID: f.node.ID, State: proto.InstRunning, GPUIndexes: gpus, NNodes: 1, APIPort: 8000, DesiredStateJSON: string(spec)}); err != nil {
		t.Fatal(err)
	}
	if !asleep {
		return
	}
	if err := f.st.OrderInstancesSleep(dep.ID, []string{uuid}, 1, ""); err != nil {
		t.Fatal(err)
	}
	if err := f.st.UpdateInstanceStatus(proto.InstanceStatus{UUID: uuid, State: proto.InstRunning, SleepLevel: 1}); err != nil {
		t.Fatal(err)
	}
}

// yieldEvents are the YIELDED_FOR_WAKE events of a deployment, as the
// instances each names.
func (f wakeWaitFixture) yieldEvents(t *testing.T, id uint) []string {
	t.Helper()
	events, err := f.st.DeploymentEvents(id, 20)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, ev := range events {
		if ev.Code != "YIELDED_FOR_WAKE" {
			continue
		}
		var data struct {
			Instances []string `json:"instances"`
		}
		if err := json.Unmarshal([]byte(ev.Data), &data); err != nil {
			t.Fatal(err)
		}
		out = append(out, strings.Join(data.Instances, ","))
	}
	slices.Sort(out)
	return out
}

// A yielding wake whose occupant is a replica on two cards (TP=2), one of
// which the sleeper needs, puts that replica to sleep whole and once, with
// one event; a wake needing both its cards does the same.
func TestWakeYieldPutsAReplicaSpanningCardsToSleepOnce(t *testing.T) {
	for _, sleeperCards := range []string{"[1]", "[0,1]"} {
		t.Run(sleeperCards, func(t *testing.T) {
			f := wakeCardsFixture(t)
			wide := f.deployment(t, &store.Deployment{Name: "wide", ServedName: "wide", Engine: "vllm", Sleepable: true, Replicas: 1})
			f.replica(t, wide, "wide-1", "[0,1]", false)
			sleeper := f.deployment(t, &store.Deployment{Name: "sleeper", ServedName: "sleeper", Engine: "vllm", Sleepable: true, Replicas: 1})
			f.replica(t, sleeper, "sleeper-1", sleeperCards, true)

			status, body := f.deploymentCall(t, sleeper.ID, "?yield=true")
			if want := `{"status":"waiting","woken":[],"waiting":["sleeper-1"],"yielded":["wide"]}`; status != http.StatusAccepted || body != want {
				t.Fatalf("wake?yield=true = %d %s, want 202 %s", status, body, want)
			}
			if got := sleepIntent(t, f.st, "wide-1"); got != 1 {
				t.Fatalf("wide-1 intent = %d, want 1", got)
			}
			if got := f.yieldEvents(t, wide.ID); !slices.Equal(got, []string{"wide-1"}) {
				t.Fatalf("wide's yield events = %v, want one naming wide-1", got)
			}
		})
	}
}

// A yielding wake whose needed cards hold two replicas of one occupant puts
// each to sleep once, one event each, and names the deployment once; a
// replica of it on a card the wake does not need stays awake.
func TestWakeYieldPutsEachReplicaOnTheNeededCardsToSleepOnce(t *testing.T) {
	f := wakeCardsFixture(t)
	pair := f.deployment(t, &store.Deployment{Name: "pair", ServedName: "pair", Engine: "vllm", Sleepable: true, Replicas: 2})
	f.replica(t, pair, "pair-1", "[0]", false)
	f.replica(t, pair, "pair-2", "[1]", false)
	big := f.deployment(t, &store.Deployment{Name: "big", ServedName: "big", Engine: "vllm", Sleepable: true, Replicas: 1})
	f.replica(t, big, "big-1", "[0,1]", true)
	narrow := f.deployment(t, &store.Deployment{Name: "narrow", ServedName: "narrow", Engine: "vllm", Sleepable: true, Replicas: 1})
	f.replica(t, narrow, "narrow-1", "[1]", true)

	status, body := f.deploymentCall(t, narrow.ID, "?yield=true")
	if want := `{"status":"waiting","woken":[],"waiting":["narrow-1"],"yielded":["pair"]}`; status != http.StatusAccepted || body != want {
		t.Fatalf("wake narrow = %d %s, want 202 %s", status, body, want)
	}
	if sleepIntent(t, f.st, "pair-2") != 1 || sleepIntent(t, f.st, "pair-1") != 0 {
		t.Fatalf("intents pair-1=%d pair-2=%d, want only pair-2 asleep", sleepIntent(t, f.st, "pair-1"), sleepIntent(t, f.st, "pair-2"))
	}
	if got := f.yieldEvents(t, pair.ID); !slices.Equal(got, []string{"pair-2"}) {
		t.Fatalf("pair's yield events = %v", got)
	}

	status, body = f.deploymentCall(t, big.ID, "?yield=true")
	if want := `{"status":"waiting","woken":[],"waiting":["big-1"],"yielded":["pair"]}`; status != http.StatusAccepted || body != want {
		t.Fatalf("wake big = %d %s, want 202 %s", status, body, want)
	}
	if sleepIntent(t, f.st, "pair-1") != 1 || sleepIntent(t, f.st, "pair-2") != 1 {
		t.Fatal("a replica of pair on big's cards is still awake")
	}
	// pair-2 was ordered asleep already: only pair-1 is put to sleep now.
	if got := f.yieldEvents(t, pair.ID); !slices.Equal(got, []string{"pair-1", "pair-2"}) {
		t.Fatalf("pair's yield events = %v, want one per replica", got)
	}
}
