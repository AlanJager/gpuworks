package controller

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type cacheEvictionContextHub struct {
	*storageHub
	call func(context.Context) (json.RawMessage, error)
}

func (h *cacheEvictionContextHub) CallNode(ctx context.Context, _ string, _ string, _ any) (json.RawMessage, error) {
	return h.call(ctx)
}

func TestCacheInlineEvictionBoundsRPCAndPreservesUnconfirmedRows(t *testing.T) {
	for _, outcome := range []string{"rpc_error", "timeout", "unconfirmed", "wrong_path", "malformed"} {
		t.Run(outcome, func(t *testing.T) {
			f := newStorageControllerFixture(t)
			prepareCache(t, f, 20)
			file := cacheFile(t, f, "candidate", 60, true)
			ctx := t.Context()
			if outcome == "timeout" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 50*time.Millisecond)
				defer cancel()
			}
			called := false
			failure := errors.New("remote removal failed")
			f.c.hub = &cacheEvictionContextHub{storageHub: f.hub, call: func(rpcCtx context.Context) (json.RawMessage, error) {
				called = true
				deadline, ok := rpcCtx.Deadline()
				if !ok || time.Until(deadline) > 30*time.Second {
					t.Error("inline eviction RPC is not bounded to 30 seconds")
				}
				if parent, ok := ctx.Deadline(); ok && deadline.After(parent) {
					t.Error("RPC extended caller deadline")
				}
				switch outcome {
				case "timeout":
					<-rpcCtx.Done()
					return nil, rpcCtx.Err()
				case "unconfirmed":
					return json.RawMessage(`{"removed":false}`), nil
				case "wrong_path":
					return json.RawMessage(`{"removed":true,"path":"/other-cache/model"}`), nil
				case "malformed":
					return json.RawMessage(`{`), nil
				default:
					return nil, failure
				}
			}}
			err := f.c.evictCacheRow(ctx, f.node, file.ID)
			if !called || err == nil {
				t.Fatalf("unconfirmed eviction accepted: called=%v err=%v", called, err)
			}
			if outcome == "timeout" && !errors.Is(err, context.DeadlineExceeded) || outcome == "rpc_error" && !errors.Is(err, failure) {
				t.Fatalf("RPC failure was not propagated: %v", err)
			}
			if _, err := f.st.ModelFileByID(file.ID); err != nil {
				t.Fatal("unconfirmed removal deleted the cache index", err)
			}
			if release, ok := f.st.TryPinNodeCaches([]uint{f.node.ID}); !ok {
				t.Fatal("failed eviction leaked its reservation")
			} else {
				release()
			}
		})
	}
}

func TestCacheResumeChargesOnlyReservationsInCurrentDirectory(t *testing.T) {
	for _, tc := range []struct {
		name    string
		changed bool
		cap     int64
		allowed bool
	}{
		{"same_directory_no_double_charge", false, 100, true},
		{"new_directory_rechecks_full_size", true, 50, false},
		{"new_directory_has_room", true, 100, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newStorageControllerFixture(t)
			prepareCache(t, f, tc.cap)
			dep := f.deployment(t, f.cached)
			if _, err := f.st.UpsertModelFile(&store.ModelFile{ModelID: f.model.ID, StorageID: f.cached.ID, State: proto.MFPresent, Path: "model", SizeBytes: 100}); err != nil {
				t.Fatal(err)
			}
			file, err := f.st.UpsertModelFile(&store.ModelFile{ModelID: f.model.ID, NodeID: f.node.ID, StorageID: f.cached.ID, State: proto.MFDownloading, Acquisition: proto.ModelAcquireCopy, Path: f.cached.CachePath + "/model", SizeBytes: 100, Downloaded: 50, TransferID: "before-withdrawal"})
			if err != nil {
				t.Fatal(err)
			}
			if err := f.st.SuspendCacheCopy(file.ID, file.TransferID); err != nil {
				t.Fatal(err)
			}
			if tc.changed {
				next := "/new-cache"
				f.cached, err = f.st.UpdateStorage(f.cached.ID, store.StoragePatch{CachePath: &next})
				if err != nil {
					t.Fatal(err)
				}
				f.ready(t, f.node, proto.StorageStatus{ID: f.cached.ID, State: proto.StorageStateReady, Path: f.cached.Path, CachePath: next})
			}
			src, err := f.st.ModelSourceOn(f.model.ID, f.node, store.SourceRules{SaveTo: f.cached.ID})
			if err != nil || src.Action != store.SourceFillCache {
				t.Fatalf("source=%+v err=%v", src, err)
			}
			if err := f.c.fillNodeCache(t.Context(), f.node, dep, f.cached, f.model, src); err != nil {
				t.Fatal(err)
			}
			after, err := f.st.ModelFileByID(file.ID)
			if err != nil {
				t.Fatal(err)
			}
			want := proto.MFAbsent
			if tc.allowed {
				want = proto.MFPending
			}
			if after.State != want {
				t.Fatalf("resume capacity check: state=%s want=%s", after.State, want)
			}
		})
	}
}
