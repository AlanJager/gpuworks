package controller

import (
	"errors"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestCacheChangedDirectoryStillTrimsCurrentFiles(t *testing.T) {
	f := newStorageControllerFixture(t)
	prepareCache(t, f, 100)
	old := cacheFile(t, f, "old-directory", 60, true)
	nextPath := "/new-cache"
	var err error
	f.cached, err = f.st.UpdateStorage(f.cached.ID, store.StoragePatch{CachePath: &nextPath})
	if err != nil {
		t.Fatal(err)
	}
	f.ready(t, f.node, proto.StorageStatus{ID: f.cached.ID, State: proto.StorageStateReady, CachePath: nextPath, Path: f.cached.Path})
	current := cacheFile(t, f, "new-directory", 120, true)
	runCache(t, f, "trim")
	op, _ := f.st.CacheOperation(f.cached.ID, f.node.ID)
	if _, err := f.st.ModelFileByID(old.ID); err != nil {
		t.Fatal("historical file was removed", err)
	}
	if _, err := f.st.ModelFileByID(current.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("current directory remains over cap; current file still exists, op=%+v calls=%v", op, f.hub.calls)
	}
	if len(f.hub.calls) != 1 || !strings.Contains(f.hub.calls[0], current.Path) {
		t.Fatalf("cleanup touched a historical directory: %v", f.hub.calls)
	}
}
