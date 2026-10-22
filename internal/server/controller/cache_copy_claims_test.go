package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestCacheCopySharedClaimsStopOnlyTheLastUser(t *testing.T) {
	f := newStorageControllerFixture(t)
	first, second := f.deployment(t, f.cached), f.deployment(t, f.cached)
	ref := store.FileRef{ModelID: f.model.ID, NodeID: f.node.ID, StorageID: f.cached.ID}
	file, err := f.st.PrepareModelCopy(ref, "model")
	if err != nil {
		t.Fatal(err)
	}
	f.c.cacheCopies.claim(first.ID, ref)
	f.c.cacheCopies.claim(second.ID, ref)
	if err := f.st.SetDeploymentState(first.ID, store.DeployStopped); err != nil {
		t.Fatal(err)
	}
	if err := f.c.withdrawCacheCopies(context.Background(), first.ID); err != nil {
		t.Fatal(err)
	}
	current, _ := f.st.ModelFileByID(file.ID)
	if current.State != proto.MFPending || current.TransferID != file.TransferID {
		t.Fatal("stopping one user canceled a shared copy")
	}
	if err := f.st.SetDeploymentState(second.ID, store.DeployStopped); err != nil {
		t.Fatal(err)
	}
	if err := f.c.withdrawCacheCopies(context.Background(), second.ID); err != nil {
		t.Fatal(err)
	}
	current, _ = f.st.ModelFileByID(file.ID)
	if current.State != proto.MFAbsent || current.TransferID == file.TransferID {
		t.Fatalf("last reference left live intent: %+v", current)
	}
	if err := f.st.UpdateModelFileStatus(f.node.ID, proto.ModelFileStatus{Key: store.ModelFileKey(file.ID), TransferID: file.TransferID, State: proto.MFPresent}); err != nil {
		t.Fatal(err)
	}
	current, _ = f.st.ModelFileByID(file.ID)
	if current.State != proto.MFAbsent {
		t.Fatal("late canceled copy became ready")
	}
}

func TestCacheCopyClaimsReconstructedAfterRestart(t *testing.T) {
	f := newStorageControllerFixture(t)
	dep := f.deployment(t, f.cached)
	ref := store.FileRef{ModelID: f.model.ID, NodeID: f.node.ID, StorageID: f.cached.ID}
	file, err := f.st.PrepareModelCopy(ref, "model")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.c.seedCacheCopyClaims([]store.Deployment{*dep}); err != nil {
		t.Fatal(err)
	}
	if err := f.st.SetDeploymentState(dep.ID, store.DeployStopped); err != nil {
		t.Fatal(err)
	}
	if err := f.c.withdrawCacheCopies(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	current, _ := f.st.ModelFileByID(file.ID)
	if current.State != proto.MFAbsent {
		t.Fatal("restart lost cancellation ownership")
	}
}

func TestCacheCopyRestartAfterWithdrawalUsesNewIntent(t *testing.T) {
	f := newStorageControllerFixture(t)
	dep := f.deployment(t, f.cached)
	f.ready(t, f.node, proto.StorageStatus{ID: f.cached.ID, State: proto.StorageStateReady, Path: "/archive", CachePath: "/cache", Writable: true})
	if _, err := f.st.UpsertModelFile(&store.ModelFile{ModelID: f.model.ID, StorageID: f.cached.ID, State: proto.MFPresent, Path: "model", SizeBytes: 100}); err != nil {
		t.Fatal(err)
	}
	ref := store.FileRef{ModelID: f.model.ID, NodeID: f.node.ID, StorageID: f.cached.ID}
	before, err := f.st.PrepareModelCopy(ref, "model")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.SuspendCacheCopy(before.ID, before.TransferID); err != nil {
		t.Fatal(err)
	}
	suspended, _ := f.st.ModelFileByID(before.ID)
	task, ok := f.st.ModelFileTask(suspended, f.model)
	if !ok || task.DesiredState != "absent" {
		t.Fatalf("withdrawn task=%+v %v", task, ok)
	}
	source, err := f.st.ModelSourceOn(f.model.ID, f.node, store.SourceRules{SaveTo: f.cached.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.c.fillNodeCache(context.Background(), f.node, dep, f.cached, f.model, source); err != nil {
		t.Fatal(err)
	}
	after, _ := f.st.ModelFileByID(before.ID)
	if after.State != proto.MFPending || after.TransferID == before.TransferID || after.TransferID == suspended.TransferID {
		t.Fatalf("reused canceled intent: %+v", after)
	}
	// A later cap reduction must also apply when resuming retained partials.
	if err := f.st.SuspendCacheCopy(after.ID, after.TransferID); err != nil {
		t.Fatal(err)
	}
	f.cached.CacheMaxBytes = 1
	source, err = f.st.ModelSourceOn(f.model.ID, f.node, store.SourceRules{SaveTo: f.cached.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.c.fillNodeCache(context.Background(), f.node, dep, f.cached, f.model, source); err != nil {
		t.Fatal(err)
	}
	blocked, err := f.st.ModelFileByID(after.ID)
	if err != nil || blocked.State != proto.MFAbsent {
		t.Fatalf("resume bypassed capacity: %+v %v", blocked, err)
	}
}

func TestCacheCopyStartupRetriesReadFailure(t *testing.T) {
	f := newStorageControllerFixture(t)
	dep := f.deployment(t, f.cached)
	ref := store.FileRef{ModelID: f.model.ID, NodeID: f.node.ID, StorageID: f.cached.ID}
	if _, err := f.st.PrepareModelCopy(ref, "model"); err != nil {
		t.Fatal(err)
	}
	queries := 0
	if err := f.st.DB().Callback().Query().Before("gorm:query").Register("cache_startup_failure", func(db *gorm.DB) {
		if db.Statement.Table == "model_files" {
			queries++
			if queries == 1 {
				db.AddError(errors.New("temporary inventory failure"))
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	defer f.st.DB().Callback().Query().Remove("cache_startup_failure")
	f.c.retryBackoff = time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if !f.c.primeCacheCopyClaims(ctx) || queries < 2 {
		t.Fatal("startup did not retry")
	}
	if refs := f.c.cacheCopies.release(dep.ID); len(refs) != 1 || refs[0] != ref {
		t.Fatalf("lost claim: %v", refs)
	}
	cancel()
	if f.c.primeCacheCopyClaims(ctx) {
		t.Fatal("canceled startup continued")
	}
}

func TestCacheCopyFailedIntentStaysWithdrawnAcrossReplay(t *testing.T) {
	for _, state := range []string{proto.MFFailed, proto.MFInsufficientDisk} {
		t.Run(state, func(t *testing.T) {
			f := newStorageControllerFixture(t)
			file, err := f.st.PrepareModelCopy(store.FileRef{ModelID: f.model.ID, NodeID: f.node.ID, StorageID: f.cached.ID}, "model")
			if err != nil {
				t.Fatal(err)
			}
			if err := f.st.SetModelFileState(file.ID, state, "copy failed"); err != nil {
				t.Fatal(err)
			}
			file, err = f.st.ModelFileByID(file.ID)
			if err != nil {
				t.Fatal(err)
			}
			task, ok := f.st.ModelFileTask(file, f.model)
			if !ok || task.DesiredState != "absent" || task.TransferID != file.TransferID {
				t.Fatalf("replayed failed copy: %+v %v", task, ok)
			}
			if err := f.st.SuspendCacheCopy(file.ID, file.TransferID); err != nil {
				t.Fatal(err)
			}
			current, err := f.st.ModelFileByID(file.ID)
			if err != nil || current.State != proto.MFAbsent || current.TransferID == file.TransferID {
				t.Fatalf("stop revived failed copy: %+v %v", current, err)
			}
		})
	}
}

func TestCacheCopyRestartClaimsNarrowToActualPlacement(t *testing.T) {
	f := newStorageControllerFixture(t)
	first, second := f.deployment(t, f.cached), f.deployment(t, f.cached)
	ref := store.FileRef{ModelID: f.model.ID, NodeID: f.node.ID, StorageID: f.cached.ID}
	file, err := f.st.PrepareModelCopy(ref, "model")
	if err != nil {
		t.Fatal(err)
	}
	// Unknown placement at restart retains this copy for both deployments.
	first.NodeIDs, second.NodeIDs = "[]", "[]"
	first.NodeID, second.NodeID = 0, 0
	if err := f.st.DB().Model(&store.Deployment{}).Where("id IN ?", []uint{first.ID, second.ID}).Updates(map[string]any{"node_id": 0, "node_ids": "[]"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.c.seedCacheCopyClaims([]store.Deployment{*first, *second}); err != nil {
		t.Fatal(err)
	}
	if len(f.c.cacheCopies.byFile[ref]) != 2 {
		t.Fatalf("restart did not retain both unresolved placements: %v", f.c.cacheCopies.byFile)
	}
	// The first deployment resolves elsewhere; the second still needs this node.
	if err := f.c.retainCacheCopyNodes(context.Background(), first.ID, []uint{f.node.ID + 100}); err != nil {
		t.Fatal(err)
	}
	current, _ := f.st.ModelFileByID(file.ID)
	if current.State != proto.MFPending {
		t.Fatal("placement resolution canceled another deployment's copy")
	}
	if err := f.st.SetDeploymentState(second.ID, store.DeployStopped); err != nil {
		t.Fatal(err)
	}
	if err := f.c.withdrawCacheCopies(context.Background(), second.ID); err != nil {
		t.Fatal(err)
	}
	current, _ = f.st.ModelFileByID(file.ID)
	if current.State != proto.MFAbsent {
		t.Fatalf("obsolete restart claim prevented last-user cancellation: file=%+v protection=%v claims=%v", current, f.st.CheckModelFileRowEviction(file.ID), f.c.cacheCopies.byDeployment)
	}
}

func TestCacheCopyPlacementRetainsUnvisitedDependenciesAndRetriesWithdrawal(t *testing.T) {
	var claims cacheCopyClaims
	primary := store.FileRef{NodeID: 2, ModelID: 3, StorageID: 4}
	component := store.FileRef{NodeID: 2, ModelID: 5, StorageID: 4}
	old := store.FileRef{NodeID: 6, ModelID: 3, StorageID: 4}
	for _, ref := range []store.FileRef{primary, component, old} {
		claims.claim(1, ref)
	}
	unused := claims.releaseOutsideNodes(1, []uint{2})
	if len(unused) != 1 || unused[0] != old {
		t.Fatalf("wrong obsolete references: %v", unused)
	}
	if retained := claims.release(1); len(retained) != 2 {
		t.Fatalf("unvisited component was lost: %v", retained)
	}

	f := newStorageControllerFixture(t)
	dep := f.deployment(t, f.cached)
	ref := store.FileRef{NodeID: f.node.ID, ModelID: f.model.ID, StorageID: f.cached.ID}
	file, err := f.st.PrepareModelCopy(ref, "model")
	if err != nil {
		t.Fatal(err)
	}
	f.c.cacheCopies.claim(dep.ID, ref)
	if err := f.st.SetDeploymentState(dep.ID, store.DeployStopped); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.c.retainCacheCopyNodes(ctx, dep.ID, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error lost: %v", err)
	}
	if err := f.c.retainCacheCopyNodes(context.Background(), dep.ID, nil); err != nil {
		t.Fatal(err)
	}
	current, _ := f.st.ModelFileByID(file.ID)
	if current.State != proto.MFAbsent {
		t.Fatal("failed withdrawal lost its retry claim")
	}
}
