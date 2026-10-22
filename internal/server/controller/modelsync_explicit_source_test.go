package controller

import (
	"context"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// addPresentPeer registers an online node that already holds a complete copy.
func addPresentPeer(t *testing.T, st *store.Store, modelID uint, name string) *store.Node {
	t.Helper()
	node, err := st.RegisterNode(name, store.HashToken("token-"+name), name)
	if err != nil {
		t.Fatal(err)
	}
	reportManagementAddress(t, st, name, "10.0.0.9")
	if _, err := st.UpsertModelFile(&store.ModelFile{
		ModelID: modelID, NodeID: node.ID, Path: "/models/" + name, State: proto.MFPresent,
		SizeBytes: 12, Downloaded: 12, Acquisition: proto.ModelAcquireRegistered,
	}); err != nil {
		t.Fatal(err)
	}
	return node
}

// A user who chose a specific source node must get that node, even when other
// online nodes also hold a complete copy and appear earlier in the listing.
func TestPeerSyncUsesTheExplicitlySelectedSourceNode(t *testing.T) {
	st, model, firstPeer, dst, _ := setupModelSyncController(t)
	chosen := addPresentPeer(t, st, model.ID, "chosen")
	if err := st.SetModelSyncSource(store.FileRef{ModelID: model.ID, NodeID: dst.ID}, chosen.ID); err != nil {
		t.Fatal(err)
	}
	dstMF, err := st.ModelFileByModelNode(model.ID, dst.ID)
	if err != nil {
		t.Fatal(err)
	}
	hub := &modelSyncHub{online: map[string]bool{"src": true, "dst": true, "chosen": true}, called: make(chan struct{}), release: make(chan struct{})}
	c := New(st, hub, nil)
	if !c.trySyncModelFromPeer(context.Background(), dst, model, dstMF) {
		t.Fatal("peer sync was not accepted")
	}
	select {
	case <-hub.called:
	case <-time.After(2 * time.Second):
		t.Fatal("peer copy did not start")
	}
	got, err := st.ModelFileByModelNode(model.ID, dst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.TransferSourceNodeID != chosen.ID {
		t.Fatalf("synced from node %d, want the explicitly chosen %d (other holder=%d)", got.TransferSourceNodeID, chosen.ID, firstPeer.ID)
	}
	hub.mu.Lock()
	requests := append([]proto.ModelSyncCopyRequest(nil), hub.requests...)
	hub.mu.Unlock()
	if len(requests) != 1 || requests[0].SourceNodeUUID != chosen.UUID {
		t.Fatalf("copy requests = %+v, want a single request from %s", requests, chosen.UUID)
	}
	close(hub.release)
}

// When the chosen source is offline the sync must not silently retarget a
// different node or fall back to a public download.
func TestPeerSyncDoesNotSubstituteAnotherNodeWhenTheChosenSourceIsOffline(t *testing.T) {
	st, model, _, dst, _ := setupModelSyncController(t)
	chosen := addPresentPeer(t, st, model.ID, "offline-choice")
	if err := st.SetModelSyncSource(store.FileRef{ModelID: model.ID, NodeID: dst.ID}, chosen.ID); err != nil {
		t.Fatal(err)
	}
	dstMF, mfErr := st.ModelFileByModelNode(model.ID, dst.ID)
	if mfErr != nil {
		t.Fatal(mfErr)
	}
	// "src" is online and holds a copy, but it is not what the user chose.
	hub := &modelSyncHub{online: map[string]bool{"src": true, "dst": true}}
	c := New(st, hub, nil)
	if !c.trySyncModelFromPeer(context.Background(), dst, model, dstMF) {
		t.Fatal("reconcile did not claim the model file")
	}
	if hub.requestCount() != 0 {
		t.Fatalf("a peer copy was started from a node the user did not choose: %d requests", hub.requestCount())
	}
	got, gotErr := st.ModelFileByModelNode(model.ID, dst.ID)
	if gotErr != nil {
		t.Fatal(gotErr)
	}
	if got.Acquisition != proto.ModelAcquirePeer || got.State != proto.MFDownloadRequired {
		t.Fatalf("model file after unavailable explicit source = %+v, want a blocked peer sync", got)
	}
}

func TestPeerSourcesFilterTargetAndRespectExplicitChoice(t *testing.T) {
	st, model, src, dst, _ := setupModelSyncController(t)
	third, err := st.RegisterNode("third", store.HashToken("t3"), "third")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: third.ID, Path: "/models/c", State: proto.MFPresent, SizeBytes: 12}); err != nil {
		t.Fatal(err)
	}
	c := New(st, &modelSyncHub{online: map[string]bool{"src": true, "dst": true, "third": true}}, nil)
	nodes := func(sources []peerSource) []uint {
		var ids []uint
		for _, s := range sources {
			ids = append(ids, s.node.ID)
		}
		return ids
	}
	got, err := c.peerSources(model.ID, dst.ID, 0)
	if err != nil || len(got) != 2 || got[0].node.ID != src.ID || got[1].node.ID != third.ID {
		t.Fatalf("without an explicit choice = %v err=%v", nodes(got), err)
	}
	if got, _ := c.peerSources(model.ID, dst.ID, third.ID); len(got) != 1 || got[0].node.ID != third.ID {
		t.Fatalf("with an explicit choice = %v", nodes(got))
	}
	// A stale choice that names the target itself, or a node with no copy,
	// yields no candidate rather than an arbitrary substitute.
	if got, _ := c.peerSources(model.ID, dst.ID, dst.ID); len(got) != 0 {
		t.Fatalf("choice naming the target = %v", nodes(got))
	}
	if got, _ := c.peerSources(model.ID, dst.ID, 99); len(got) != 0 {
		t.Fatalf("choice naming an unknown node = %v", nodes(got))
	}
	// An offline holder is never a source.
	c = New(st, &modelSyncHub{online: map[string]bool{"dst": true, "third": true}}, nil)
	if got, _ := c.peerSources(model.ID, dst.ID, 0); len(got) != 1 || got[0].node.ID != third.ID {
		t.Fatalf("offline source offered: %v", nodes(got))
	}
}

func TestFailedSelectedSourceRequiresExplicitRetry(t *testing.T) {
	for _, state := range []string{proto.MFFailed, proto.MFDownloadRequired} {
		t.Run(state, func(t *testing.T) {
			st, model, src, dst, mf := setupModelSyncController(t)
			mf.State = state
			mf.Acquisition = proto.ModelAcquirePeer
			mf.TransferSourceNodeID = src.ID
			mf.LastRateLimit = 10240
			mf.Message = "previous failure"
			if _, err := st.UpsertModelFile(mf); err != nil {
				t.Fatal(err)
			}
			hub := &modelSyncHub{online: map[string]bool{"src": true, "dst": true}}
			c := New(st, hub, nil)
			for i := 0; i < 3; i++ {
				current, err := st.ModelFileByModelNode(model.ID, dst.ID)
				if err != nil {
					t.Fatal(err)
				}
				if !c.trySyncModelFromPeer(context.Background(), dst, model, current) {
					t.Fatal("task not claimed")
				}
			}
			got, err := st.ModelFileByModelNode(model.ID, dst.ID)
			if err != nil {
				t.Fatal(err)
			}
			if hub.requestCount() != 0 || got.State != state || got.LastRateLimit != 10240 || got.Message != "previous failure" {
				t.Fatalf("failed task restarted or mutated: %+v", got)
			}
		})
	}
}
