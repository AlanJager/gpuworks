package controller

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestDisabledCacheBackgroundCleanup(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		name := "disabled"
		if enabled {
			name = "enabled_control"
		}
		t.Run(name, func(t *testing.T) {
			f := newStorageControllerFixture(t)
			prepareCache(t, f, 1000)
			file := cacheFile(t, f, "safe", 60, true)
			if _, err := f.st.UpdateStorage(f.cached.ID, store.StoragePatch{Enabled: &enabled}); err != nil {
				t.Fatal(err)
			}
			runCache(t, f, "clear")
			op, err := f.st.CacheOperation(f.cached.ID, f.node.ID)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("operation=%s error=%q", op.State, op.Error)
			if op.State != "completed" {
				t.Errorf("ready unused local cache should clear: %s %s", op.State, op.Error)
			}
			if _, err := f.st.ModelFileByID(file.ID); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("cache index remains: %v", err)
			}
		})
	}
}

func TestDisabledCachePolicyChangeQueuesTrim(t *testing.T) {
	f := newStorageControllerFixture(t)
	prepareCache(t, f, 100)
	cacheFile(t, f, "safe", 60, true)
	enabled := false
	cap := int64(10)
	if _, err := f.st.UpdateStorage(f.cached.ID, store.StoragePatch{Enabled: &enabled, CacheMaxBytes: &cap}); err != nil {
		t.Fatal(err)
	}
	if err := f.c.cacheWork(context.Background(), fmt.Sprintf("config/%d", f.cached.ID)); err != nil {
		t.Fatal(err)
	}
	op, err := f.st.CacheOperation(f.cached.ID, f.node.ID)
	if err != nil {
		t.Fatalf("policy change did not queue trim: %v", err)
	}
	if op.State != "pending" {
		t.Errorf("expected pending trim, got %+v", op)
	}
}
