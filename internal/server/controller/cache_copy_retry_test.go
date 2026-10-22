package controller

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// stopHub answers model.copy.stop as the agent would: confirmed or not, or
// an RPC error, and records each stop it was asked for.
type stopHub struct {
	storageHub
	confirmed bool
	rpcErr    error
	evictErr  error
	stops     []proto.ModelCopyStopParams
}

func (h *stopHub) CallNode(ctx context.Context, uuid, method string, params any) (json.RawMessage, error) {
	if method == proto.RPCCacheEvict && h.evictErr != nil {
		return nil, h.evictErr
	}
	if method != proto.RPCModelCopyStop {
		return h.storageHub.CallNode(ctx, uuid, method, params)
	}
	if _, ok := ctx.Deadline(); !ok {
		return nil, errors.New("unbounded wait for the old worker")
	}
	h.stops = append(h.stops, params.(proto.ModelCopyStopParams))
	raw, _ := json.Marshal(proto.ModelCopyStopResult{Stopped: h.confirmed})
	return raw, h.rpcErr
}

func TestRetryCacheCopyNeedsConfirmedExitThenPushesANewIntent(t *testing.T) {
	f := newStorageControllerFixture(t)
	hub := &stopHub{storageHub: storageHub{distributedTestHub: f.hub.distributedTestHub}}
	f.c = New(f.st, hub, nil)
	f.ready(t, f.node, proto.StorageStatus{ID: f.cached.ID, State: proto.StorageStateReady, Path: "/archive", CachePath: "/cache", Writable: true})
	if _, err := f.st.UpsertModelFile(&store.ModelFile{ModelID: f.model.ID, StorageID: f.cached.ID, Path: "Qwen/Qwen3-8B", State: proto.MFPresent, SizeBytes: 15 << 30}); err != nil {
		t.Fatal(err)
	}
	ref := store.FileRef{ModelID: f.model.ID, NodeID: f.node.ID, StorageID: f.cached.ID}
	file, err := f.st.PrepareModelCopy(ref, "Qwen/Qwen3-8B")
	if err != nil {
		t.Fatal(err)
	}
	var blocked *planning.CacheRetryBlocked
	if err := f.c.RetryCacheCopy(context.Background(), ref); !errors.As(err, &blocked) || len(hub.stops) != 0 {
		t.Fatalf("a copy that has not failed was retried: %v", err)
	}
	if err := f.st.SetModelFileState(file.ID, proto.MFFailed, "read failed"); err != nil {
		t.Fatal(err)
	}
	for _, rpcErr := range []error{nil, errors.New("blocked I/O")} {
		hub.rpcErr = rpcErr
		if err := f.c.RetryCacheCopy(context.Background(), ref); !errors.As(err, &blocked) {
			t.Fatalf("unconfirmed exit (rpc err %v) accepted: %v", rpcErr, err)
		}
		current, _ := f.st.ModelFileByID(file.ID)
		if current.State != proto.MFFailed || current.TransferID != file.TransferID {
			t.Fatalf("unconfirmed stop replaced the intent: %+v", current)
		}
	}
	if len(hub.stops) != 2 || hub.stops[0] != (proto.ModelCopyStopParams{Key: store.ModelFileKey(file.ID), TransferID: file.TransferID}) {
		t.Fatalf("stop requests = %+v", hub.stops)
	}
	hub.online[f.node.UUID] = false
	if err := f.c.RetryCacheCopy(context.Background(), ref); !errors.As(err, &blocked) || len(hub.stops) != 2 {
		t.Fatalf("offline node: %v, stops %d", err, len(hub.stops))
	}
	hub.online[f.node.UUID] = true
	hub.rpcErr, hub.confirmed = nil, true
	before := len(hub.updates[f.node.UUID])
	if err := f.c.RetryCacheCopy(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	current, _ := f.st.ModelFileByID(file.ID)
	if current.State != proto.MFPending || current.TransferID == file.TransferID || current.TransferID == "" {
		t.Fatalf("retry: %+v", current)
	}
	pushed := hub.updates[f.node.UUID][before:]
	found := false
	for _, update := range pushed {
		for _, task := range update.ModelFiles {
			found = found || (task.Key == store.ModelFileKey(file.ID) && task.TransferID == current.TransferID)
		}
	}
	if !found {
		t.Fatalf("the new intent was not pushed: %+v", pushed)
	}
	if err := f.c.RetryCacheCopy(context.Background(), ref); !errors.As(err, &blocked) {
		t.Fatalf("duplicate retry accepted: %v", err)
	}
}

// A failed eviction is shown as such and retried after the backoff: no
// model-file event follows it, and nothing else polls a waiting deployment.
func TestCacheEvictionFailureIsShownAndRetried(t *testing.T) {
	f := newStorageControllerFixture(t)
	hub := &stopHub{storageHub: storageHub{distributedTestHub: f.hub.distributedTestHub}, evictErr: errors.New("hung NFS")}
	f.c = New(f.st, hub, nil)
	small, err := f.st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "Qwen/Qwen3-0.6B", Name: "small", WeightBytes: 10 << 30})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []*store.Model{f.model, small} {
		if _, err := f.st.UpsertModelFile(&store.ModelFile{ModelID: m.ID, StorageID: f.cached.ID, Path: m.SourceID, State: proto.MFPresent, SizeBytes: m.WeightBytes}); err != nil {
			t.Fatal(err)
		}
	}
	f.ready(t, f.node, proto.StorageStatus{ID: f.cached.ID, State: proto.StorageStateReady, Path: "/mnt/gpuworks/archive-cache", Writable: true, CachePath: "/var/cache/gw"})
	old := time.Now().Add(-48 * time.Hour)
	if _, err := f.st.UpsertModelFile(&store.ModelFile{ModelID: small.ID, StorageID: f.cached.ID, NodeID: f.node.ID, Path: "/var/cache/gw/small", State: proto.MFPresent, SizeBytes: 10 << 30, LastUsedAt: &old}); err != nil {
		t.Fatal(err)
	}
	dep := f.deployment(t, f.cached)
	if _, ready, err := f.c.ensureModelReady(context.Background(), f.node, dep, f.model, store.SourceRules{SaveTo: dep.SaveToStorageID}); err != nil || ready {
		t.Fatalf("ready=%v err=%v", ready, err)
	}
	ref := store.FileRef{ModelID: f.model.ID, NodeID: f.node.ID, StorageID: f.cached.ID}
	if got := f.c.CachePhase(ref); got != "eviction_failed" {
		t.Fatalf("cache phase = %q", got)
	}
	f.c.q.mu.Lock()
	due, queued := f.c.q.pending[depKey(dep.ID)]
	f.c.q.mu.Unlock()
	if !queued || time.Until(due) <= f.c.q.debounce || time.Until(due) > f.c.retryBackoff {
		t.Fatalf("eviction failure not retried after the backoff: queued=%v in %s", queued, time.Until(due))
	}
	hub.evictErr = nil
	if _, _, err := f.c.ensureModelReady(context.Background(), f.node, dep, f.model, store.SourceRules{SaveTo: dep.SaveToStorageID}); err != nil {
		t.Fatal(err)
	}
	if got := f.c.CachePhase(ref); got != "" {
		t.Fatalf("cache phase after a successful eviction = %q", got)
	}
}
