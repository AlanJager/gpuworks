package deployment

import (
	"reflect"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestListsConnectionChangesRebuildStatesButUnchangedPollingReusesThem(t *testing.T) {
	f := newListsFixture(t)
	if err := f.st.UpdateInstanceStatus(proto.InstanceStatus{UUID: f.instance, State: proto.InstRunning}); err != nil {
		t.Fatal(err)
	}
	live := false
	f.lists = NewListsWithConnections(f.st, nil, func() time.Time { return f.now }, func(string) bool { return live })
	first, _ := f.rows(t)
	firstStates := first.States()
	if firstStates[f.dep.ID] != "offline" {
		t.Fatalf("unreconnected deployment=%s", firstStates[f.dep.ID])
	}
	for range 3 {
		unchanged, rebuilt := f.rows(t)
		if rebuilt || unchanged != first || reflect.ValueOf(unchanged.States()).Pointer() != reflect.ValueOf(firstStates).Pointer() {
			t.Fatal("unchanged polling rebuilt inventory or state memo")
		}
	}
	// No store event is needed for authentication to invalidate the snapshot.
	live = true
	connected, rebuilt := f.rows(t)
	if !rebuilt || connected.States()[f.dep.ID] != "running" {
		t.Fatalf("connected deployment=%s rebuilt=%v", connected.States()[f.dep.ID], rebuilt)
	}
	if first.States()[f.dep.ID] != "offline" {
		t.Fatal("connection change mutated a snapshot already held by another request")
	}
	live = false
	disconnected, rebuilt := f.rows(t)
	if !rebuilt || disconnected.States()[f.dep.ID] != "offline" {
		t.Fatalf("disconnected deployment=%s rebuilt=%v", disconnected.States()[f.dep.ID], rebuilt)
	}
	stored, err := f.st.NodeByUUID(f.node.UUID)
	if err != nil || stored.State != store.NodeStateOnline {
		t.Fatalf("presentation changed persisted state=%+v err=%v", stored, err)
	}
	// Preserve the safety refresh for external writes that announce no event.
	f.now = f.now.Add(listsMaxAge)
	if _, rebuilt := f.rows(t); !rebuilt {
		t.Fatal("connection view lost the existing refresh deadline")
	}
}
