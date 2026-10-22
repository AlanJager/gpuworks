package store

import (
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

// The pool ranking reads which GPUs awake instances hold for every node in one
// query. Sleeping instances and ones that hold nothing any more are left out,
// and a row whose GPU list does not read is skipped rather than failing the
// whole read.
func TestAwakeGPUHolders(t *testing.T) {
	st := newTestStore(t)
	for _, inst := range []Instance{
		{UUID: "a", NodeID: 1, State: proto.InstRunning, GPUIndexes: "[0,1]"},
		{UUID: "b", NodeID: 1, State: proto.InstDraining, GPUIndexes: "[3]"},
		{UUID: "c", NodeID: 2, State: proto.InstStarting, GPUIndexes: "[2]"},
		{UUID: "asleep", NodeID: 2, State: proto.InstRunning, SleepLevel: 1, GPUIndexes: "[0]"},
		{UUID: "stopped", NodeID: 3, State: proto.InstStopped, GPUIndexes: "[0]"},
		{UUID: "failed", NodeID: 3, State: proto.InstFailed, GPUIndexes: "[1]"},
		{UUID: "unreadable", NodeID: 3, State: proto.InstRunning, GPUIndexes: "not json"},
	} {
		if err := st.db.Create(&inst).Error; err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.AwakeGPUHolders()
	if err != nil {
		t.Fatal(err)
	}
	want := map[uint]map[int]bool{1: {0: true, 1: true, 3: true}, 2: {2: true}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("holders = %v, want %v", got, want)
	}
}
